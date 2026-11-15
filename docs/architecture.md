# Arquitetura

## Visão geral

```
 Browser ──► frontend (Vite dev server :5173) ──/api──► backend Go (:8080) ──► Postgres 16 (:5432)
              React SPA                          proxy    net/http + pgx    │    fonte da verdade
                                                                            └──► Redis 7 (:6379)
                                                                                 seat gate (opcional)
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

Implementado em `backend/internal/store/store.go` (`Reserve`, `reserveTx`, `ConfirmBooking`) e
`backend/internal/holds/redis.go`. Duas camadas (ADR-010):

```
POST /api/reserve
  │
  ├─ 1. Seat gate (Redis, opcional) ── assento já seguro por outro? ──► 409 (não toca no Postgres)
  │       SET hold:{e<evento>}:<ticket> <token> NX PX <TTL>  (script Lua, tudo ou nada)
  │       Redis fora/lento (>300 ms) ──► segue só com o Postgres (circuit breaker de 2 s)
  │
  └─ 2. Postgres (fonte da verdade) ── UPDATE condicional + SKIP LOCKED ──► 201 ou 409
          se o Postgres recusar, as chaves do Redis deste cliente são apagadas
```

### Camada 2 — Postgres (sempre roda, decide de verdade)
Nível de isolamento padrão (READ COMMITTED), uma transação:

```sql
BEGIN;
SELECT count(*) FROM tickets WHERE event_id = $1 AND id = ANY($2);   -- pertencem ao evento? (senão 400)
INSERT INTO bookings (...) VALUES (..., 'PENDING', expires_at = now() + TTL) RETURNING id;
UPDATE tickets t SET status='RESERVED', booking_id=b.id, reserved_until=b.expires_at
FROM bookings b
WHERE b.id = $booking
  AND t.id IN (SELECT t.id FROM tickets t
               WHERE event_id = $1 AND id = ANY($2) AND <livre>
               ORDER BY t.id
               FOR UPDATE OF t SKIP LOCKED)    -- pula linhas travadas em vez de esperar
RETURNING t.id;
-- se voltou menos linhas que o pedido -> ROLLBACK + HTTP 409 (tudo ou nada)
COMMIT;
```

- **Fail fast**: antes (ADR-004) o `SELECT ... FOR UPDATE` fazia os perdedores **esperarem** na fila de lock
  só para receber 409 no final, cada um segurando uma conexão do pool. Com `SKIP LOCKED`, linha travada por
  outra transação em andamento é tratada como ocupada na hora.
- Linha livre e não travada é travada e o predicado `<livre>` é **reavaliado na versão mais recente** da linha
  (comportamento do `FOR UPDATE` em READ COMMITTED), então duas transações nunca pegam o mesmo assento.
- Trade-off: se a transação que segura o lock fizer rollback (ex.: o pedido dela tinha outro assento ocupado),
  quem foi recusado recebeu 409 por um assento que acabou ficando livre. Aceitável: o usuário tenta de novo.
- `make loadtest` dispara 50 reservas simultâneas no mesmo assento: resultado esperado 1×201 e 49×409,
  e mostra a latência (p50/p99/máx).

### Camada 1 — Seat gate em Redis (filtro, não garantia)
- Uma chave por assento com o **mesmo TTL do hold**. `acquire` (Lua) só grava se **nenhuma** chave existir e
  devolve quais estavam ocupadas; `release` (Lua) apaga só as chaves com **o token deste cliente**
  (compare-and-delete), para nunca liberar o hold de outra pessoa.
- Hash tag `{e<evento>}` deixa todas as chaves de um evento no mesmo slot (compatível com Redis Cluster).
- **Por que não é a fonte da verdade**: replicação assíncrona/failover podem perder chaves, e Redis + Postgres
  não commitam juntos. Por isso o Postgres sempre revalida; o pior caso de um erro no Redis é uma recusa
  indevida até o TTL (ex.: processo morreu entre o `SET` no Redis e o `ROLLBACK`), nunca venda duplicada.
- **Degradação**: cada chamada tem prazo duro de 300 ms; falhou → circuit breaker abre por 2 s e as reservas
  vão direto ao Postgres. Se a chamada estourou o prazo, o gate tenta apagar em background as chaves que o
  script pode ter gravado. Medido com o container do Redis parado: sem o prazo duro a resolução DNS do host
  `redis` serializava as conexões e 50 reservas levavam até 25 s.
- `REDIS_URL` vazio desliga o gate (útil para comparar com `make loadtest`).

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
- `/admin` — **relatório de ingressos** (link "Relatório" no topo): tabela com todos os assentos, status
  (vendido / reservado / disponível), comprador (nome, e-mail), data da compra ou fim do hold e valor.
  - Filtros de **local** e **intervalo de datas do evento** vão para o servidor (`GET /api/admin/tickets`);
    o filtro de **status** é feito no cliente (chips com contagem + receita dos vendidos).
    Todos ficam na query string (`/admin?venueId=1&startDate=...&status=SOLD`).
  - **Sem polling**: recarrega só ao mudar filtros ou clicar em "Atualizar" (ícone gira durante a carga).
  - Leitura pura, sem lock: usa a mesma regra de expiração preguiçosa (`freeSeatSQL`), então bate com o mapa.

## Configuração (env do backend)
| Variável | Padrão | Uso |
|---|---|---|
| `DATABASE_URL` | `postgres://ticket:ticket@localhost:5432/ticketmaster?sslmode=disable` | conexão |
| `PORT` | `8080` | porta HTTP |
| `RESERVATION_TTL` | `5m` | duração do hold (formato Go: `30s`, `2m`) |
| `SEED_ON_START` | `true` | popula dados mock se `events` vazia |
| `REDIS_URL` | vazio (compose: `redis://redis:6379/0`) | liga o seat gate em Redis; vazio = só Postgres |
