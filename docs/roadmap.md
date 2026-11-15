# Roadmap e histórico

## Próximas ideias (não implementadas)
- [ ] Testes automatizados no backend (testcontainers / Postgres no compose) cobrindo `Reserve` concorrente
- [ ] Comparar estratégias de concorrência (ADR-004): otimista com `version`, SERIALIZABLE
- [ ] Cache curto do mapa de assentos no Redis (invalidado em reserve/confirm) + pub/sub para o frontend
- [ ] Fila virtual / rate limit por evento no Redis para aberturas de venda
- [ ] Endpoint para cancelar reserva (liberar hold no Postgres e no Redis antes do TTL)
- [ ] Job que marca bookings vencidos como `EXPIRED` e libera tickets
- [ ] Autenticação de usuário e "meus ingressos"
- [ ] Proteger `/admin` e `/api/admin/*` (auth + papel de administrador) — hoje expõe nome/e-mail (ADR-009)
- [ ] Paginação e exportação CSV no relatório `/admin`
- [ ] Venues com tamanhos variados / setores e preços por setor
- [ ] Paginação na listagem/busca (`search_after` no Elasticsearch)
- [ ] Busca: autocomplete/sugestões ("você quis dizer…") e destaque (`highlight`) do termo encontrado
- [ ] Busca: comparar com `pg_trgm` + full-text do Postgres (sem componente extra) — ver ADR-013
- [ ] Idempotency-Key no `POST /reserve`
- [ ] Build de produção do frontend (nginx) e compose de "prod"
- [ ] Trocar runner de migrations por golang-migrate ou goose
- [ ] Observabilidade: alertas no Grafana (ex.: p99 de `/api/reserve`, taxa de 5xx, fallback do seat gate)
- [ ] Observabilidade: OTel no frontend (trace do clique até o banco via `traceparent`)
- [ ] Observabilidade: SLO de disponibilidade/latência da reserva; testar envio ao Datadog via OTLP

## Histórico
### 2026-11-15 — Busca aproximada com Elasticsearch
- `GET /api/search` com `keyword` tolera erro de digitação (`jota qest`), acha por prefixo/radical
  (`casa` → casas, casarão, casinha) e ordena por relevância; mesmo contrato da API.
- Elasticsearch 8.15 no compose (`ES_URL`, vazio desliga). Novo `internal/search`: cliente REST puro,
  mapping/analyzers em português, query fuzzy, circuit breaker, indexer.
- Migration `003_search_outbox`: triggers em `events`/`venues` gravam no outbox na mesma transação + NOTIFY.
- Fallback para `ILIKE` no Postgres; readiness `degraded` com ES fora; métricas `ticketmaster.search.*` e
  painéis no Grafana. Seed com 3 eventos "casa/casinha/casas" (exige `make reset`). Alvos `make es-reindex`/`es-search`.
- Validado: cenários de busca acima, filtro de datas inclusivo, update/delete refletidos em ~1 s,
  ES parado → fallback (~4 ms com breaker aberto), loadtest 1×201/49×409. Decisão no ADR-013.

### 2026-11-15 — Mapa de assentos em tempo real (SSE)
- [x] Atualização em tempo real do mapa (SSE/WebSocket) em vez de polling.
- `GET /api/events/{idOrSlug}/stream` (Server-Sent Events) com snapshot completo do mapa a cada mudança.
- Migration `002_seat_notify`: trigger em `tickets` → `pg_notify('seat_changes', event_id)`.
- Novo `internal/live`: `LISTEN` em conexão dedicada (reconexão com backoff), worker único que coalesce e
  serializa snapshots, timer para o próximo vencimento de hold (expiração preguiçosa), gauge
  `ticketmaster.sse.subscribers`. `store.SeatSnapshot` extraído de `GetEvent`.
- Frontend: `EventPage` usa `EventSource` em vez de `setInterval`; aviso "reconectando…" se o stream cair.
- Validado: push na reserva, liberação no vencimento do hold sem escrita, loadtest 1×201/49×409 com 1 só
  atualização no stream, shutdown < 1 s com stream aberto. Decisão no ADR-012.

### 2026-11-15 — Observabilidade (OpenTelemetry + Grafana LGTM)
- Novo `internal/telemetry`: traces, métricas e logs via OTLP/HTTP; `slog` em JSON com `trace_id`/`span_id`.
- Instrumentação: `otelhttp` (span/métrica por rota), `otelpgx` (span por query + métricas do pool),
  `redisotel` (seat gate); spans `store.Reserve`/`store.ConfirmBooking` e contadores
  `ticketmaster.reservations{result}`, `ticketmaster.seat_gate.fallback{reason}`, `ticketmaster.bookings{result}`.
- Health: `/api/health/live` (healthcheck do compose) e `/api/health/ready` (Postgres/Redis, `ok`/`degraded`/`down`).
- Serviço `grafana/otel-lgtm` no compose (Grafana em http://localhost:3000) + dashboard provisionado
  "Ticketmaster Lite". Graceful shutdown passou a esperar o dreno antes do flush. Decisão no ADR-011.

### 2026-11-15 — Redis como seat gate + `SKIP LOCKED`
- `Reserve` com UPDATE condicional e `FOR UPDATE SKIP LOCKED`: perdedores recebem 409 na hora, sem fila de lock.
- Novo `internal/holds` (go-redis v9): holds por assento com `SET NX PX` em script Lua (tudo ou nada),
  liberação com compare-and-delete, prazo de 300 ms e circuit breaker de 2 s. Postgres segue como fonte da verdade.
- Serviço `redis:7-alpine` no compose; `REDIS_URL` vazio desliga o gate.
- `cmd/loadtest` passou a mostrar latência p50/p99/máx. Decisão no ADR-010.

### 2026-11-15 — relatório `/admin`
- Página `/admin` (link "Relatório" no topo): tabela com todos os assentos — vendido / reservado / disponível —
  e o comprador (nome, e-mail, data da compra ou fim do hold), valor e receita dos vendidos.
- Filtros por local e intervalo de datas do evento (servidor) e por status (cliente); tudo na query string.
- Botão "Atualizar" com ícone (sem polling).
- Backend: `GET /api/venues` e `GET /api/admin/tickets`; validação de datas extraída para `parseDateRange`
  (reaproveitada pela busca). Decisão registrada no ADR-009.

### 2026-11-08 — fix countdown da reserva
- Bug: a tela dizia "O tempo da sua reserva acabou" logo após reservar. O `useCountdown` guardava o valor em
  estado (0 antes da reserva) e, no 1º render com a reserva, o efeito de expiração via 0 e descartava o booking.
- Correção: countdown calculado no render a partir de um prazo local; backend passou a enviar
  `seconds_remaining` (relógio do banco), eliminando dependência de sincronia de relógio WSL2 × Windows.

### 2026-11-08 — v0.1 (estrutura inicial)
- Monorepo com docker compose: Postgres 16, backend Go, frontend React/Vite.
- Schema venues/events/tickets/bookings, migrations embutidas, seed com 6 eventos (alguns assentos já vendidos).
- Endpoints: listar, detalhe por slug/id, busca (keyword sem acento + datas), reserve, booking.
- Concorrência com `SELECT ... FOR UPDATE`; hold de 5 min com expiração preguiçosa.
- Frontend: lista + busca, página `/event/:slug` com mapa 3x3, reserva, countdown e confirmação.
- `cmd/loadtest`: validado 50 requisições simultâneas → 1×201, 49×409.
