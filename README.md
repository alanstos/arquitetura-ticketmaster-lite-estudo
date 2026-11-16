# Ticketmaster Lite — projeto de estudo

Venda de ingressos com lugar marcado (matriz 3x3 por local), com foco em **impedir que dois usuários comprem
o mesmo assento**. Monorepo: backend Go + frontend React + Postgres, tudo em Docker.

## Rodando (WSL + Docker)
```bash
make up        # ou: docker compose up -d --build
```
- Web: http://localhost:5173 — ex.: http://localhost:5173/event/jota-quest-rio-de-janeiro
- Relatório (admin): http://localhost:5173/admin — ingressos vendidos/disponíveis e compradores, filtro por local e data
- API: http://localhost:8080/api/events

O banco é criado e populado com eventos mock automaticamente no start. `make reset` recria do zero.

## Testando a concorrência
```bash
make loadtest  # 50 reservas simultâneas no mesmo assento -> 1 aceita, 49 recebem 409
```
Ou abra a mesma página de evento em duas abas e tente reservar o mesmo assento.

## Observabilidade: Grafana
Sobe junto com o `make up` (container `grafana/otel-lgtm`, ver ADR-011).

- **Abra no navegador do Windows:** http://localhost:3000. Não tem login: o acesso anônimo já entra como admin.
- **Dashboard do projeto:** http://localhost:3000/d/ticketmaster-lite (também em ☰ → Dashboards → "Ticketmaster Lite").
  Mostra requisições/s, erros 5xx, latência por rota, reservas por resultado, fallback do seat gate,
  pool do Postgres, busca (Elasticsearch × fallback) e, no fim, logs do backend e os traces de reserva mais lentos.
- **Explore** (http://localhost:3000/explore): escolha a fonte no topo:
  - **Tempo** = traces: veja uma requisição do HTTP até cada query no Postgres e o script Lua no Redis.
  - **Loki** = logs do backend (cada linha tem `trace_id`, que leva ao trace).
  - **Prometheus** = métricas (ex.: `ticketmaster_search_fallback_total`).
- Dica: rode `make loadtest` e acompanhe o dashboard (canto superior direito: período "Last 15 minutes" e auto-refresh).
- `make obs` mostra essas URLs.

## Busca: Elasticsearch + Kibana
A busca por palavra-chave usa o Elasticsearch (ADR-013), que sobe com o `make up`:
http://localhost:9200/events/_count. O **Kibana** é uma interface visual para inspecioná-lo. Ele é pesado
(~1 GB de RAM), então **só sobe sob demanda** (ADR-014):

```bash
make kibana        # ou: docker compose --profile tools up -d kibana kibana-setup
make kibana-down   # ou: docker compose --profile tools rm -sf kibana kibana-setup
```

O Kibana só precisa do Elasticsearch; o resto do projeto pode estar parado.

**1. Abra no navegador do Windows:** http://localhost:5601 (leva ~1 min para ficar pronto; confira com
`docker compose ps kibana` até aparecer *healthy*).
O WSL2 repassa a porta para o Windows, então o `localhost` funciona direto. Não tem login, porque a segurança
está desligada. Se aparecer uma tela de boas-vindas, clique em **"Explore on my own"**.

**2. Ver os documentos (Discover)**
- Menu ☰ (canto superior esquerdo) → **Analytics → Discover**, ou direto: http://localhost:5601/app/discover
- No canto superior esquerdo, confira se a data view selecionada é **"events (busca)"** (criada
  automaticamente pelo container `kibana-setup`).
- Clique na setinha ao lado de um documento para ver todos os campos (`name`, `city`, `starts_at`…).
- No campo de busca no topo dá para filtrar, por exemplo `city : "Rio de Janeiro"`.
- A data view não tem campo de tempo de propósito: os eventos estão no futuro, e o filtro padrão
  ("últimos 15 minutos") esconderia todos.

**3. Rodar queries (Dev Tools), a parte mais útil para estudar**
- Menu ☰ → **Management → Dev Tools**, ou: http://localhost:5601/app/dev_tools#/console
- Apague o exemplo que vem no editor da esquerda.
- Copie uma requisição de [observability/kibana/console-exemplos.txt](observability/kibana/console-exemplos.txt)
  e cole lá. Por exemplo:
  ```
  POST events/_analyze
  {
    "analyzer": "pt_text",
    "text": "Casarão Cultural São Paulo"
  }
  ```
- Com o cursor dentro da requisição, aperte **Ctrl+Enter** (ou clique no ▶). A resposta aparece à direita.
- Sugestão de roteiro: `GET events/_mapping` → os `_analyze` → a query do backend trocando "jota qest" por
  outros termos → o `explain`, para ver por que um evento ficou na frente do outro.

Pela linha de comando: `make es-search q=casa` (busca pela API) e `make es-reindex` (reindexa tudo a partir do Postgres).

> Se o `make` não estiver instalado no WSL (`sudo apt install make`), use os comandos `docker compose`
> equivalentes, que estão no [Makefile](Makefile).

## Documentação
- [docs/architecture.md](docs/architecture.md) — modelo de dados, fluxo de compra, estratégia de concorrência
- [docs/api.md](docs/api.md) — endpoints
- [docs/decisions.md](docs/decisions.md) — decisões (ADRs)
- [docs/roadmap.md](docs/roadmap.md) — próximos passos e histórico
- [CLAUDE.md](CLAUDE.md) — contexto para o Claude Code
