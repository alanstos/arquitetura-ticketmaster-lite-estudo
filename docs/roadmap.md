# Roadmap e histórico

## Próximas ideias (não implementadas)
- [ ] Testes automatizados no backend (testcontainers / Postgres no compose) cobrindo `Reserve` concorrente
- [ ] Comparar estratégias de concorrência (ADR-004): otimista com `version`, SERIALIZABLE
- [ ] Cache curto do mapa de assentos no Redis (invalidado em reserve/confirm) + pub/sub para o frontend
- [ ] Fila virtual / rate limit por evento no Redis para aberturas de venda
- [ ] Endpoint para cancelar reserva (liberar hold no Postgres e no Redis antes do TTL)
- [ ] Job que marca bookings vencidos como `EXPIRED` e libera tickets
- [ ] Atualização em tempo real do mapa (SSE/WebSocket) em vez de polling
- [ ] Autenticação de usuário e "meus ingressos"
- [ ] Proteger `/admin` e `/api/admin/*` (auth + papel de administrador) — hoje expõe nome/e-mail (ADR-009)
- [ ] Paginação e exportação CSV no relatório `/admin`
- [ ] Venues com tamanhos variados / setores e preços por setor
- [ ] Paginação na listagem/busca; índice trigram (`pg_trgm`) para a busca
- [ ] Idempotency-Key no `POST /reserve`
- [ ] Build de produção do frontend (nginx) e compose de "prod"
- [ ] Trocar runner de migrations por golang-migrate ou goose

## Histórico
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
