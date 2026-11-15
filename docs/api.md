# API (backend Go)

Base: `http://localhost:8080/api` (ou `http://localhost:5173/api` via proxy do Vite). JSON em tudo.
Erros: `{"error": "mensagem"}`.

| Método | Rota | Descrição |
|---|---|---|
| GET | `/api/health/live` | liveness: processo respondendo (não olha dependências) |
| GET | `/api/health/ready` | readiness: pinga Postgres, Redis e Elasticsearch (ver abaixo). `/api/health` é alias |
| GET | `/api/events` | eventos futuros, ordenados por data |
| GET | `/api/events/{idOrSlug}` | detalhe + assentos (aceita slug ou id numérico) |
| GET | `/api/events/{idOrSlug}/stream` | **SSE**: mapa de assentos em tempo real (ver abaixo) |
| GET | `/api/search?keyword=&startDate=&endDate=` | busca |
| POST | `/api/reserve` | segura assentos (hold) e cria booking PENDING |
| POST | `/api/booking` | confirma (simula pagamento) um booking |
| GET | `/api/booking/{id}` | consulta um booking |
| GET | `/api/venues` | lista os locais (filtro do relatório) |
| GET | `/api/admin/tickets?venueId=&startDate=&endDate=` | relatório: todos os assentos + comprador (**sem auth**) |

> Rascunho original → implementado: `GET /event/{event_id}` virou `GET /api/events/{idOrSlug}`;
> `GET /search`, `POST /reserve`, `POST /booking` mantidos (sob `/api`).

## GET /api/events
```json
[{
  "id": 1, "slug": "jota-quest-rio-de-janeiro", "name": "Jota Quest", "description": "...",
  "starts_at": "2026-12-13T00:00:00Z", "price_cents": 18000,
  "venue": {"id": 1, "name": "Qualistage", "city": "Rio de Janeiro", "address": "...", "rows": 3, "cols": 3},
  "available_seats": 7, "total_seats": 9
}]
```

## GET /api/events/{idOrSlug}
Mesmo objeto acima + `seats`:
```json
"seats": [{"ticket_id": 1, "row": "A", "number": 1, "label": "A1", "status": "AVAILABLE"}]
```
`status`: `AVAILABLE` | `RESERVED` | `SOLD` (reserva vencida já aparece como `AVAILABLE`). 404 se não existir.

## GET /api/events/{idOrSlug}/stream (Server-Sent Events)
Conexão longa com `Content-Type: text/event-stream`. 404 JSON se o evento não existir. Formato:
```
retry: 3000

event: seats
data: {"seats":[{"ticket_id":1,"row":"A","number":1,"label":"A1","status":"RESERVED"}, ...]}

: ping
```
- A 1ª mensagem `seats` chega logo após conectar; as seguintes a cada mudança no mapa — reserva, compra
  ou **hold vencido** (o servidor agenda um refresh para o próximo `reserved_until`).
- Cada mensagem traz o mapa **completo** (snapshot, não diff): o cliente substitui o que tem.
- `: ping` (comentário, ignorado pelo navegador) a cada 20 s mantém proxies sem fechar a conexão.
- Conexão caiu → o `EventSource` reconecta sozinho após `retry` (3 s) e recebe um snapshot novo.
- Testar: `curl -N http://localhost:8080/api/events/jota-quest-rio-de-janeiro/stream`

## GET /api/search
| Param | Formato | Observação |
|---|---|---|
| `keyword` | texto | busca no nome do evento, descrição, nome do local e cidade; ignora acento e caixa; tolera erro de digitação e acha por prefixo/radical (ver abaixo) |
| `startDate` | `YYYY-MM-DD` | inclusivo, fuso America/Sao_Paulo |
| `endDate` | `YYYY-MM-DD` | inclusivo |

Todos opcionais. 400 se data inválida ou `endDate < startDate`. Exemplo:
`/api/search?keyword=rio&startDate=2026-12-01&endDate=2027-12-31`

**Com `keyword` e Elasticsearch ligado (ADR-013)** — busca aproximada:
- todas as palavras digitadas precisam casar (em qualquer campo): `jazz rio` acha "Festival de Jazz" no Qualistage (RJ);
- erro de digitação (1 letra até 5 letras, 2 a partir de 6): `jota qest`, `orqestra`, `caza`;
- prefixo e radical: `casa` acha "Casas de Fado", "Roda de Samba na Casa", "A Casinha Encantada" e o que for no "Casarão Cultural" (seed); `teatr` acha eventos em "Teatro ...";
- a 1ª letra precisa estar certa (`kasa` não acha "casa");
- **ordem por relevância** (nome pesa mais que local/cidade, que pesam mais que descrição); empate → data;
- no máximo 50 resultados.

