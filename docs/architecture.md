# Arquitetura

## Visão geral

```
 Browser ──► frontend (Vite dev server :5173) ──/api──► backend Go (:8080) ──► Postgres 16 (:5432)
              React SPA                          proxy    net/http + pgx         migrations + seed no start
```

Tudo sobe com `docker compose`. O frontend nunca fala direto com o backend pelo host: o Vite faz proxy de
`/api/*` para `http://backend:8080`, então não há CORS.

## Modelo de dados

```
venues 1───* events 1───* tickets *───0..1 bookings
                    1───* bookings
```

| Tabela | Papel | Campos principais |
|---|---|---|
| `venues` | Local físico | `name`, `city`, `address`, `seat_rows`, `seat_cols` (3x3) |
| `events` | Evento | `slug` (UNIQUE, usado na URL), `name`, `starts_at` (timestamptz), `price_cents`, `venue_id` |
| `tickets` | **Um assento de um evento** (A1..C3). Criado junto com o evento | `seat_row`, `seat_number`, `status` (`AVAILABLE`/`RESERVED`/`SOLD`), `booking_id`, `reserved_until`. `UNIQUE(event_id, seat_row, seat_number)` |
| `bookings` | Pedido do cliente | `status` (`PENDING`/`CONFIRMED`/`EXPIRED`), `seat_count`, `total_cents`, `expires_at` |

Escolha de modelagem: o assento de cada evento é **pré-materializado** como linha em `tickets`. Assim
"comprar um assento" vira "atualizar uma linha existente", e o lock de linha do Postgres resolve a disputa.

## Fluxo de compra (2 fases)

```
[AVAILABLE] --POST /api/reserve--> [RESERVED, reserved_until = now+TTL] --POST /api/booking--> [SOLD]
                                          │
                                          └── TTL vence ──► tratado como AVAILABLE (expiração "preguiçosa")
```

1. **Reserve (hold)**: o usuário escolhe assentos e informa nome/e-mail. O backend segura os assentos por
   `RESERVATION_TTL` (padrão 5 min) e cria um booking `PENDING`.
2. **Booking (confirmação)**: simula o pagamento. Se ainda dentro do prazo, tickets → `SOLD` e booking → `CONFIRMED`.

Separar em duas fases imita sistemas reais (o assento fica "seu" enquanto você paga) e evita que dois
usuários paguem pelo mesmo lugar.

## Concorrência — como o sistema impede venda duplicada

Implementado em `backend/internal/store/store.go` (`Reserve` e `ConfirmBooking`). Estratégia **pessimista**
com lock de linha, nível de isolamento padrão (READ COMMITTED):

```sql
BEGIN;
SELECT t.id, (status='AVAILABLE' OR (status='RESERVED' AND reserved_until < now())) AS free
FROM tickets t
WHERE event_id = $1 AND id = ANY($2)
ORDER BY id          -- ordem fixa => sem deadlock entre pedidos com assentos em comum
FOR UPDATE;          -- trava as linhas
-- se algum !free -> ROLLBACK + HTTP 409
INSERT INTO bookings (...) VALUES (..., 'PENDING', expires_at = now() + TTL);
UPDATE tickets SET status='RESERVED', booking_id=..., reserved_until=... WHERE id = ANY($2);
COMMIT;
```

- Duas transações querendo o mesmo assento: a segunda **bloqueia** no `FOR UPDATE` até a primeira terminar.
  Em READ COMMITTED o Postgres então **re-avalia a linha com a versão já commitada**, vê `RESERVED` e a
  segunda recebe 409.
- Pedido de vários assentos é **tudo ou nada**: se qualquer um estiver ocupado, nada é reservado.
- `make loadtest` dispara 50 reservas simultâneas no mesmo assento: resultado esperado 1×201 e 49×409.

### Expiração
Não há job de limpeza. Um ticket `RESERVED` com `reserved_until < now()` é **considerado livre** em todas as
leituras e na própria reserva (expiração preguiçosa). Na confirmação:
- booking vencido → marcado `EXPIRED`, tickets ainda dele voltam para `AVAILABLE`, HTTP 410;
- se algum ticket foi pego por outra pessoa após o vencimento, a contagem `seat_count` não fecha → 410.

A confirmação usa `clock_timestamp()` (hora real) em vez de `now()` (hora do início da transação) para
não aceitar pagamento milissegundos após o prazo.

## Busca
`GET /api/search` usa `unaccent(...) ILIKE` sobre `nome do evento + nome do local + cidade`
("sao paulo" encontra "São Paulo"). Datas são comparadas no fuso `America/Sao_Paulo` e o intervalo é
inclusivo. Só eventos futuros (`starts_at >= now()`) são listados.

## Frontend
- `/` — lista de eventos + formulário de busca; filtros ficam na query string.
- `/event/:slug` — URL amigável; mostra o mapa 3x3 (livre / selecionado / sua reserva / reservado / vendido),
  faz **polling a cada 5 s** para refletir compras de outros usuários, countdown do hold e botão de confirmar.

## Configuração (env do backend)
| Variável | Padrão | Uso |
|---|---|---|
| `DATABASE_URL` | `postgres://ticket:ticket@localhost:5432/ticketmaster?sslmode=disable` | conexão |
| `PORT` | `8080` | porta HTTP |
| `RESERVATION_TTL` | `5m` | duração do hold (formato Go: `30s`, `2m`) |
| `SEED_ON_START` | `true` | popula dados mock se `events` vazia |
