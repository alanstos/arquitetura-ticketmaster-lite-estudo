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
	"ticketmaster-lite/backend/internal/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

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

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           api.NewRouter(store.New(pool, ttl), log),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
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
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
