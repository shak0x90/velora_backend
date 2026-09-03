.PHONY: help run work test cover lint build up down logs migrate tidy

help:
	@echo "run      Start the API locally (reads .env)"
	@echo "work     Start the background worker"
	@echo "test     Run all tests"
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

test:
	go test ./...

cover:
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

lint:
	go vet ./...
	@test -z "$$(gofmt -l . )" || (echo "gofmt needed:"; gofmt -l .; exit 1)

build:
	go build -trimpath -o bin/velora ./cmd/velora

up:
	docker compose -f deploy/docker-compose.yml up -d --build

down:
	docker compose -f deploy/docker-compose.yml down

logs:
	docker compose -f deploy/docker-compose.yml logs -f api worker

tidy:
	go mod tidy
