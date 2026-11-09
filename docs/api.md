# API (backend Go)

Base: `http://localhost:8080/api` (ou `http://localhost:5173/api` via proxy do Vite). JSON em tudo.
Erros: `{"error": "mensagem"}`.

| Método | Rota | Descrição |
|---|---|---|
| GET | `/api/health` | healthcheck |
| GET | `/api/events` | eventos futuros, ordenados por data |
| GET | `/api/events/{idOrSlug}` | detalhe + assentos (aceita slug ou id numérico) |
| GET | `/api/search?keyword=&startDate=&endDate=` | busca |
| POST | `/api/reserve` | segura assentos (hold) e cria booking PENDING |
| POST | `/api/booking` | confirma (simula pagamento) um booking |
| GET | `/api/booking/{id}` | consulta um booking |

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

## GET /api/search
| Param | Formato | Observação |
|---|---|---|
| `keyword` | texto | casa com nome do evento, nome do local ou cidade; ignora acento e caixa |
| `startDate` | `YYYY-MM-DD` | inclusivo, fuso America/Sao_Paulo |
| `endDate` | `YYYY-MM-DD` | inclusivo |

Todos opcionais. 400 se data inválida ou `endDate < startDate`. Exemplo:
`/api/search?keyword=rio&startDate=2026-12-01&endDate=2027-12-31`

## POST /api/reserve
```json
{"event_id": 1, "ticket_ids": [7, 8], "customer_name": "Ana", "customer_email": "ana@example.com"}
```
- **201** → booking (`status: "PENDING"`, `expires_at`, `seconds_remaining`, `seats`, `ticket_ids`, `total_cents`)
  - `seconds_remaining` é calculado pelo relógio do banco; o frontend usa este campo no countdown
    (e não `expires_at`) para não depender do relógio do navegador estar sincronizado com o servidor.
- **400** → validação (1 a 9 assentos, ids duplicados, ticket de outro evento, e-mail inválido)
- **409** → assento ocupado: `{"error": "...", "unavailable_ticket_ids": [8]}`

## POST /api/booking
```json
{"booking_id": "26d8bb8e-0fb7-4b3d-a6ce-6dec58e56ed2"}
```
- **200** → booking `CONFIRMED` (idempotente: confirmar de novo devolve 200)
- **404** → booking não existe
- **410** → reserva expirou

## Exemplos com curl
```bash
curl localhost:8080/api/events/jota-quest-rio-de-janeiro
curl -X POST localhost:8080/api/reserve -d '{"event_id":1,"ticket_ids":[1],"customer_name":"Ana","customer_email":"ana@x.com"}'
curl -X POST localhost:8080/api/booking -d '{"booking_id":"<id>"}'
```
