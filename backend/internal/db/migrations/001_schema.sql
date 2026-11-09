-- Schema inicial: Venue / Event / Booking / Ticket
CREATE EXTENSION IF NOT EXISTS unaccent;

CREATE TABLE venues (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    city       TEXT NOT NULL,
    address    TEXT NOT NULL,
    seat_rows  INT  NOT NULL DEFAULT 3 CHECK (seat_rows BETWEEN 1 AND 26),
    seat_cols  INT  NOT NULL DEFAULT 3 CHECK (seat_cols > 0)
);

CREATE TABLE events (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    venue_id    BIGINT NOT NULL REFERENCES venues(id),
    starts_at   TIMESTAMPTZ NOT NULL,
    price_cents INT NOT NULL CHECK (price_cents >= 0)
);
CREATE INDEX idx_events_starts_at ON events (starts_at);

-- Booking = pedido do cliente. Nasce PENDING (assentos segurados por um TTL)
-- e vira CONFIRMED após o "pagamento" (POST /api/booking).
CREATE TABLE bookings (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id       BIGINT NOT NULL REFERENCES events(id),
    customer_name  TEXT NOT NULL,
    customer_email TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('PENDING', 'CONFIRMED', 'EXPIRED')),
    seat_count     INT  NOT NULL CHECK (seat_count > 0),
    total_cents    INT  NOT NULL,
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at   TIMESTAMPTZ
);

-- Ticket = um assento de um evento específico. Existe uma linha por assento
-- (criada junto com o evento). UNIQUE(event_id, seat_row, seat_number)
-- garante que o assento é único; o controle de "quem comprou" é feito por
-- status + booking_id com lock de linha (ver docs/architecture.md).
CREATE TABLE tickets (
    id             BIGSERIAL PRIMARY KEY,
    event_id       BIGINT NOT NULL REFERENCES events(id),
    seat_row       TEXT   NOT NULL,
    seat_number    INT    NOT NULL,
    status         TEXT   NOT NULL DEFAULT 'AVAILABLE' CHECK (status IN ('AVAILABLE', 'RESERVED', 'SOLD')),
    booking_id     UUID REFERENCES bookings(id),
    reserved_until TIMESTAMPTZ,
    UNIQUE (event_id, seat_row, seat_number)
);
CREATE INDEX idx_tickets_booking_id ON tickets (booking_id);
