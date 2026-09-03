# Velora backend

Go API behind the Velora clients — the web app (`velora_web`) and the Flutter
app (`Dating_Frontend`). A modular monolith: one binary, three modes.

```
velora serve     HTTP + WebSocket API
velora work      background jobs (photos, daily picks, push)
velora migrate   apply database migrations, then exit
```

## Run it

```bash
cp .env.example .env
docker compose -f deploy/docker-compose.yml up -d postgres
make run
curl localhost:8080/healthz
```

Or the whole stack in Docker:

```bash
make up
```

## Layout

```
cmd/velora/            entry point; serve / work / migrate
internal/
  domain/              shared shapes — stdlib only
  compat/              compatibility engine — stdlib + domain only
  config/              environment parsing and validation
  httpx/               problem-details errors, middleware
  db/migrations/       goose migrations
deploy/                Dockerfile, Compose, Caddyfile
```

Packages depend inward. `compat` imports nothing but `domain` and the standard
library, and that rule is what keeps the scoring engine testable — no database,
no HTTP, no clock.

## The compatibility engine

`internal/compat` is a port of `CompatibilityService` from the Flutter client.
Six weighted sub-scores — personality, lifestyle, interests, relationship
intent, communication, location — plus the human-readable insight strings the
UI renders as "why this match".

Determinism is the product claim, so the port is verified against **golden
vectors generated from the Dart implementation**, not re-derived by hand.

```bash
go test ./internal/compat/ -v
```

`TestMatchesDartGoldens` currently **skips**: it needs
`internal/compat/testdata/vectors.json`. Generate it from the Flutter repo
(15 profiles → 210 ordered pairs), commit it, and the test starts enforcing
byte-for-byte agreement on every sub-score, the overall, the shared-interest
list, and the generated copy. Only once it passes should the client-side
scorers be deleted.

If Go and Dart disagree, Go is wrong. Never edit an expectation to go green.

## Conventions

- **Errors**: every failure is an RFC 9457 problem document (`httpx.Problem`).
  Non-`Problem` errors are logged in full and returned as a generic 500, so
  database messages never reach a client.
- **Enums** are stored as `text` with a check constraint, and the values are the
  exact Dart enum names — the JSON wire format is identical across all three
  codebases.
- **Timestamps** are `timestamptz`, serialized ISO-8601.
- **Config** is read once at startup via `config.Load()`. Nothing below reaches
  for `os.Getenv` on its own, and secrets have no defaults in production.

## Status

Phase 0 (foundations) and the compatibility port are done. Still to build,
in order: identity and profile, discovery, chat and presence, safety.
See the backend blueprint for phase exit criteria.
