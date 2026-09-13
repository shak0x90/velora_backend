.PHONY: help run work test cover lint build up down logs migrate tidy apitest

help:
	@echo "run      Start the API locally (reads .env)"
	@echo "work     Start the background worker"
	@echo "test     Run all unit tests"
	@echo "apitest  Run the API checks against a running server"
	@echo "cover    Run tests with a coverage summary"
	@echo "lint     go vet + gofmt check"
	@echo "build    Build ./bin/velora"
	@echo "up       Start Postgres + api + worker + Caddy in Docker"
	@echo "down     Stop the Docker stack"
	@echo "migrate  Apply database migrations"

# `set -a` exports everything in .env so the process inherits it.
run:
	@set -a && . ./.env && set +a && go run ./cmd/velora serve

work:
	@set -a && . ./.env && set +a && go run ./cmd/velora work

migrate:
	@set -a && . ./.env && set +a && go run ./cmd/velora migrate

# -p 1 runs one package at a time. The chat and chatpush integration tests
# share a single CHAT_TEST_DATABASE_URL and truncate as they go, so letting Go
# run those two packages concurrently makes them fail each other at random —
# which reads as a flaky feature rather than a test harness overlapping itself.
test:
	go test -p 1 ./...

# Against a live server, so it needs one running. VELORA_API_URL overrides the
# default of http://127.0.0.1:8080 — on the box, that default is what you want.
apitest:
	go run ./cmd/apitest suite $(SUITE)

cover:
	go test -p 1 -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

lint:
	go vet ./...
	@test -z "$$(gofmt -l . )" || (echo "gofmt needed:"; gofmt -l .; exit 1)

build:
	go build -trimpath -o bin/velora ./cmd/velora
	go build -trimpath -o bin/apitest ./cmd/apitest

up:
	docker compose -f deploy/docker-compose.yml up -d --build

down:
	docker compose -f deploy/docker-compose.yml down

logs:
	docker compose -f deploy/docker-compose.yml logs -f api worker

tidy:
	go mod tidy
