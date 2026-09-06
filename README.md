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

## Auth

No passwords was the plan; a domain got in the way. Google rejects a bare IP
as an authorized origin and the test server has no domain yet, so
**email + password is the interim** and the Google path sits ready but
unconfigured.

```
POST /auth/register   {email, password}   → 201 + session
POST /auth/login      {email, password}   → 200 + session
POST /auth/social     {provider, idToken} → dormant until GOOGLE_CLIENT_IDS is set
POST /auth/refresh    {refreshToken}      → rotated pair
POST /auth/logout     {refreshToken}      → 204
GET  /me              Bearer <token>      → user + profile (profile null until onboarding)
```

Three decisions worth not undoing:

- **Login runs a bcrypt comparison even when the email is unknown.** Returning
  early makes "no such account" measurably faster than "wrong password", which
  is enough to enumerate users. Both paths also return identical text.
- **Passwords over 72 bytes are rejected**, not truncated. bcrypt silently
  ignores the remainder, so accepting them means a password whose tail does
  nothing while the user believes otherwise.
- **Refresh reuse revokes the whole family.** Presenting a spent token logs out
  the real holder and the thief alike — correct when you cannot tell them apart.

Google lookups key on the `sub` claim, not the email: an admin can reassign an
email, but the subject is permanent. The email is stored as its own identity
row so a later email login resolves to the same user instead of duplicating it.

## Photos

`POST /me/photos` takes a multipart `photo` and returns every variant URL.
Also `GET /me/photos`, `DELETE /me/photos/{id}`, `PATCH /me/photos/order`.

Each upload becomes three JPEGs — 1080 fit, 480 and 160 square — because
serving a full-size image into a 160px avatar is the usual reason a photo feed
feels slow. A thumbnail costs about 5 KB against 228 KB for the original.

**EXIF orientation is applied before resizing**, or every portrait comes out
sideways; re-encoding then drops all metadata, which is how EXIF gets
stripped. That is a safety requirement, not housekeeping — phone photos carry
GPS. Original bytes are never copied through.

Keys are content-addressed, so identical bytes always land on the same key and
an edit is a new key. That is what makes `immutable` caching safe.

Storage sits behind `media.Store`. `LocalStore` writes to disk for nginx to
serve; **R2 is the intended destination** but needs a custom domain to reach
Cloudflare's edge — `r2.dev` is rate-limited and explicitly not for production.
Swapping is one implementation plus `MEDIA_ROOT`/`MEDIA_BASE_URL`.

## Test server

**http://89.167.77.99:8091** — `ssh root@89.167.77.99`, key auth.

```
/        → 127.0.0.1:3001   velora-web   (workerd)
/api/    → 127.0.0.1:8080   velora-api   (this binary)
/media/  → /var/www/velora-media
```

Both under PM2 from `/var/www/velora-ecosystem.config.cjs` (mode 600 — holds
the DB password and signing key). Deploy:

```bash
cd /var/www/velora-api && git pull
/usr/local/go1.26/bin/go build -trimpath -ldflags="-s -w" -o bin/velora ./cmd/velora
set -a && . ./.env && set +a && ./bin/velora migrate
pm2 delete velora-api && pm2 start /var/www/velora-ecosystem.config.cjs --only velora-api && pm2 save
```

`pm2 restart --update-env` re-reads the **shell** environment, not the
ecosystem file — always delete and start. Go 1.26 is isolated at
`/usr/local/go1.26` because five unrelated apps share the box on system Go 1.21.

Plain HTTP, so treat everything on it as visible on the wire, and rotate
`TOKEN_SIGNING_KEY` when TLS arrives.

## Status

Done: Phase 0 foundations, the compatibility port, and Phase 1 auth plus photo
storage. Schema at version 3.

Next, in order:

1. **Profile endpoints** — `PATCH /me`, an onboarding write, a real `profile`
   in `/me`. The tables exist; this is handlers and queries.
2. **Golden vectors** — `TestMatchesDartGoldens` still skips. Until it is green
   the Go scorer is unverified against Dart and the client-side scorers must
   stay.
3. **Discovery** — candidate query plus the ported ranking.
4. **Chat and presence**, then **safety**.

Blocked on a domain: Google sign-in, and moving photos to R2 behind a CDN.
