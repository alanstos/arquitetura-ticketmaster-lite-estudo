# Ticketmaster Lite — projeto de estudo

Venda de ingressos com lugar marcado (matriz 3x3 por local), com foco em **impedir que dois usuários comprem
o mesmo assento**. Monorepo: backend Go + frontend React + Postgres, tudo em Docker.

## Rodando (WSL + Docker)
```bash
make up        # ou: docker compose up -d --build
```
- Web: http://localhost:5173 — ex.: http://localhost:5173/event/jota-quest-rio-de-janeiro
- API: http://localhost:8080/api/events

O banco é criado e populado com eventos mock automaticamente no start. `make reset` recria do zero.

## Testando a concorrência
```bash
make loadtest  # 50 reservas simultâneas no mesmo assento -> 1 aceita, 49 recebem 409
```
Ou abra a mesma página de evento em duas abas e tente reservar o mesmo assento.

## Documentação
- [docs/architecture.md](docs/architecture.md) — modelo de dados, fluxo de compra, estratégia de concorrência
- [docs/api.md](docs/api.md) — endpoints
- [docs/decisions.md](docs/decisions.md) — decisões (ADRs)
- [docs/roadmap.md](docs/roadmap.md) — próximos passos e histórico
- [CLAUDE.md](CLAUDE.md) — contexto para o Claude Code
