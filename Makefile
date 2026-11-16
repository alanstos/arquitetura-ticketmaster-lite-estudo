.PHONY: up down logs reset loadtest go-check fe-check psql obs es-reindex es-search kibana kibana-down

up:
	docker compose up -d --build

down:
	docker compose --profile tools down

logs:
	docker compose logs -f backend

# Apaga o banco (volume) e sobe de novo, reaplicando migrations + seed.
reset:
	docker compose --profile tools down -v
	docker compose up -d --build

loadtest:
	docker compose exec backend /app/loadtest -n 50

# Observabilidade (ADR-011): Grafana com dashboard, traces (Tempo) e logs (Loki).
obs:
	@echo "Grafana:    http://localhost:3000/d/ticketmaster-lite"
	@echo "Explore:    http://localhost:3000/explore  (Tempo = traces, Loki = logs, Prometheus = métricas)"
	@echo "Readiness:  http://localhost:8080/api/health/ready"
	@echo "Elastic:    http://localhost:9200/events/_count"
	@echo "Kibana:     http://localhost:5601  (só com make kibana)"

# Busca (ADR-013): enfileira todos os eventos no outbox; o indexer reenvia ao Elasticsearch.
es-reindex:
	docker compose exec postgres psql -U ticket -d ticketmaster -c \
		"INSERT INTO search_outbox (event_id) SELECT id FROM events; NOTIFY search_outbox;"

# Ex.: make es-search q=casa
es-search:
	curl -s "http://localhost:8080/api/search?keyword=$(q)" | python3 -m json.tool

# Kibana (ADR-014), sob demanda: UI do Elasticsearch. Leva ~1 min para ficar pronto.
kibana:
	docker compose --profile tools up -d kibana kibana-setup
	@echo "Kibana:     http://localhost:5601  (aguarde ~1 min: docker compose ps kibana)"
	@echo "Dev Tools:  http://localhost:5601/app/dev_tools#/console  (exemplos em observability/kibana/console-exemplos.txt)"
	@echo "Discover:   http://localhost:5601/app/discover  (data view 'events')"

kibana-down:
	docker compose --profile tools rm -sf kibana kibana-setup

psql:
	docker compose exec postgres psql -U ticket -d ticketmaster

go-check:
	docker run --rm -v "$(PWD)/backend":/src -w /src -u $$(id -u):$$(id -g) \
		-e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath golang:1.23-alpine \
		sh -c 'test -z "$$(gofmt -l .)" && go vet ./... && go build ./...'

fe-check:
	docker run --rm -v "$(PWD)/frontend":/app -w /app -u $$(id -u):$$(id -g) \
		-e npm_config_cache=/tmp/npmcache node:20-alpine sh -c 'npm ci && npm run build && rm -rf dist'
