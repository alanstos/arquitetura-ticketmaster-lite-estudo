# Arquitetura

## Visão geral

```
 Browser ──► frontend (Vite dev server :5173) ──/api──► backend Go (:8080) ──► Postgres 16 (:5432)
              React SPA                          proxy    net/http + pgx    │    fonte da verdade
                                                                            ├──► Redis 7 (:6379)
                                                                            │    seat gate (opcional)
                                                                            └──► Elasticsearch 8 (:9200)
                                                                                 índice de busca (opcional)
                                                     │
                                                     └── OTLP/HTTP ──► grafana/otel-lgtm (:3000 Grafana, :4318 OTLP)
                                                         traces, métricas, logs   Collector → Tempo / Prometheus / Loki
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

## Busca (ADR-013)
O Postgres continua sendo a fonte da verdade; o Elasticsearch é um **índice derivado e opcional**.

```
escrita:  INSERT/UPDATE/DELETE events, UPDATE venues(name, city)
            └─ trigger (mesma transação) ─► search_outbox(event_id) + pg_notify('search_outbox')
          internal/search.Indexer (goroutine, LISTEN + poll de 5 s)
            └─ lote FOR UPDATE SKIP LOCKED ─► lê estado atual dos eventos ─► POST /_bulk ─► DELETE do outbox

leitura:  GET /api/search?keyword=...
            ├─ Elasticsearch: IDs por relevância (≤ 50)  ── timeout 500 ms / breaker 5 s ──┐
            │    └─ Postgres: eventSummarySelect WHERE e.id = ANY(ids) (+ filtros de data)   │ falhou
            │       e reordena pela ordem do ES                                              ▼
            └─ sem keyword / ES desligado ─────────────────────► Postgres: unaccent(...) ILIKE (ordem por data)
```

- **Outbox transacional**: a mudança só entra na fila se o COMMIT acontecer; a linha só sai depois do `_bulk`
  confirmado (pelo menos uma vez; documento = estado atual, então reprocessar é idempotente). Evento sumiu
  do Postgres → `delete` no índice.
- **Índice** `events_v1` atrás do alias `events` (troca de mapping sem downtime). Na criação do índice (1ª
  subida ou volume do ES apagado), o indexer enfileira todos os eventos. `make es-reindex` faz o mesmo à mão.
- **Analyzers**: `pt_text` (minúsculas + stemmer `light_portuguese` + `asciifolding`) e `all.prefix`
  (edge n-grams 2–20 na indexação). Campos `name`, `description`, `venue_name`, `city` são copiados para
  `all`, então "jazz rio" exige as duas palavras mas em qualquer campo.
- **Query**: `match` em `all` e em `all.prefix` com `operator: and`, `fuzziness: AUTO`, `prefix_length: 1`;
  reforço por `multi_match` em `name^3, venue_name^2, city^2, description`. Fuzzy sobre prefixos faz
  `casa` ≈ `casi`(nha). Filtros de data com `||/d` e `time_zone: America/Sao_Paulo`.
- **Dados frescos**: o ES só decide quais eventos e em que ordem; nome, preço e assentos livres vêm do Postgres
  na mesma requisição. Atraso de indexação típico: < 1 s.
- **Degradação**: ES fora → readiness `degraded`, busca usa `ILIKE`, métrica
  `ticketmaster.search.fallback{reason=error|breaker_open}`. Duração (ms) por backend em `ticketmaster.search.duration`; painéis na linha "Busca" do dashboard.
  A 1ª busca com a JVM do ES "fria" pode estourar os 500 ms e cair no fallback por 5 s.

Sem keyword, a busca continua sendo `unaccent(...) ILIKE` + datas, no fuso `America/Sao_Paulo`, intervalo
inclusivo, só eventos futuros (`starts_at >= now()`).

## Frontend
- `/` — lista de eventos + formulário de busca; filtros ficam na query string.
- `/event/:slug` — URL amigável; mostra o mapa 3x3 (livre / selecionado / sua reserva / reservado / vendido),
  recebe o mapa **em tempo real via SSE** (ver abaixo), countdown do hold e botão de confirmar.
- `/admin` — **relatório de ingressos** (link "Relatório" no topo): tabela com todos os assentos, status
  (vendido / reservado / disponível), comprador (nome, e-mail), data da compra ou fim do hold e valor.
  - Filtros de **local** e **intervalo de datas do evento** vão para o servidor (`GET /api/admin/tickets`);
    o filtro de **status** é feito no cliente (chips com contagem + receita dos vendidos).
    Todos ficam na query string (`/admin?venueId=1&startDate=...&status=SOLD`).
  - **Sem polling**: recarrega só ao mudar filtros ou clicar em "Atualizar" (ícone gira durante a carga).
  - Leitura pura, sem lock: usa a mesma regra de expiração preguiçosa (`freeSeatSQL`), então bate com o mapa.

## Mapa de assentos em tempo real (SSE, ADR-012)

Substitui o polling de 5 s. O navegador abre `GET /api/events/{slug}/stream` (`EventSource`) e o servidor
empurra o mapa completo sempre que ele muda.

```
UPDATE tickets ─► trigger pg_notify('seat_changes', event_id)     (migration 002; entregue só no COMMIT)
                     │
   internal/live.Hub: conexão pgx dedicada com LISTEN ─► marca o evento como "sujo"
                     │
   worker (1 goroutine): 1 query SeatSnapshot por evento sujo ─► fan-out para os assinantes SSE do evento
                     │                                          └─► timer para o próximo reserved_until
   handler SSE: canal com buffer 1 ("último vence") ─► event: seats / data: {...}
