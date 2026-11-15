// Package live empurra o mapa de assentos em tempo real para os navegadores (SSE).
//
// Fluxo (ADR-012):
//
//	UPDATE tickets ─► trigger pg_notify('seat_changes', event_id)   (entregue no COMMIT)
//	    └─► Hub.listen (conexão pgx dedicada com LISTEN) ─► markDirty(event_id)
//	          └─► Hub.worker: 1 query de snapshot por evento ─► fan-out para os assinantes
//
// O Postgres continua sendo a fonte da verdade: a notificação só diz "o evento X
// mudou"; o hub relê o mapa e manda o snapshot completo (9 assentos), então uma
// notificação perdida ou duplicada nunca deixa o cliente num estado errado.
package live

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"ticketmaster-lite/backend/internal/store"
)

// Channel é o canal do LISTEN/NOTIFY (ver migration 002_seat_notify.sql).
const Channel = "seat_changes"

// expiryMargin: o timer de expiração dispara um pouco depois do reserved_until,
// para a query ver o hold já vencido mesmo com pequena diferença de relógio
// entre o processo Go e o Postgres.
const expiryMargin = 500 * time.Millisecond

var tracer = otel.Tracer("ticketmaster-lite/live")

// Snapshotter é a parte do store que o hub usa (facilita testar sem banco).
type Snapshotter interface {
	SeatSnapshot(ctx context.Context, eventID int64) ([]store.Seat, *time.Time, error)
}

// Sub é um assinante (uma conexão SSE). C recebe o mapa mais recente; é fechado
// quando o hub encerra (shutdown do servidor).
type Sub struct {
	EventID int64
	C       chan []store.Seat
}

type Hub struct {
	st          Snapshotter
	databaseURL string
	log         *slog.Logger
	subsGauge   metric.Int64UpDownCounter

	mu     sync.Mutex
	subs   map[int64]map[*Sub]struct{} // event_id -> assinantes
	timers map[int64]*time.Timer       // event_id -> timer do próximo vencimento de hold
	dirty  map[int64]struct{}          // eventos a reler pelo worker
	closed bool
	wake   chan struct{} // buffer 1: "há trabalho no dirty"
}

func NewHub(st Snapshotter, databaseURL string, log *slog.Logger) *Hub {
	g, _ := otel.Meter("ticketmaster-lite/live").Int64UpDownCounter("ticketmaster.sse.subscribers",
		metric.WithDescription("Conexões SSE abertas no mapa de assentos"))
	return &Hub{
		st: st, databaseURL: databaseURL, log: log, subsGauge: g,
		subs:   map[int64]map[*Sub]struct{}{},
		timers: map[int64]*time.Timer{},
		dirty:  map[int64]struct{}{},
		wake:   make(chan struct{}, 1),
	}
}

// Subscribe registra um assinante do evento e agenda um snapshot inicial — que é
// lido DEPOIS do registro, então nenhuma mudança entre "abrir o stream" e
// "começar a ouvir" se perde.
func (h *Hub) Subscribe(eventID int64) *Sub {
	s := &Sub{EventID: eventID, C: make(chan []store.Seat, 1)}
	h.mu.Lock()
	if h.closed {
		close(s.C)
		h.mu.Unlock()
		return s
	}
	if h.subs[eventID] == nil {
		h.subs[eventID] = map[*Sub]struct{}{}
	}
	h.subs[eventID][s] = struct{}{}
	h.markDirtyLocked(eventID)
	h.mu.Unlock()
	h.subsGauge.Add(context.Background(), 1)
	return s
}

// Unsubscribe remove o assinante (chamado quando o navegador desconecta).
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.subs[s.EventID]
	if _, ok := set[s]; !ok {
		return // já removido pelo Close
	}
	delete(set, s)
	h.subsGauge.Add(context.Background(), -1)
	if len(set) == 0 {
		delete(h.subs, s.EventID)
		if t := h.timers[s.EventID]; t != nil {
			t.Stop()
			delete(h.timers, s.EventID)
		}
	}
}

