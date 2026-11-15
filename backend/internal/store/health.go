package store

import (
	"context"
	"sync"
	"time"
)

// Estados de saúde (de um componente e do sistema como um todo).
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded" // funciona, mas sem uma dependência opcional (Redis/Elasticsearch)
	HealthDown     = "down"     // não consegue atender (Postgres fora)
	HealthDisabled = "disabled" // dependência desligada por configuração
)

// healthCheckTimeout: uma dependência que demora mais que isso para responder
// um ping é tratada como fora do ar (health check não pode travar).
const healthCheckTimeout = time.Second

type ComponentHealth struct {
	Status    string `json:"status"`
	LatencyMS *int64 `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

type HealthReport struct {
	Status string                     `json:"status"`
	Checks map[string]ComponentHealth `json:"checks"`
}

// pinger é implementado pelo seat gate do Redis (holds.Gate) e pelo search.Client. Interface à parte
// para não obrigar todo SeatGate a saber se pingar.
type pinger interface {
	Ping(ctx context.Context) error
}

// Health verifica as dependências em paralelo (readiness).
//
//   - Postgres é a fonte da verdade: fora do ar => "down".
//   - Redis é opcional (ADR-010): fora do ar => "degraded"; reservas seguem só com o Postgres.
//   - Elasticsearch é opcional (ADR-013): fora do ar => "degraded"; busca cai para o ILIKE do Postgres.
func (s *Store) Health(ctx context.Context) HealthReport {
	var pg, rd, es ComponentHealth
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); pg = check(ctx, s.pool.Ping) }()

	if p, ok := s.gate.(pinger); ok {
		wg.Add(1)
		go func() { defer wg.Done(); rd = check(ctx, p.Ping) }()
	} else {
		rd = ComponentHealth{Status: HealthDisabled}
	}
	if p, ok := s.searcher.(pinger); ok {
		wg.Add(1)
		go func() { defer wg.Done(); es = check(ctx, p.Ping) }()
	} else {
		es = ComponentHealth{Status: HealthDisabled}
	}
	wg.Wait()

	status := HealthOK
	switch {
	case pg.Status != HealthOK:
		status = HealthDown
	case rd.Status == HealthDown, es.Status == HealthDown:
		status = HealthDegraded
	}
	return HealthReport{Status: status, Checks: map[string]ComponentHealth{"postgres": pg, "redis": rd, "elasticsearch": es}}
}

func check(ctx context.Context, ping func(context.Context) error) ComponentHealth {
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	start := time.Now()
	err := ping(ctx)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return ComponentHealth{Status: HealthDown, LatencyMS: &ms, Error: err.Error()}
	}
	return ComponentHealth{Status: HealthOK, LatencyMS: &ms}
}