```

- **Por que LISTEN/NOTIFY**: o NOTIFY é transacional — rollback (os 49 perdedores do loadtest) não notifica,
  e notificações iguais na mesma transação são colapsadas (reservar 3 assentos = 1 aviso). Funciona com
  várias instâncias do backend (cada uma tem seu LISTEN) e captura qualquer escrita, inclusive SQL manual.
- **A notificação só diz "o evento X mudou"**: o hub relê o mapa no Postgres. Perder ou duplicar um aviso
  nunca deixa o cliente num estado errado; ao reconectar o LISTEN, o hub relê todos os eventos assistidos.
- **Expiração preguiçosa × tempo real**: um hold que vence não gera UPDATE, logo não gera NOTIFY.
  `SeatSnapshot` devolve o próximo `reserved_until` e o hub agenda um `time.AfterFunc` (+500 ms) para reler.
- **Um worker só**: serializa os snapshots (um resultado antigo nunca chega depois de um novo) e coalesce
  rajadas. Eventos sem assinantes não geram query.
- **Shutdown**: `srv.RegisterOnShutdown(hub.Close)` fecha os canais e encerra os streams; sem isso o
  `Shutdown` esperaria o timeout. O servidor não tem `WriteTimeout` (cortaria os streams).
- **Observabilidade**: os streams ficam fora do `otelhttp` (span de minutos distorceria a latência); há o
  gauge `ticketmaster.sse.subscribers` e o span `live.refresh` por snapshot. `statusRecorder` implementa
  `Unwrap` para o `http.ResponseController` achar o `Flush`.
- Limite conhecido: em HTTP/1.1 o navegador abre no máximo 6 conexões por origem — muitas abas do mesmo
  site esgotam esse limite (HTTP/2 resolve).

## Observabilidade (ADR-011)

Instrumentação com **OpenTelemetry** (padrão aberto) e visualização local no container `grafana/otel-lgtm`.
O backend só conhece OTLP: trocar de ferramenta (Datadog, Honeycomb, Grafana Cloud…) é trocar o endpoint.

```
backend Go ──OTLP/HTTP :4318──► OTel Collector ─┬─► Tempo       (traces)   ┐
  otelhttp, otelpgx, redisotel,                 ├─► Prometheus  (métricas) ├─► Grafana :3000
  spans/métricas do store, slog→OTLP            └─► Loki        (logs)     ┘
  └─ stdout: logs JSON com trace_id (docker compose logs)
```

| Pilar | De onde vem | Onde ver |
|---|---|---|
| **Traces** | `otelhttp` (span por requisição, nomeado pela rota, ex.: `POST /api/reserve`) → span `store.Reserve`/`store.ConfirmBooking` → spans do Redis (`evalsha` do script Lua) e do pgx (cada query, `pool.acquire`) | Grafana → Explore → Tempo |
| **Métricas** | HTTP (`http_server_duration_milliseconds` por `http_route`/status), pool do pgx (`pgxpool_*`, `db_client_*`), Redis, e de negócio: `ticketmaster_reservations_total{result}`, `ticketmaster_seat_gate_fallback_total{reason}`, `ticketmaster_bookings_total{result}` | Dashboard **Ticketmaster Lite** |
| **Logs** | `slog` com fan-out: JSON no stdout + bridge `otelslog` → Loki. Logs com `*Context` levam `trace_id`/`span_id` | `make logs` ou Explore → Loki `{service_name="ticketmaster-api"}` |

- **Resultado da reserva** (`result`): `created`, `gate_rejected` (Redis recusou sem tocar no banco),
  `conflict` (Postgres recusou: ocupado/`SKIP LOCKED`), `invalid` (400), `error` (500). No `make loadtest` com
  Redis ligado aparece 1 `created` + 49 `gate_rejected`; com `REDIS_URL=` vazio, 1 `created` + 49 `conflict`.
- **Cardinalidade**: span e label `http_route` usam o padrão do `ServeMux` (`/api/events/{idOrSlug}`), nunca o
  caminho real; requisições sem rota viram `unmatched`. Health checks não geram trace/métrica/log.
- **Erros**: só `result=error` marca o span como erro; 409/410 são respostas de negócio.
- **Shutdown**: SIGTERM → `http.Server.Shutdown` drena as requisições → flush dos exporters (sem isso, os
  últimos spans se perdem).
- **Health**: `GET /api/health/live` (liveness, usado pelo healthcheck do compose) e `GET /api/health/ready`
  (readiness: pinga Postgres e Redis em paralelo, 1 s de timeout cada). Postgres fora → 503 `down`;
  Redis fora → 200 `degraded` (é opcional, ADR-010).
- Dashboard provisionado de `observability/grafana/dashboards/ticketmaster.json` (editável na UI; para
  persistir, exporte o JSON e salve no arquivo).

## Configuração (env do backend)
| Variável | Padrão | Uso |
|---|---|---|
| `DATABASE_URL` | `postgres://ticket:ticket@localhost:5432/ticketmaster?sslmode=disable` | conexão |
| `PORT` | `8080` | porta HTTP |
| `RESERVATION_TTL` | `5m` | duração do hold (formato Go: `30s`, `2m`) |
| `SEED_ON_START` | `true` | popula dados mock se `events` vazia |
| `REDIS_URL` | vazio (compose: `redis://redis:6379/0`) | liga o seat gate em Redis; vazio = só Postgres |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | vazio (compose: `http://lgtm:4318`) | liga a telemetria OTLP/HTTP; vazio = só logs JSON no stdout |
| `OTEL_SERVICE_NAME` | `ticketmaster-api` | `service.name` nos traces/métricas/logs |