// Close fecha todos os assinantes. Registrado em http.Server.RegisterOnShutdown:
// sem isso, o Shutdown esperaria até o timeout pelas conexões SSE (que nunca
// terminam sozinhas).
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for _, set := range h.subs {
		for s := range set {
			close(s.C)
			h.subsGauge.Add(context.Background(), -1)
		}
	}
	for _, t := range h.timers {
		t.Stop()
	}
	h.subs, h.timers = map[int64]map[*Sub]struct{}{}, map[int64]*time.Timer{}
}

// Run inicia o worker e o listener; bloqueia até ctx ser cancelado.
func (h *Hub) Run(ctx context.Context) {
	go h.worker(ctx)
	h.listen(ctx)
}

// markDirtyLocked marca o evento para ser relido e acorda o worker. Exige h.mu.
func (h *Hub) markDirtyLocked(eventID int64) {
	h.dirty[eventID] = struct{}{}
	select {
	case h.wake <- struct{}{}:
	default: // worker já tem um aviso pendente
	}
}

func (h *Hub) markDirty(eventID int64) {
	h.mu.Lock()
	h.markDirtyLocked(eventID)
	h.mu.Unlock()
}

// markAllDirty relê todos os eventos com assinantes (após reconectar o LISTEN,
// notificações enviadas durante a queda foram perdidas).
func (h *Hub) markAllDirty() {
	h.mu.Lock()
	for id := range h.subs {
		h.markDirtyLocked(id)
	}
	h.mu.Unlock()
}

// worker processa os eventos marcados, um de cada vez. Ser um só goroutine:
//   - serializa os snapshots de um evento (um resultado antigo nunca chega
//     depois de um novo);
//   - coalesce rajadas: 50 NOTIFYs do mesmo evento antes do worker acordar
//     viram uma única query.
func (h *Hub) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		}
		h.mu.Lock()
		batch := h.dirty
		h.dirty = map[int64]struct{}{}
		h.mu.Unlock()
		for id := range batch {
			h.refresh(ctx, id)
		}
	}
}

// refresh relê o mapa do evento, envia a todos os assinantes e reagenda o timer
// do próximo vencimento de hold.
func (h *Hub) refresh(ctx context.Context, eventID int64) {
	h.mu.Lock()
	n := len(h.subs[eventID])
	h.mu.Unlock()
	if n == 0 {
		return // ninguém olhando este evento: não gasta query
	}

	ctx, span := tracer.Start(ctx, "live.refresh", trace.WithAttributes(
		attribute.Int64("event.id", eventID), attribute.Int("sse.subscribers", n)))
	defer span.End()
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	seats, nextExpiry, err := h.st.SeatSnapshot(qctx, eventID)
	cancel()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		h.log.WarnContext(ctx, "snapshot do mapa de assentos falhou", "event_id", eventID, "erro", err)
		return // a próxima notificação (ou reconexão do cliente) tenta de novo
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[eventID] {
		// "Último vence": se o cliente ainda não consumiu o snapshot anterior,
		// descarta-o. Um cliente lento nunca trava o hub.
		select {
		case <-s.C:
		default:
		}
		s.C <- seats
	}
	if t := h.timers[eventID]; t != nil {
		t.Stop()
		delete(h.timers, eventID)
	}
	if nextExpiry != nil && len(h.subs[eventID]) > 0 {
		d := time.Until(*nextExpiry) + expiryMargin
		h.timers[eventID] = time.AfterFunc(d, func() { h.markDirty(eventID) })
	}
}

// listen mantém uma conexão dedicada com LISTEN (não vem do pool: a conexão fica
// presa esperando notificações) e reconecta com backoff se ela cair.
func (h *Hub) listen(ctx context.Context) {
	backoff := time.Second
	for {
		err := h.listenOnce(ctx, func() { backoff = time.Second })
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("LISTEN do mapa de assentos caiu; reconectando", "erro", err, "em", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (h *Hub) listenOnce(ctx context.Context, connected func()) error {
	conn, err := pgx.Connect(ctx, h.databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	connected()
	h.log.Info("ouvindo mudanças de assentos (LISTEN)", "channel", Channel)
	h.markAllDirty()

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		id, err := strconv.ParseInt(n.Payload, 10, 64)
		if err != nil {
			h.log.Warn("payload de NOTIFY inválido", "payload", n.Payload)
			continue
		}
		h.markDirty(id)
	}
}
