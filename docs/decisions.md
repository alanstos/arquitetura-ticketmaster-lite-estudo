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

## ADR-011 — Observabilidade com OpenTelemetry + Grafana LGTM local (em vez de Datadog)
- **Contexto**: só havia log de acesso em texto no stdout e um `/api/health` que não olhava dependências.
  Objetivo de estudo: ver os três pilares (logs, métricas, traces) — em especial o caminho de uma reserva
  disputada (Redis → Postgres) — e checar a saúde do sistema.
- **Decisão**:
  1. Instrumentar com **OpenTelemetry** e exportar via **OTLP/HTTP** (`internal/telemetry`): `otelhttp` no
     router, `otelpgx` no pool, `redisotel` no seat gate, spans/contadores de negócio no `store`, `slog`
     com fan-out (JSON no stdout com `trace_id` + bridge `otelslog`).
  2. Backend de observabilidade local: container **`grafana/otel-lgtm`** (Collector + Prometheus + Tempo +
     Loki + Grafana) com dashboard provisionado.
  3. Health dividido em **liveness** (`/api/health/live`) e **readiness** (`/api/health/ready`, com
     `degraded` quando só o Redis cai).
- **Alternativas**:
  - **Datadog**: UI excelente e APM pronto, mas exige conta/API key, trial de 14 dias e depois é pago, e os
    dados saem da máquina. Instrumentar com o SDK proprietário (`dd-trace-go`) prenderia o código ao fornecedor.
    Com OTel, migrar é configuração: o Datadog Agent recebe OTLP (`OTEL_EXPORTER_OTLP_ENDPOINT=http://datadog-agent:4318`).
  - **Prometheus scrape (`/metrics`) + promtail**: mais peças e não cobre traces; OTLP push unifica os três sinais.
- **Consequências**:
  - Mais um container (~1 GB de imagem, alguns centos de MB de RAM); dados em volume `lgtmdata`.
  - `OTEL_EXPORTER_OTLP_ENDPOINT` vazio desliga tudo (providers no-op): o app não depende do coletor.
  - Versões do OTel fixadas para Go 1.23 (`otel` v1.35, `otelhttp` v0.60). Esse `otelhttp` emite a convenção
    HTTP **antiga** (`http.server.duration` em ms); a estável (`http.server.request.duration` em s) só via
    `OTEL_SEMCONV_STABILITY_OPT_IN=http/dup` (duplicada). Ao subir o Go, atualizar e ajustar o dashboard.
  - `otelpgx` grava o SQL nos spans (sem valores dos parâmetros). Logs de acesso não incluem dados pessoais,
    mas o relatório `/admin` continua sem autenticação (ADR-009).
  - `grafana/otel-lgtm` é para dev/estudo (sem HA, retenção curta); em produção cada componente seria separado
    ou um SaaS.


## ADR-012 — Mapa de assentos em tempo real com SSE + Postgres LISTEN/NOTIFY
- **Contexto**: a página do evento fazia polling de `GET /api/events/{slug}` a cada 5 s: até 5 s de atraso
  para ver a compra de outra pessoa e carga proporcional a usuários × tempo, mesmo sem mudança.
- **Decisão**:
  1. **SSE** (`GET /api/events/{idOrSlug}/stream`): o servidor manda o mapa completo a cada mudança.
  2. **Trigger em `tickets`** (migration 002) faz `pg_notify('seat_changes', event_id)`; `internal/live.Hub`
     escuta com `LISTEN` numa conexão dedicada, relê o mapa (`store.SeatSnapshot`) e faz fan-out.
  3. **Timer** para o próximo `reserved_until`, porque a expiração preguiçosa (ADR-005) não gera escrita.
