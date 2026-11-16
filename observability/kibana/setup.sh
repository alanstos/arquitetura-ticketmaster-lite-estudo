#!/bin/sh
# Cria a data view "events" (alias do índice de busca, ADR-013) no Kibana.
# Idempotente: override=true sobrescreve se já existir. Roda no container kibana-setup.
set -eu

KIBANA_URL="${KIBANA_URL:-http://kibana:5601}"

# Sem timeFieldName de propósito: os eventos estão no futuro e o filtro padrão
# do Discover ("últimos 15 minutos") esconderia todos.
curl -fsS -X POST "$KIBANA_URL/api/data_views/data_view" \
  -H 'kbn-xsrf: true' -H 'Content-Type: application/json' \
  -d '{"override": true, "data_view": {"id": "events", "title": "events", "name": "events (busca)"}}' \
  >/dev/null

echo "Data view 'events' pronta."
echo "Discover:  http://localhost:5601/app/discover"
echo "Dev Tools: http://localhost:5601/app/dev_tools#/console"
