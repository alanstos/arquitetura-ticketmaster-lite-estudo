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

## ADR-010 — Redis como filtro de holds na frente do Postgres; `SKIP LOCKED` na reserva
- **Contexto**: em pico (muita gente no mesmo assento), o custo não era volume de escrita (cada reserva são
  2 escritas e a expiração já é preguiçosa, ADR-005), e sim a **fila de lock**: com `FOR UPDATE` os
  perdedores esperavam o vencedor commitar, segurando conexões, só para receber 409.
- **Decisão**:
  1. `Reserve` passa a usar `UPDATE ... WHERE id IN (SELECT ... FOR UPDATE SKIP LOCKED) RETURNING`: perdedores
     recebem 409 imediatamente; menos linhas retornadas que o pedido → rollback (tudo ou nada).
  2. Seat gate opcional em Redis (`internal/holds`, `REDIS_URL`): `SET NX PX` por assento via Lua (tudo ou
     nada) antes da transação. Recusa sem tocar no banco; quem passa ainda é validado pelo Postgres.
     O store depende só da interface `store.SeatGate`.
- **Alternativa descartada**: Redis como fonte da verdade do hold (Postgres só na confirmação). Exigiria
  dual-write sem transação comum, e perda de chave em failover (replicação assíncrona) permitiria dois holds
  no mesmo assento; a confirmação teria que consultar os dois sistemas.
- **Consequências**:
  - Correção não depende do Redis: com ele fora, o gate abre um circuit breaker (2 s) e tudo segue pelo
    Postgres; prazo duro de 300 ms por chamada (medido: sem ele, 50 reservas levavam até 25 s com o Redis parado).
  - Pior caso de inconsistência: chave órfã no Redis (crash entre o `SET` e o rollback) recusa o assento até
    o TTL, embora o mapa mostre livre. Nunca venda duplicada.
  - `SKIP LOCKED` pode recusar um assento cuja transação concorrente acabou em rollback (409 "falso").
  - Chaves não são apagadas na confirmação/expiração: vencem pelo TTL junto com o hold do banco.
  - Mais um serviço no compose (Redis sem persistência: `--save "" --appendonly no`).
