.PHONY: up down logs reset loadtest go-check fe-check psql obs

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f backend

# Apaga o banco (volume) e sobe de novo, reaplicando migrations + seed.
reset:
	docker compose down -v
	docker compose up -d --build

loadtest:
	docker compose exec backend /app/loadtest -n 50

# Observabilidade (ADR-011): Grafana com dashboard, traces (Tempo) e logs (Loki).
obs:
	@echo "Grafana:    http://localhost:3000/d/ticketmaster-lite"
	@echo "Explore:    http://localhost:3000/explore  (Tempo = traces, Loki = logs, Prometheus = métricas)"
	@echo "Readiness:  http://localhost:8080/api/health/ready"

psql:
	docker compose exec postgres psql -U ticket -d ticketmaster

go-check:
	docker run --rm -v "$(PWD)/backend":/src -w /src -u $$(id -u):$$(id -g) \
		-e GOCACHE=/tmp/gocache -e GOPATH=/tmp/gopath golang:1.23-alpine \
		sh -c 'test -z "$$(gofmt -l .)" && go vet ./... && go build ./...'

fe-check:
	docker run --rm -v "$(PWD)/frontend":/app -w /app -u $$(id -u):$$(id -g) \
		-e npm_config_cache=/tmp/npmcache node:20-alpine sh -c 'npm ci && npm run build && rm -rf dist'