Sem `keyword`, com `ES_URL` vazio ou com o Elasticsearch fora do ar: busca no Postgres (`ILIKE` sem acento em
nome do evento + local + cidade, sem tolerância a erro), ordenada por data. O formato da resposta é o mesmo.

## POST /api/reserve
```json
{"event_id": 1, "ticket_ids": [7, 8], "customer_name": "Ana", "customer_email": "ana@example.com"}
```
- **201** → booking (`status: "PENDING"`, `expires_at`, `seconds_remaining`, `seats`, `ticket_ids`, `total_cents`)
  - `seconds_remaining` é calculado pelo relógio do banco; o frontend usa este campo no countdown
    (e não `expires_at`) para não depender do relógio do navegador estar sincronizado com o servidor.
- **400** → validação (1 a 9 assentos, ids duplicados, ticket de outro evento, e-mail inválido)
- **409** → assento ocupado: `{"error": "...", "unavailable_ticket_ids": [8]}`. Pode vir do seat gate em Redis
  (sem tocar no banco) ou do Postgres — inclusive quando o assento está travado por uma reserva concorrente
  ainda em andamento (`SKIP LOCKED`, ADR-010).

## POST /api/booking
```json
{"booking_id": "26d8bb8e-0fb7-4b3d-a6ce-6dec58e56ed2"}
```
- **200** → booking `CONFIRMED` (idempotente: confirmar de novo devolve 200)
- **404** → booking não existe
- **410** → reserva expirou

## GET /api/venues
```json
[{"id": 1, "name": "Qualistage", "city": "Rio de Janeiro", "address": "...", "rows": 3, "cols": 3}]
```
Ordenado por cidade e nome.

## GET /api/admin/tickets
Relatório usado pela página `/admin`. Devolve **um item por assento** (vendido, reservado ou disponível),
ordenado por data do evento e assento. Diferente da busca pública, **inclui eventos passados**.

> ⚠️ Sem autenticação: expõe nome e e-mail dos compradores. Aceitável só no projeto de estudo (ADR-009).

| Param | Formato | Observação |
|---|---|---|
| `venueId` | inteiro > 0 | filtra pelo local (ids de `GET /api/venues`) |
| `startDate` | `YYYY-MM-DD` | data do evento, inclusivo, fuso America/Sao_Paulo |
| `endDate` | `YYYY-MM-DD` | inclusivo |

Todos opcionais. 400 se `venueId` não for inteiro positivo, data inválida ou `endDate < startDate`.

```json
[{
  "ticket_id": 4, "seat": "B1", "status": "SOLD", "price_cents": 18000,
  "event_id": 1, "event_slug": "jota-quest-rio-de-janeiro", "event_name": "Jota Quest",
  "event_starts_at": "2026-12-13T00:00:00Z",
  "venue_id": 1, "venue_name": "Qualistage", "venue_city": "Rio de Janeiro",
  "buyer": {
    "booking_id": "…", "name": "Cliente Seed", "email": "…",
    "booked_at": "2026-11-09T00:56:06Z",
    "confirmed_at": "2026-11-09T00:56:06Z"
  }
}]
```
- `status` é o **efetivo**: hold vencido aparece como `AVAILABLE`, igual ao mapa de assentos.
- `buyer` é `null` quando o assento está livre (inclusive hold vencido).
- `buyer.confirmed_at` vem quando `SOLD`; `buyer.reserved_until` (fim do hold) vem quando `RESERVED`.

## Exemplos com curl
```bash
curl localhost:8080/api/events/jota-quest-rio-de-janeiro
curl -X POST localhost:8080/api/reserve -d '{"event_id":1,"ticket_ids":[1],"customer_name":"Ana","customer_email":"ana@x.com"}'
curl -X POST localhost:8080/api/booking -d '{"booking_id":"<id>"}'
curl "localhost:8080/api/admin/tickets?venueId=1&startDate=2026-12-01"
```

## Health check — `GET /api/health/ready`
```json
{"status":"degraded","checks":{
  "postgres":{"status":"ok","latency_ms":1},
  "redis":{"status":"down","latency_ms":502,"error":"dial tcp: lookup redis: i/o timeout"},
  "elasticsearch":{"status":"ok","latency_ms":11}}}
```
| `status` | HTTP | Quando |
|---|---|---|
| `ok` | 200 | Postgres, Redis e Elasticsearch respondendo (ou `disabled`, com `REDIS_URL`/`ES_URL` vazio) |
| `degraded` | 200 | Redis fora: reservas seguem só com o Postgres (ADR-010); Elasticsearch fora: busca cai para o `ILIKE` (ADR-013) |
| `down` | 503 | Postgres fora (ou ping > 1 s): não dá para atender |
