# Decisões de arquitetura (ADRs resumidos)

Formato: contexto → decisão → consequências. Adicione novas no final, numeradas.

## ADR-001 — Monorepo com docker compose
- **Contexto**: estudo em Windows/WSL; Go não instalado no host.
- **Decisão**: `backend/`, `frontend/` e `docker-compose.yml` na raiz; build e verificação via containers (`Makefile`).
- **Consequências**: ambiente reprodutível; primeiro build mais lento.

## ADR-002 — Go sem framework (net/http + pgx)
- **Decisão**: roteamento nativo do Go 1.22 (`"GET /api/events/{idOrSlug}"`), `pgx/v5` com SQL escrito à mão.
- **Por quê**: o foco do estudo é ver o SQL e a transação de reserva explicitamente, sem ORM escondendo locks.

## ADR-003 — Assentos pré-materializados em `tickets`
- **Decisão**: ao criar o evento, gera-se uma linha por assento (9 por evento).
- **Por quê**: a disputa vira uma atualização de linha existente, protegida por lock de linha.
- **Alternativa descartada**: inserir ticket só na compra com `UNIQUE(event_id, seat)` — também funciona
  (concorrência resolvida pela constraint), mas complica o hold temporário e a exibição do mapa.

## ADR-004 — Lock pessimista (`SELECT ... FOR UPDATE`) + hold com TTL
- **Decisão**: reserva em duas fases (`/reserve` cria hold; `/booking` confirma). Locks em ordem de id.
- **Alternativas para estudar depois**: lock otimista (coluna `version` + `UPDATE ... WHERE version = $n`),
  `UPDATE ... WHERE status='AVAILABLE' RETURNING` (lock implícito), isolamento SERIALIZABLE com retry,
  lock distribuído em Redis (padrão do Ticketmaster real para holds).

## ADR-005 — Expiração preguiçosa (sem job)
- **Decisão**: `RESERVED` com `reserved_until < now()` é tratado como livre nas consultas.
- **Consequências**: simples e correto; bookings vencidos ficam `PENDING` no banco até alguém tentar
  confirmá-los (a API os exibe como `EXPIRED`). Um job de limpeza pode vir depois.

## ADR-006 — Slug como identificador público do evento
- **Decisão**: URL `/event/{slug}`; slug UNIQUE e definido no seed. API aceita slug ou id.

## ADR-007 — Migrations próprias com `embed`
- **Decisão**: runner mínimo em `internal/db` aplica `migrations/*.sql` em ordem e registra em `schema_migrations`.
- **Por quê**: zero dependência extra; trocar por golang-migrate/goose é um item do roadmap.

## ADR-008 — Frontend em Vite dev server dentro do Docker
- **Decisão**: container roda `vite` com hot reload e proxy `/api`; sem nginx por enquanto.
- **Consequências**: ótimo para estudo; não é setup de produção.

## ADR-009 — Relatório `/admin` sem autenticação, uma linha por assento
- **Contexto**: precisamos visualizar ingressos vendidos/disponíveis e quem comprou, filtrando por local e data.
- **Decisão**: endpoint de leitura `GET /api/admin/tickets` que parte de `tickets` (LEFT JOIN `bookings` só
  quando o assento não está livre) e devolve uma linha por assento; status efetivo calculado com `freeSeatSQL`.
  Filtros de local/data no servidor; filtro de status e totais no cliente (volume máximo = 9 × nº de eventos).
  Atualização manual (botão), sem polling.
- **Consequências**: relatório consistente com o mapa de assentos e sem custo de lock. **Expõe dados pessoais
  sem autenticação** — aceitável apenas em ambiente de estudo; proteger `/api/admin/*` antes de qualquer
  uso real (item no roadmap). Sem paginação: se o volume crescer, paginar e mover o filtro de status para o servidor.
