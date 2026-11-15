# CLAUDE.md — Ticketmaster Lite (projeto de estudo)

Contexto para o Claude retomar o trabalho em sessões futuras. **Leia também `docs/`** antes de mudanças grandes.

## O que é
Monorepo de estudo: venda de ingressos com **lugar marcado** para eventos físicos (nome, local, data).
Cada venue tem uma matriz de assentos **3x3** (A1..C3). Um assento só pode ser vendido para uma pessoa —
o foco de estudo é **controle de concorrência** na reserva.

Requisitos: (a) listar eventos, (b) pesquisar por palavra-chave + intervalo de datas, (c) comprar ingressos
sem venda duplicada do mesmo assento.

## Stack / ambiente
- Windows + WSL2 + Docker. **Go não está instalado no host** — compilar/rodar via Docker.
- `backend/` — Go 1.23, `net/http` (roteamento do Go 1.22+), `pgx/v5`. Sem framework.
- `frontend/` — React 18 + Vite + TypeScript + react-router-dom 6. Dev server com proxy `/api` → backend.
- Postgres 16 (extensão `unaccent` para busca sem acento) — fonte da verdade dos assentos.
- Redis 7 (opcional, `REDIS_URL`): só filtro rápido de holds na frente do Postgres (ADR-010), nunca fonte da verdade.
- Elasticsearch 8 (opcional, `ES_URL`): índice de busca aproximada (ADR-013), alimentado por outbox
  transacional (`search_outbox`); nunca fonte da verdade, fallback para `ILIKE` no Postgres.
- Observabilidade (ADR-011): OpenTelemetry (OTLP/HTTP, `OTEL_EXPORTER_OTLP_ENDPOINT`; vazio desliga) →
  container `grafana/otel-lgtm` (Grafana + Prometheus + Tempo + Loki). Versões OTel fixadas para Go 1.23.

## Comandos
`make` pode não estar instalado no WSL (`sudo apt install make`); os alvos são só atalhos para `docker compose`.
```bash
make up            # docker compose up -d --build (postgres:5432, backend:8080, frontend:5173)
make logs          # logs do backend
make reset         # apaga o volume do banco e sobe de novo (re-seed)
make loadtest      # 50 reservas simultâneas no mesmo assento -> espera 1x201 e 49x409
make go-check      # gofmt + go vet + go build dentro de um container golang
make fe-check      # tsc + vite build dentro de um container node
make obs           # mostra as URLs do Grafana / dashboard
make es-reindex    # reenfileira todos os eventos no outbox -> Elasticsearch
make es-search q=casa  # testa a busca
```
URLs: http://localhost:5173 (web), http://localhost:5173/event/jota-quest-rio-de-janeiro, http://localhost:5173/admin, http://localhost:8080/api/events,
http://localhost:8080/api/health/ready, http://localhost:3000/d/ticketmaster-lite (Grafana),
http://localhost:9200/events/_count (Elasticsearch)

## Estrutura
```
backend/
  cmd/api/            main: config via env, migrate, seed, servidor HTTP
  cmd/loadtest/       teste de concorrência (binário incluso na imagem: /app/loadtest)
  internal/api/       handlers HTTP + validação + mapeamento de erros -> status
  internal/store/     queries SQL e regras de reserva/confirmação (concorrência mora aqui)
  internal/holds/     seat gate em Redis (implementa store.SeatGate; Lua, timeout, circuit breaker)
  internal/search/    busca com Elasticsearch (store.Searcher): cliente REST, mapping, query fuzzy, indexer do outbox
  internal/live/      mapa de assentos em tempo real: LISTEN/NOTIFY do Postgres -> hub -> SSE (ADR-012)
  internal/db/        conexão, runner de migrations (embed), seed.sql
  internal/telemetry/ setup do OpenTelemetry (traces/métricas/logs OTLP) + logger slog com trace_id
frontend/src/
  api.ts              tipos + cliente HTTP
  pages/HomePage.tsx  lista + busca (filtros na query string)
  pages/EventPage.tsx detalhe, mapa de assentos (SSE via EventSource), reserva, countdown, confirmação
  pages/AdminPage.tsx relatório de ingressos (filtros local/data/status, botão atualizar)
  components/SeatMap.tsx
observability/        provisioning + dashboard JSON do Grafana (montados no container lgtm)
docs/                 arquitetura, API, decisões (ADRs), roadmap
```

## Convenções
- Documentação e mensagens ao usuário em **português**; identificadores de código em inglês.
- Schema: **nunca editar migration já aplicada**; criar `backend/internal/db/migrations/NNN_descricao.sql`.
- Seed (`seed.sql`) só roda se `events` estiver vazia; após mudá-lo use `make reset`.
- Toda rota da API fica sob `/api`. Rotas do frontend: `/`, `/event/:slug` e `/admin` (relatório).
- Ao concluir uma mudança relevante: atualizar `docs/api.md` / `docs/architecture.md`, registrar decisão em
  `docs/decisions.md` e marcar o item em `docs/roadmap.md` (seção "Histórico").
