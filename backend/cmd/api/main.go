package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ticketmaster-lite/backend/internal/api"
	"ticketmaster-lite/backend/internal/db"
	"ticketmaster-lite/backend/internal/holds"
	"ticketmaster-lite/backend/internal/live"
	"ticketmaster-lite/backend/internal/search"
	"ticketmaster-lite/backend/internal/store"
	"ticketmaster-lite/backend/internal/telemetry"
)

// version aparece como service.version na telemetria (sobrescrevível com -ldflags).
var version = "dev"

func main() {
	// Telemetria primeiro: o logger e a instrumentação do pgx/Redis dependem dos
	// providers globais. OTEL_EXPORTER_OTLP_ENDPOINT vazio => desligada (ADR-011).
	serviceName := env("OTEL_SERVICE_NAME", "ticketmaster-api")
	tel, err := telemetry.Setup(context.Background(), serviceName, version)
	if err != nil {
		slog.Error("telemetria", "erro", err)
		os.Exit(1)
	}
	defer func() {
		// Flush final: sem isso, os últimos spans/métricas/logs se perdem no shutdown.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tel.Shutdown(ctx)
	}()
	log := tel.NewLogger(serviceName)
	slog.SetDefault(log)
	if tel.Enabled {
		log.Info("telemetria ligada (OTLP)", "endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	} else {
		log.Info("telemetria desligada (OTEL_EXPORTER_OTLP_ENDPOINT vazio): só logs no stdout")
	}

	databaseURL := env("DATABASE_URL", "postgres://ticket:ticket@localhost:5432/ticketmaster?sslmode=disable")
	port := env("PORT", "8080")
	ttl, err := time.ParseDuration(env("RESERVATION_TTL", "5m"))
	if err != nil {
		log.Error("RESERVATION_TTL inválido", "erro", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, databaseURL, log)
	if err != nil {
		log.Error("db", "erro", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, log); err != nil {
		log.Error("migrate", "erro", err)
		os.Exit(1)
	}
	if env("SEED_ON_START", "true") == "true" {
		if err := db.Seed(ctx, pool, log); err != nil {
			log.Error("seed", "erro", err)
			os.Exit(1)
		}
	}

	// Redis é opcional: REDIS_URL vazio => reservas decididas só pelo Postgres.
	opts := []store.Option{store.WithLogger(log)}
	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		gate, err := holds.New(redisURL)
		if err != nil {
			log.Error("redis", "erro", err)
			os.Exit(1)
		}
		defer gate.Close()
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := gate.Ping(pingCtx); err != nil {
			log.Warn("redis indisponível na partida; o seat gate será tentado a cada reserva", "erro", err)
		}
		cancel()
		opts = append(opts, store.WithSeatGate(gate))
		log.Info("seat gate (Redis) ligado")
	} else {
		log.Info("seat gate desligado (REDIS_URL vazio): só Postgres")
	}

	// Elasticsearch é opcional (ADR-013): ES_URL vazio => busca ILIKE no Postgres.
	// O indexer cria o índice e consome o search_outbox em segundo plano; se o ES
	// ainda não subiu, ele tenta de novo sozinho e a busca usa o fallback.
	if esURL := os.Getenv("ES_URL"); esURL != "" {
		es := search.New(esURL)
		go search.NewIndexer(es, pool, databaseURL, log).Run(ctx)
		opts = append(opts, store.WithSearcher(es))
		log.Info("busca com Elasticsearch ligada", "url", esURL)
	} else {
		log.Info("busca com Elasticsearch desligada (ES_URL vazio): ILIKE no Postgres")
	}

	st := store.New(pool, ttl, opts...)

	// Mapa de assentos em tempo real (SSE, ADR-012): LISTEN numa conexão dedicada.
	hub := live.NewHub(st, databaseURL, log)
	go hub.Run(ctx)

	// Sem WriteTimeout de propósito: ele cortaria os streams SSE (conexões longas).
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           api.NewRouter(st, hub, log),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Shutdown não cancela o contexto das requisições abertas: fechar o hub
	// encerra os streams SSE para o Shutdown não esperar até o timeout.
	srv.RegisterOnShutdown(hub.Close)

	// Graceful shutdown: ao receber SIGTERM, para de aceitar conexões e espera as
	// requisições em andamento. main só retorna (e faz o flush da telemetria nos
	// defers) depois disso.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("api ouvindo", "port", port, "reservation_ttl", ttl.String())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http", "erro", err)
		os.Exit(1)
	}
	<-drained
	log.Info("api encerrada")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
