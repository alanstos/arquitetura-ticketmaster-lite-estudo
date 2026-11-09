// Package store concentra o acesso ao Postgres e as regras de reserva/compra.
package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const MaxSeatsPerBooking = 9

type Store struct {
	pool           *pgxpool.Pool
	reservationTTL time.Duration
}

func New(pool *pgxpool.Pool, reservationTTL time.Duration) *Store {
	return &Store{pool: pool, reservationTTL: reservationTTL}
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

// SearchEvents lista eventos futuros, filtrando por palavra-chave (nome do evento,
// local ou cidade, sem diferenciar acentos/maiúsculas) e intervalo de datas.
func (s *Store) SearchEvents(ctx context.Context, f SearchFilter) ([]EventSummary, error) {
	rows, err := s.pool.Query(ctx, eventSummarySelect+`
WHERE e.starts_at >= now()
  AND ($1::text = '' OR unaccent(e.name || ' ' || v.name || ' ' || v.city) ILIKE '%' || unaccent($1::text) || '%')
  AND ($2::date IS NULL OR (e.starts_at AT TIME ZONE 'America/Sao_Paulo')::date >= $2::date)
  AND ($3::date IS NULL OR (e.starts_at AT TIME ZONE 'America/Sao_Paulo')::date <= $3::date)
ORDER BY e.starts_at`, f.Keyword, f.StartDate, f.EndDate)
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

	rows, err := s.pool.Query(ctx, `
SELECT t.id, t.seat_row, t.seat_number,
       CASE WHEN `+freeSeatSQL+` THEN 'AVAILABLE' ELSE t.status END
FROM tickets t
WHERE t.event_id = $1
ORDER BY t.seat_row, t.seat_number`, ev.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ev.Seats = []Seat{}
	for rows.Next() {
		var st Seat
		if err := rows.Scan(&st.TicketID, &st.Row, &st.Number, &st.Status); err != nil {
			return nil, err
		}
		st.Label = st.Row + strconv.Itoa(st.Number)
		ev.Seats = append(ev.Seats, st)
	}
	return &ev, rows.Err()
}

// Reserve segura (hold) os assentos por reservationTTL e cria um Booking PENDING.
//
// Controle de concorrência (pessimista):
//  1. SELECT ... FOR UPDATE trava as linhas dos tickets pedidos, sempre em ordem
//     de id (evita deadlock entre duas reservas com assentos em comum).
//  2. Quem chegar depois fica bloqueado até o primeiro commit/rollback e então
//     relê o status já atualizado => vê RESERVED e recebe 409.
//  3. Só depois de validar que TODOS estão livres o booking é criado (tudo ou nada).
func (s *Store) Reserve(ctx context.Context, in ReserveInput) (*Booking, error) {
	if len(in.TicketIDs) == 0 || len(in.TicketIDs) > MaxSeatsPerBooking {
		return nil, &ValidationError{Msg: "selecione entre 1 e 9 assentos"}
	}
	seen := map[int64]bool{}
	for _, id := range in.TicketIDs {
		if seen[id] {
			return nil, &ValidationError{Msg: "ticket_ids duplicados"}
		}
		seen[id] = true
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
SELECT t.id, `+freeSeatSQL+` AS free
FROM tickets t
WHERE t.event_id = $1 AND t.id = ANY($2)
ORDER BY t.id
FOR UPDATE`, in.EventID, in.TicketIDs)
	if err != nil {
		return nil, err
	}
	var found int
	var unavailable []int64
	for rows.Next() {
		var id int64
		var free bool
		if err := rows.Scan(&id, &free); err != nil {
			rows.Close()
			return nil, err
		}
		found++
		if !free {
			unavailable = append(unavailable, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if found != len(in.TicketIDs) {
		return nil, &ValidationError{Msg: "um ou mais ticket_ids não pertencem a este evento"}
	}
	if len(unavailable) > 0 {
		return nil, &SeatUnavailableError{TicketIDs: unavailable}
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

	if _, err := tx.Exec(ctx, `
UPDATE tickets t
SET status = 'RESERVED', booking_id = b.id, reserved_until = b.expires_at
FROM bookings b
WHERE b.id = $1 AND t.id = ANY($2)`, bookingID, in.TicketIDs); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetBooking(ctx, bookingID)
}

// ConfirmBooking simula o pagamento: PENDING (dentro do prazo) -> CONFIRMED e tickets -> SOLD.
// Idempotente: confirmar um booking já confirmado devolve o próprio booking.
func (s *Store) ConfirmBooking(ctx context.Context, bookingID string) (*Booking, error) {
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
       b.total_cents, b.expires_at, b.created_at, b.confirmed_at
FROM bookings b JOIN events e ON e.id = b.event_id
WHERE b.id = $1`, bookingID).Scan(&b.ID, &b.EventID, &b.EventSlug, &b.CustomerName, &b.CustomerEmail,
		&b.Status, &b.TotalCents, &b.ExpiresAt, &b.CreatedAt, &b.ConfirmedAt)
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
