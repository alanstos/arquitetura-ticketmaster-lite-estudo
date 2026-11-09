# Roadmap e histórico

## Próximas ideias (não implementadas)
- [ ] Testes automatizados no backend (testcontainers / Postgres no compose) cobrindo `Reserve` concorrente
- [ ] Comparar estratégias de concorrência (ADR-004): otimista com `version`, SERIALIZABLE, Redis
- [ ] Job que marca bookings vencidos como `EXPIRED` e libera tickets
- [ ] Atualização em tempo real do mapa (SSE/WebSocket) em vez de polling
- [ ] Autenticação de usuário e "meus ingressos"
- [ ] Venues com tamanhos variados / setores e preços por setor
- [ ] Paginação na listagem/busca; índice trigram (`pg_trgm`) para a busca
- [ ] Idempotency-Key no `POST /reserve`
- [ ] Build de produção do frontend (nginx) e compose de "prod"
- [ ] Trocar runner de migrations por golang-migrate ou goose

## Histórico
### 2026-11-08 — v0.1 (estrutura inicial)
- Monorepo com docker compose: Postgres 16, backend Go, frontend React/Vite.
- Schema venues/events/tickets/bookings, migrations embutidas, seed com 6 eventos (alguns assentos já vendidos).
- Endpoints: listar, detalhe por slug/id, busca (keyword sem acento + datas), reserve, booking.
- Concorrência com `SELECT ... FOR UPDATE`; hold de 5 min com expiração preguiçosa.
- Frontend: lista + busca, página `/event/:slug` com mapa 3x3, reserva, countdown e confirmação.
- `cmd/loadtest`: validado 50 requisições simultâneas → 1×201, 49×409.