- **Alternativas**:
  - **WebSocket**: bidirecional, mas aqui o fluxo é só servidor → cliente; SSE é HTTP comum (passa pelo
    proxy do Vite sem configuração), tem reconexão nativa no `EventSource` e não precisa de biblioteca.
  - **Broker em memória** (store publica após commit): mais simples, mas só vale com uma instância e
    exigiria lembrar de publicar em todo caminho de escrita.
  - **Redis pub/sub**: multi-instância, mas o Redis é opcional (ADR-010) e o publish não é atômico com o commit.
  - **Diff em vez de snapshot**: economiza bytes, mas exige ordem e entrega garantidas. Com 9 assentos o
    snapshot é pequeno e idempotente.
- **Consequências**:
  - Uma conexão extra no Postgres por instância (fora do pool). Payload do NOTIFY é só o `event_id`.
  - Rollback não notifica: no loadtest (1×201, 49×409) o stream recebe uma única atualização.
  - Conexões longas: sem `WriteTimeout`, shutdown fecha os streams via `RegisterOnShutdown`; streams fora
    do `otelhttp`, com gauge próprio.
  - Em escala, um snapshot por evento por mudança poderia virar gargalo; o worker já coalesce rajadas, e um
    debounce curto ou cache (ver roadmap) seriam os próximos passos.


## ADR-013 — Busca aproximada com Elasticsearch, alimentado por outbox transacional
- **Contexto**: a busca era `unaccent(evento || local || cidade) ILIKE '%kw%'`: sempre *seq scan* (expressão
  sobre JOIN + `%` à esquerda) e sem tolerância a erro de digitação nem variações ("casa" → casinha).
- **Decisão**:
  1. **Elasticsearch** (nó único, sem segurança, só local) como índice de busca: analyzer português
     (stemmer leve + sem acento), edge n-grams para prefixo e `fuzziness: AUTO` para erro de digitação.
  2. **Postgres continua a fonte da verdade**: o ES devolve só IDs ordenados; os dados vêm do Postgres
     (assentos livres sempre atuais, filtros de data reaplicados).
  3. **Outbox transacional** (`search_outbox`, migration 003) preenchido por triggers + `NOTIFY`; um indexer
     no backend consome em lotes (`SKIP LOCKED`) e envia `_bulk`. Sem *dual write* na aplicação.
  4. **Opcional e degradável**, como o Redis (ADR-010): `ES_URL` vazio desliga; erro/timeout (500 ms) abre
     um circuit breaker de 5 s e a busca cai para o `ILIKE`.
  5. Cliente **REST puro** (`net/http`), sem biblioteca: chamadas explícitas e sem conflito de versões no Go 1.23.
- **Alternativas**:
  - **`pg_trgm` + full-text no Postgres**: sem componente novo, transacional e usa índice GIN; suficiente para
    milhares/milhões de eventos. Preterido só porque o objetivo aqui é estudar um motor de busca dedicado;
    numa decisão de produto com esse volume, seria a primeira opção.
  - **Meilisearch/Typesense**: tolerância a erro pronta e bem mais leves; menos controle de analyzers/score
    e menos comum no mercado que ES/OpenSearch.
  - **OpenSearch**: equivalente (fork do ES 7.10); a API usada aqui funciona nos dois.
  - **Dual write** (gravar no ES logo após o commit): mais simples, mas se o processo cair entre o commit e a
    chamada ao ES o índice fica divergente para sempre. **CDC** (Debezium + Kafka): robusto, mas pesado demais.
- **Consequências**:
  - Consistência eventual (~1 s) só na seleção dos resultados; os dados exibidos são sempre os do Postgres.
  - +~1 GB de RAM (JVM com heap de 512 MB) e mais um volume (`esdata`). Apagar o volume é seguro: o índice é
    recriado e reindexado a partir do Postgres na próxima subida.
  - Com keyword, a ordem passa a ser por relevância (não mais por data) e limitada a 50 resultados.
  - Também busca na descrição (o `ILIKE` não buscava); o fallback continua sem descrição e sem tolerância a erro.
  - Pequeno custo nas escritas de `events`/`venues` (uma linha no outbox por evento afetado).

