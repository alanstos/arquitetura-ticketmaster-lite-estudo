// Package store concentra o acesso ao Postgres e as regras de reserva/compra.
package store

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const MaxSeatsPerBooking = 9

type Store struct {
	pool           *pgxpool.Pool
	reservationTTL time.Duration
	gate           SeatGate // opcional (nil = só Postgres)
	searcher       Searcher // opcional (nil = busca ILIKE no Postgres)
	log            *slog.Logger
	m              instruments // métricas de negócio (telemetry.go)
}

// SeatGate é um filtro rápido, fora do banco, para disputas por assento
// (implementação em internal/holds, com Redis). Ele só serve para recusar cedo
// quem com certeza vai perder; quem passa ainda é validado pelo Postgres.
type SeatGate interface {
	// Acquire tenta segurar todos os assentos (tudo ou nada) por ttl.
	// Conflicts != vazio => algum assento já estava seguro por outro cliente.
	Acquire(ctx context.Context, eventID int64, ticketIDs []int64, ttl time.Duration) (*GateHold, error)
}

// ErrGateUnavailable: o gate está em pausa (circuit breaker aberto após falha)
// e a reserva deve seguir só com o Postgres.
var ErrGateUnavailable = errors.New("seat gate em pausa (circuit breaker aberto)")

// GateHold é o resultado de SeatGate.Acquire.
type GateHold struct {
	Conflicts []int64                         // assentos já segurados por outro cliente
	Release   func(ctx context.Context) error // desfaz o hold (só as chaves deste cliente)
}

// Searcher é o índice de busca textual (implementação em internal/search, com
// Elasticsearch). Devolve só IDs em ordem de relevância; os dados vêm do Postgres.
type Searcher interface {
	SearchEventIDs(ctx context.Context, f SearchFilter) ([]int64, error)
}

// ErrSearchUnavailable: o índice de busca está em pausa (circuit breaker aberto
// após falha) e a busca deve seguir com o Postgres.
var ErrSearchUnavailable = errors.New("índice de busca em pausa (circuit breaker aberto)")

// Option configura o Store.
type Option func(*Store)

// WithSeatGate liga o filtro de assentos (ex.: Redis) na frente do Postgres.
func WithSeatGate(g SeatGate) Option { return func(s *Store) { s.gate = g } }

// WithSearcher liga o índice de busca (ex.: Elasticsearch) para buscas por palavra-chave.
func WithSearcher(se Searcher) Option { return func(s *Store) { s.searcher = se } }

// WithLogger define o logger usado para avisos (ex.: gate indisponível).
func WithLogger(l *slog.Logger) Option { return func(s *Store) { s.log = l } }

