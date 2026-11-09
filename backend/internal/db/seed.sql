-- Dados mock. Executado no start apenas se a tabela events estiver vazia.

INSERT INTO venues (name, city, address, seat_rows, seat_cols) VALUES
    ('Qualistage',            'Rio de Janeiro', 'Av. Ayrton Senna, 3000 - Barra da Tijuca', 3, 3),
    ('Parque Ibirapuera',     'São Paulo',      'Av. Pedro Álvares Cabral - Vila Mariana',  3, 3),
    ('Teatro Renault',        'São Paulo',      'Av. Brigadeiro Luís Antônio, 411 - Bela Vista', 3, 3),
    ('Palácio das Artes',     'Belo Horizonte', 'Av. Afonso Pena, 1537 - Centro',           3, 3),
    ('Teatro Guaíra',         'Curitiba',       'R. Conselheiro Laurindo, 175 - Centro',    3, 3);

INSERT INTO events (slug, name, description, venue_id, starts_at, price_cents) VALUES
    ('jota-quest-rio-de-janeiro', 'Jota Quest', 'Turnê com os grandes sucessos da banda.',
        (SELECT id FROM venues WHERE name = 'Qualistage'), '2026-12-12 21:00:00-03', 18000),
    ('villa-de-natal-sao-paulo-2026', 'Villa de Natal', 'Espetáculo natalino com coral e iluminação especial.',
        (SELECT id FROM venues WHERE name = 'Parque Ibirapuera'), '2026-12-20 19:00:00-03', 9000),
    ('o-fantasma-da-opera-sao-paulo', 'O Fantasma da Ópera', 'Musical clássico em nova montagem.',
        (SELECT id FROM venues WHERE name = 'Teatro Renault'), '2027-01-15 20:00:00-03', 25000),
    ('stand-up-comedy-belo-horizonte', 'Noite do Stand-up', 'Quatro comediantes, uma noite.',
        (SELECT id FROM venues WHERE name = 'Palácio das Artes'), '2027-02-05 21:30:00-03', 6000),
    ('orquestra-sinfonica-curitiba-2027', 'Orquestra Sinfônica', 'Concerto de abertura da temporada 2027.',
        (SELECT id FROM venues WHERE name = 'Teatro Guaíra'), '2027-03-10 20:00:00-03', 12000),
    ('festival-de-jazz-rio-de-janeiro-2027', 'Festival de Jazz', 'Encontro de músicos de jazz nacionais e internacionais.',
        (SELECT id FROM venues WHERE name = 'Qualistage'), '2027-04-18 18:00:00-03', 15000);

-- Gera os assentos (tickets) de cada evento a partir da matriz do venue (A1..C3).
INSERT INTO tickets (event_id, seat_row, seat_number, status)
SELECT e.id, chr(64 + r), c, 'AVAILABLE'
FROM events e
JOIN venues v ON v.id = e.venue_id
CROSS JOIN LATERAL generate_series(1, v.seat_rows) AS r
CROSS JOIN LATERAL generate_series(1, v.seat_cols) AS c;

-- Alguns assentos já vendidos, para visualizar "ocupado" na tela.
WITH b AS (
    INSERT INTO bookings (event_id, customer_name, customer_email, status, seat_count, total_cents, expires_at, confirmed_at)
    SELECT id, 'Cliente Seed', 'seed@example.com', 'CONFIRMED', 2, price_cents * 2, now(), now()
    FROM events WHERE slug = 'jota-quest-rio-de-janeiro'
    RETURNING id, event_id
)
UPDATE tickets t SET status = 'SOLD', booking_id = b.id
FROM b
WHERE t.event_id = b.event_id AND t.seat_row = 'B' AND t.seat_number IN (1, 2);

WITH b AS (
    INSERT INTO bookings (event_id, customer_name, customer_email, status, seat_count, total_cents, expires_at, confirmed_at)
    SELECT id, 'Cliente Seed', 'seed@example.com', 'CONFIRMED', 1, price_cents, now(), now()
    FROM events WHERE slug = 'villa-de-natal-sao-paulo-2026'
    RETURNING id, event_id
)
UPDATE tickets t SET status = 'SOLD', booking_id = b.id
FROM b
WHERE t.event_id = b.event_id AND t.seat_row = 'A' AND t.seat_number = 2;
