// Package holds implementa store.SeatGate com Redis: um filtro rápido que recusa,
// sem tocar no Postgres, reservas de assentos que outro cliente já está segurando.
//
// O Redis NÃO é a fonte da verdade (ver ADR-010). Quem passa pelo filtro ainda
// disputa o assento no Postgres; se o Redis cair, o store segue só com o banco.
package holds

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"

	"ticketmaster-lite/backend/internal/store"
)

// acquireScript segura todas as chaves ou nenhuma (atômico: scripts Lua rodam
// sem intercalação no Redis). Retorna os índices (1-based) das chaves ocupadas.
var acquireScript = redis.NewScript(`
local busy = {}
for i, k in ipairs(KEYS) do
  if redis.call('EXISTS', k) == 1 then busy[#busy + 1] = i end
end
if #busy > 0 then return busy end
for _, k in ipairs(KEYS) do
  redis.call('SET', k, ARGV[1], 'PX', ARGV[2])
end
return busy
`)

// releaseScript apaga só as chaves que ainda pertencem a este token
// (compare-and-delete), para não liberar o hold de outro cliente caso o
// nosso já tenha expirado e alguém tenha pego o assento.
var releaseScript = redis.NewScript(`
local n = 0
for _, k in ipairs(KEYS) do
  if redis.call('GET', k) == ARGV[1] then n = n + redis.call('DEL', k) end
end
return n
`)

const (
	// opTimeout limita cada chamada ao Redis. É imposto pelo próprio Gate (select
	// abaixo), não só pelo context do cliente: com o container do Redis parado, a
	// resolução DNS falha devagar e serializa as conexões — sem esse prazo duro,
	// 50 reservas simultâneas ficavam até 25 s na fila antes do fallback.
	opTimeout = 300 * time.Millisecond
	// breakerCooldown: depois de uma falha, o Gate fica "aberto" (pula o Redis)
	// por esse tempo e as reservas vão direto ao Postgres. Depois, tenta de novo.
	breakerCooldown = 2 * time.Second
)

// Gate é o store.SeatGate baseado em Redis.
type Gate struct {
	rdb       *redis.Client
	openUntil atomic.Int64 // circuit breaker: UnixNano até quando pular o Redis
}

// New cria o cliente Redis (ex.: "redis://redis:6379/0"). Timeouts curtos: se o
// Redis estiver lento, é melhor desistir do filtro e ir direto ao Postgres.
func New(url string) (*Gate, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("REDIS_URL inválida: %w", err)
	}
	opt.DialTimeout = 500 * time.Millisecond
	opt.ReadTimeout = 200 * time.Millisecond
	opt.WriteTimeout = 200 * time.Millisecond
	opt.MaxRetries = 1
	rdb := redis.NewClient(opt)
	// Observabilidade: span por comando (EVALSHA do script Lua aparece no trace da
	// reserva) e métricas do pool de conexões/latência. No-op se o OTel estiver desligado.
	if err := redisotel.InstrumentTracing(rdb); err != nil {
		return nil, err
	}
	if err := redisotel.InstrumentMetrics(rdb); err != nil {
		return nil, err
	}
	return &Gate{rdb: rdb}, nil
}

// Ping verifica a conexão. Falhar aqui não impede o uso do Gate: o cliente
// reconecta sozinho e, enquanto isso, o store degrada para só-Postgres.
func (g *Gate) Ping(ctx context.Context) error { return g.rdb.Ping(ctx).Err() }

func (g *Gate) Close() error { return g.rdb.Close() }

// key: o hash tag {e<id>} põe todos os assentos de um evento no mesmo slot,
// requisito para o script multi-chave funcionar num Redis Cluster.
func key(eventID, ticketID int64) string {
	return fmt.Sprintf("hold:{e%d}:%d", eventID, ticketID)
}

func (g *Gate) Acquire(ctx context.Context, eventID int64, ticketIDs []int64, ttl time.Duration) (*store.GateHold, error) {
	if time.Now().UnixNano() < g.openUntil.Load() {
		return nil, store.ErrGateUnavailable
	}
	keys := make([]string, len(ticketIDs))
	for i, id := range ticketIDs {
		keys[i] = key(eventID, id)
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}

	busy, err := withDeadline(ctx, func(ctx context.Context) ([]int64, error) {
		return acquireScript.Run(ctx, g.rdb, keys, token, ttl.Milliseconds()).Int64Slice()
	})
	if err != nil {
		g.openUntil.Store(time.Now().Add(breakerCooldown).UnixNano())
		// O script pode ter rodado no Redis e só a resposta se perdido. Nesse caso
		// as chaves ficariam com o nosso token até o TTL, bloqueando o assento à
		// toa: tentamos apagá-las em segundo plano (melhor esforço).
		go func() { _ = g.release(context.Background(), keys, token) }()
		return nil, err
	}
	if len(busy) > 0 {
		conflicts := make([]int64, len(busy))
		for i, idx := range busy {
			conflicts[i] = ticketIDs[idx-1]
		}
		return &store.GateHold{Conflicts: conflicts}, nil
	}
	return &store.GateHold{
		Release: func(ctx context.Context) error { return g.release(ctx, keys, token) },
	}, nil
}

func (g *Gate) release(ctx context.Context, keys []string, token string) error {
	_, err := withDeadline(ctx, func(ctx context.Context) (int64, error) {
		return releaseScript.Run(ctx, g.rdb, keys, token).Int64()
	})
	return err
}

// withDeadline roda fn com prazo de opTimeout e retorna no prazo mesmo que fn
// ignore o context (fn continua em background até terminar e seu resultado é
// descartado; o canal tem buffer para a goroutine não vazar).
func withDeadline[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer cancel()
		v, err := fn(ctx)
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