func New(pool *pgxpool.Pool, reservationTTL time.Duration, opts ...Option) *Store {
	s := &Store{pool: pool, reservationTTL: reservationTTL, log: slog.Default(), m: newInstruments()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Expressão SQL do "assento livre": AVAILABLE ou RESERVED com hold vencido.
const freeSeatSQL = `(t.status = 'AVAILABLE' OR (t.status = 'RESERVED' AND t.reserved_until < now()))`

const eventSummarySelect = `
SELECT e.id, e.slug, e.name, e.description, e.starts_at, e.price_cents,
       v.id, v.name, v.city, v.address, v.seat_rows, v.seat_cols,
       (SELECT count(*) FROM tickets t WHERE t.event_id = e.id AND ` + freeSeatSQL + `) AS available,
       (SELECT count(*) FROM tickets t WHERE t.event_id = e.id) AS total
FROM events e
JOIN venues v ON v.id = e.venue_id`

func scanEventSummary(row pgx.Row, e *EventSummary) error {
	return row.Scan(&e.ID, &e.Slug, &e.Name, &e.Description, &e.StartsAt, &e.PriceCents,
		&e.Venue.ID, &e.Venue.Name, &e.Venue.City, &e.Venue.Address, &e.Venue.Rows, &e.Venue.Cols,
		&e.AvailableSeats, &e.TotalSeats)
}

// Filtros comuns às buscas: só eventos futuros e intervalo de datas ($1, $2)
// no fuso de São Paulo. A condição extra usa $3.
const searchWhere = `
WHERE e.starts_at >= now()
  AND ($1::date IS NULL OR (e.starts_at AT TIME ZONE 'America/Sao_Paulo')::date >= $1::date)
  AND ($2::date IS NULL OR (e.starts_at AT TIME ZONE 'America/Sao_Paulo')::date <= $2::date)
  AND `

// SearchEvents lista eventos futuros, filtrando por palavra-chave e intervalo de datas.
//
// Com palavra-chave e índice de busca ligado (ADR-013): o Elasticsearch decide
// QUAIS eventos e em que ORDEM (relevância, tolerante a erro de digitação) e o
// Postgres devolve os dados atuais deles. Se o índice falhar, ou sem palavra-chave,
// a busca é feita só no Postgres (ILIKE sem acento, ordenada por data).
func (s *Store) SearchEvents(ctx context.Context, f SearchFilter) ([]EventSummary, error) {
	start := time.Now()
	backend := searchBackendPostgres
	if f.Keyword != "" && s.searcher != nil {
		events, err := s.searchViaIndex(ctx, f)
		if err == nil {
			s.recordSearch(ctx, searchBackendIndex, start)
			return events, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err() // cliente desistiu: não adianta tentar o fallback
		}
		reason := "error"
		if errors.Is(err, ErrSearchUnavailable) {
			reason = "breaker_open"
		} else {
			s.log.WarnContext(ctx, "índice de busca falhou; usando o Postgres", "erro", err)
		}
		s.countSearchFallback(ctx, reason)
		backend = searchBackendFallback
	}
	events, err := s.queryEvents(ctx, f,
		`($3::text = '' OR unaccent(e.name || ' ' || v.name || ' ' || v.city) ILIKE '%' || unaccent($3::text) || '%')
ORDER BY e.starts_at`, f.Keyword)
	if err == nil {
		s.recordSearch(ctx, backend, start)
	}
	return events, err
}

func (s *Store) searchViaIndex(ctx context.Context, f SearchFilter) ([]EventSummary, error) {
	ids, err := s.searcher.SearchEventIDs(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []EventSummary{}, nil
	}
	// Os filtros de data são reaplicados aqui: o Postgres tem a palavra final
	// (o índice pode estar alguns instantes atrasado em relação ao banco).
	events, err := s.queryEvents(ctx, f, `e.id = ANY($3::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	rank := make(map[int64]int, len(ids))
	for i, id := range ids {
		rank[id] = i
	}
	sort.SliceStable(events, func(i, j int) bool { return rank[events[i].ID] < rank[events[j].ID] })
	return events, nil
}

// queryEvents roda eventSummarySelect + searchWhere + cond (que usa $3 = arg).
func (s *Store) queryEvents(ctx context.Context, f SearchFilter, cond string, arg any) ([]EventSummary, error) {
	rows, err := s.pool.Query(ctx, eventSummarySelect+searchWhere+cond, f.StartDate, f.EndDate, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := []EventSummary{}
	for rows.Next() {
		var e EventSummary
		if err := scanEventSummary(rows, &e); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// GetEvent busca o evento pelo slug (ou id numérico) com o mapa de assentos.
func (s *Store) GetEvent(ctx context.Context, slug string, id int64) (*EventDetail, error) {
	var ev EventDetail
	err := scanEventSummary(s.pool.QueryRow(ctx, eventSummarySelect+`
WHERE e.slug = $1 OR e.id = $2`, slug, id), &ev.EventSummary)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	if ev.Seats, _, err = s.SeatSnapshot(ctx, ev.ID); err != nil {
		return nil, err
	}
	return &ev, nil
}

// SeatSnapshot devolve o mapa de assentos do evento (status efetivo) e o instante
// do próximo vencimento de hold (nil se não há hold ativo).
//
// nextExpiry existe por causa da expiração preguiçosa (ADR-005): quando um hold
// vence, nenhum UPDATE acontece — logo nenhum NOTIFY. Quem empurra o mapa em
// tempo real (internal/live) agenda um timer para esse instante.
func (s *Store) SeatSnapshot(ctx context.Context, eventID int64) (seats []Seat, nextExpiry *time.Time, err error) {
	rows, err := s.pool.Query(ctx, `
SELECT t.id, t.seat_row, t.seat_number,
       CASE WHEN `+freeSeatSQL+` THEN 'AVAILABLE' ELSE t.status END,
       CASE WHEN t.status = 'RESERVED' AND t.reserved_until >= now() THEN t.reserved_until END
FROM tickets t
WHERE t.event_id = $1
ORDER BY t.seat_row, t.seat_number`, eventID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	seats = []Seat{}
	for rows.Next() {
		var st Seat
		var until *time.Time
		if err := rows.Scan(&st.TicketID, &st.Row, &st.Number, &st.Status, &until); err != nil {
			return nil, nil, err
		}
		st.Label = st.Row + strconv.Itoa(st.Number)
		seats = append(seats, st)
		if until != nil && (nextExpiry == nil || until.Before(*nextExpiry)) {
			nextExpiry = until
		}
	}
	return seats, nextExpiry, rows.Err()
}

// Reserve segura (hold) os assentos por reservationTTL e cria um Booking PENDING.
//
// Controle de concorrência em duas camadas (ver ADR-010):
//
//  0. (opcional) SeatGate — filtro rápido em Redis (SET NX PX). Se outro cliente
//     já segura algum assento, devolve 409 sem abrir transação no Postgres.
//     O gate NÃO é a garantia: se ele estiver fora do ar, seguimos só com o banco.
//  1. Postgres (fonte da verdade): UPDATE condicional em um único comando, cujo
//     subselect usa FOR UPDATE SKIP LOCKED. Linhas travadas por outra transação
//     em andamento são puladas em vez de esperadas (fail fast: sem fila de lock),
//     e o predicado freeSeatSQL é reavaliado sobre a versão mais recente da linha.
//  2. Se o UPDATE pegou menos assentos que o pedido => rollback (tudo ou nada) e 409.
//
// Observabilidade: span "store.Reserve" (pai dos spans do Redis e das queries) e
// métrica ticketmaster.reservations{result}. Ver telemetry.go.
func (s *Store) Reserve(ctx context.Context, in ReserveInput) (*Booking, error) {
	ctx, span := tracer.Start(ctx, "store.Reserve", trace.WithAttributes(
		attribute.Int64("event.id", in.EventID),
		attribute.Int("seats.count", len(in.TicketIDs)),
		attribute.Bool("seat_gate.enabled", s.gate != nil),
	))
	b, byGate, err := s.reserve(ctx, in)
	result := reserveResult(err, byGate)
	s.m.reservations.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	endSpan(span, result, err)
	return b, err
}

// reserve devolve byGate=true quando a recusa veio do seat gate (Redis).
func (s *Store) reserve(ctx context.Context, in ReserveInput) (_ *Booking, byGate bool, _ error) {
	if len(in.TicketIDs) == 0 || len(in.TicketIDs) > MaxSeatsPerBooking {
		return nil, false, &ValidationError{Msg: "selecione entre 1 e 9 assentos"}
	}
	seen := map[int64]bool{}
	for _, id := range in.TicketIDs {
		if seen[id] {
			return nil, false, &ValidationError{Msg: "ticket_ids duplicados"}
		}
		seen[id] = true
	}

	if s.gate != nil {
		hold, err := s.gate.Acquire(ctx, in.EventID, in.TicketIDs, s.reservationTTL)
		switch {
		case errors.Is(err, ErrGateUnavailable):
			// Breaker aberto: nem tenta o Redis; vai direto ao Postgres.
			s.countFallback(ctx, "breaker_open")
		case err != nil:
			// Gate falhou: degrada para só-Postgres (a correção não depende dele).
			s.countFallback(ctx, "error")
			s.log.WarnContext(ctx, "seat gate indisponível; seguindo só com o Postgres", "erro", err)
		case len(hold.Conflicts) > 0:
			return nil, true, &SeatUnavailableError{TicketIDs: hold.Conflicts}
		default:
			b, err := s.reserveTx(ctx, in)
			if err != nil {
				// Postgres recusou (ou falhou): libera as chaves para não bloquear
				// o assento à toa até o TTL. Contexto próprio: o da requisição pode
				// já ter sido cancelado.
				relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				defer cancel()
				if rErr := hold.Release(relCtx); rErr != nil {
					s.log.WarnContext(ctx, "falha ao liberar hold no seat gate", "erro", rErr)
				}
			}
			return b, false, err
		}
	}
	b, err := s.reserveTx(ctx, in)
	return b, false, err
}

// reserveTx é a parte autoritativa da reserva, toda no Postgres.
func (s *Store) reserveTx(ctx context.Context, in ReserveInput) (*Booking, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Os tickets pertencem ao evento? (event_id nunca muda: não precisa de lock.)
	var found int
	if err := tx.QueryRow(ctx, `
SELECT count(*) FROM tickets WHERE event_id = $1 AND id = ANY($2)`,
		in.EventID, in.TicketIDs).Scan(&found); err != nil {
		return nil, err
	}
	if found != len(in.TicketIDs) {
		return nil, &ValidationError{Msg: "um ou mais ticket_ids não pertencem a este evento"}
	}

	var bookingID string
	err = tx.QueryRow(ctx, `
INSERT INTO bookings (event_id, customer_name, customer_email, status, seat_count, total_cents, expires_at)
SELECT e.id, $2, $3, 'PENDING', $4, e.price_cents * $4, now() + make_interval(secs => $5)
FROM events e WHERE e.id = $1
RETURNING id::text`, in.EventID, in.CustomerName, in.CustomerEmail, len(in.TicketIDs), s.reservationTTL.Seconds()).Scan(&bookingID)
	if err != nil {
		return nil, err
	}

	// UPDATE condicional: só toca assentos livres e não travados por outra
	// transação. RETURNING diz quais foram efetivamente pegos.
	rows, err := tx.Query(ctx, `
UPDATE tickets t
SET status = 'RESERVED', booking_id = b.id, reserved_until = b.expires_at
FROM bookings b
WHERE b.id = $1
  AND t.id IN (
      SELECT t.id FROM tickets t
      WHERE t.event_id = $2 AND t.id = ANY($3) AND `+freeSeatSQL+`
      ORDER BY t.id
      FOR UPDATE OF t SKIP LOCKED)
RETURNING t.id`, bookingID, in.EventID, in.TicketIDs)
	if err != nil {
		return nil, err
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	if len(got) != len(in.TicketIDs) {
		taken := map[int64]bool{}
		for _, id := range got {
			taken[id] = true
		}
		var unavailable []int64
		for _, id := range in.TicketIDs {
			if !taken[id] {
				unavailable = append(unavailable, id)
			}
		}
		return nil, &SeatUnavailableError{TicketIDs: unavailable} // defer faz o rollback
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetBooking(ctx, bookingID)
}

// ConfirmBooking simula o pagamento: PENDING (dentro do prazo) -> CONFIRMED e tickets -> SOLD.
// Idempotente: confirmar um booking já confirmado devolve o próprio booking.
func (s *Store) ConfirmBooking(ctx context.Context, bookingID string) (*Booking, error) {
	ctx, span := tracer.Start(ctx, "store.ConfirmBooking", trace.WithAttributes(attribute.String("booking.id", bookingID)))
	b, err := s.confirmBooking(ctx, bookingID)
	result := bookingResult(err)
	s.m.bookings.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	endSpan(span, result, err)
	return b, err
}

func (s *Store) confirmBooking(ctx context.Context, bookingID string) (*Booking, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var status string
	var seatCount int
	var stillValid bool
	err = tx.QueryRow(ctx, `
SELECT status, seat_count, expires_at > clock_timestamp()
FROM bookings WHERE id = $1
FOR UPDATE`, bookingID).Scan(&status, &seatCount, &stillValid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	switch {
	case status == BookingConfirmed:
		_ = tx.Rollback(ctx)
		return s.GetBooking(ctx, bookingID)
	case status == BookingExpired:
		return nil, ErrBookingExpired
	case !stillValid:
		if err := expireBooking(ctx, tx, bookingID); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, ErrBookingExpired
	}

	// Só converte tickets que ainda pertencem a este booking. Se algum foi
	// "roubado" após expirar, a contagem não fecha e a confirmação falha.
	tag, err := tx.Exec(ctx, `
UPDATE tickets SET status = 'SOLD', reserved_until = NULL
WHERE booking_id = $1 AND status = 'RESERVED'`, bookingID)
	if err != nil {
		return nil, err
	}
	if int(tag.RowsAffected()) != seatCount {
		_ = tx.Rollback(ctx)
		if err := s.expireStandalone(ctx, bookingID); err != nil {
			return nil, err
		}
		return nil, ErrBookingExpired
	}

	if _, err := tx.Exec(ctx, `
UPDATE bookings SET status = 'CONFIRMED', confirmed_at = now() WHERE id = $1`, bookingID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetBooking(ctx, bookingID)
}

func expireBooking(ctx context.Context, tx pgx.Tx, bookingID string) error {
	if _, err := tx.Exec(ctx, `UPDATE bookings SET status = 'EXPIRED' WHERE id = $1`, bookingID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
UPDATE tickets SET status = 'AVAILABLE', booking_id = NULL, reserved_until = NULL
WHERE booking_id = $1 AND status = 'RESERVED'`, bookingID)
	return err
}

func (s *Store) expireStandalone(ctx context.Context, bookingID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := expireBooking(ctx, tx, bookingID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetBooking retorna o booking com os assentos que ainda pertencem a ele.
func (s *Store) GetBooking(ctx context.Context, bookingID string) (*Booking, error) {
	var b Booking
	err := s.pool.QueryRow(ctx, `
SELECT b.id::text, b.event_id, e.slug, b.customer_name, b.customer_email,
       CASE WHEN b.status = 'PENDING' AND b.expires_at < now() THEN 'EXPIRED' ELSE b.status END,
       b.total_cents, b.expires_at,
       CASE WHEN b.status = 'PENDING'
            THEN GREATEST(0, floor(EXTRACT(EPOCH FROM b.expires_at - clock_timestamp())))::int
            ELSE 0 END,
       b.created_at, b.confirmed_at
FROM bookings b JOIN events e ON e.id = b.event_id
WHERE b.id = $1`, bookingID).Scan(&b.ID, &b.EventID, &b.EventSlug, &b.CustomerName, &b.CustomerEmail,
		&b.Status, &b.TotalCents, &b.ExpiresAt, &b.SecondsRemaining, &b.CreatedAt, &b.ConfirmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
SELECT id, seat_row || seat_number FROM tickets WHERE booking_id = $1 ORDER BY seat_row, seat_number`, bookingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b.Seats = []string{}
	b.TicketIDs = []int64{}
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, err
		}
		b.TicketIDs = append(b.TicketIDs, id)
		b.Seats = append(b.Seats, label)
	}
	return &b, rows.Err()
}

// ListVenues devolve todos os locais (usado no filtro do relatório).
func (s *Store) ListVenues(ctx context.Context) ([]Venue, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, name, city, address, seat_rows, seat_cols FROM venues ORDER BY city, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	venues := []Venue{}
	for rows.Next() {
		var v Venue
		if err := rows.Scan(&v.ID, &v.Name, &v.City, &v.Address, &v.Rows, &v.Cols); err != nil {
			return nil, err
		}
		venues = append(venues, v)
	}
	return venues, rows.Err()
}

// AdminTickets lista todos os assentos (inclusive de eventos passados) com o
// status efetivo e o comprador. Só leitura: não trava linhas nem "limpa"
// holds vencidos — aplica a mesma regra de expiração preguiçosa das outras
// consultas (freeSeatSQL), então o relatório bate com o mapa de assentos.
func (s *Store) AdminTickets(ctx context.Context, f AdminFilter) ([]AdminTicket, error) {
	rows, err := s.pool.Query(ctx, `
SELECT t.id, t.seat_row || t.seat_number,
       CASE WHEN `+freeSeatSQL+` THEN 'AVAILABLE' ELSE t.status END AS eff_status,
       e.price_cents, e.id, e.slug, e.name, e.starts_at,
       v.id, v.name, v.city,
       b.id::text, b.customer_name, b.customer_email, b.created_at, b.confirmed_at, t.reserved_until
FROM tickets t
JOIN events e ON e.id = t.event_id
JOIN venues v ON v.id = e.venue_id
-- O LEFT JOIN só traz o booking se o assento NÃO estiver livre.
LEFT JOIN bookings b ON b.id = t.booking_id AND NOT `+freeSeatSQL+`
WHERE ($1::bigint = 0 OR v.id = $1)
  AND ($2::date IS NULL OR (e.starts_at AT TIME ZONE 'America/Sao_Paulo')::date >= $2::date)
  AND ($3::date IS NULL OR (e.starts_at AT TIME ZONE 'America/Sao_Paulo')::date <= $3::date)
ORDER BY e.starts_at, e.id, t.seat_row, t.seat_number`, f.VenueID, f.StartDate, f.EndDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tickets := []AdminTicket{}
	for rows.Next() {
		var t AdminTicket
		// Colunas do booking são NULL quando o assento está livre.
		var bID, bName, bEmail *string
		var bCreated, bConfirmed, reservedUntil *time.Time
		if err := rows.Scan(&t.TicketID, &t.Seat, &t.Status, &t.PriceCents,
			&t.EventID, &t.EventSlug, &t.EventName, &t.EventStartsAt,
			&t.VenueID, &t.VenueName, &t.VenueCity,
			&bID, &bName, &bEmail, &bCreated, &bConfirmed, &reservedUntil); err != nil {
			return nil, err
		}
		if bID != nil {
			t.Buyer = &AdminBuyer{BookingID: *bID, Name: *bName, Email: *bEmail, BookedAt: *bCreated, ConfirmedAt: bConfirmed}
			if t.Status == SeatReserved {
				t.Buyer.ReservedUntil = reservedUntil
			}
		}
		tickets = append(tickets, t)
	}
	return tickets, rows.Err()
}
