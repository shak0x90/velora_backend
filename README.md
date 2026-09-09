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
  auth/                identity, tokens, RequireAuth
  media/               photo upload, variants, object keys
  profile/             the /me surface, and every profile read
  discovery/           feed, daily picks, search, profile detail
  social/              passes, likes, matches, notifications
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

## Profile

```
GET   /me                 → user + profile + completion (both null pre-onboarding)
PATCH /me                 → the updated profile
POST  /me/onboarding      → creates the row; firstName, dateOfBirth, gender required
```

`PATCH /me` takes a partial profile where every field is optional, so absent
and cleared are different requests. Validation runs before any column is
written, which is what makes a rejected patch leave the row untouched.
Server-derived fields (`age`, `distanceKm`, `photoVerified`, …) are accepted
and ignored, because both clients type their patch as a partial `Profile`.

`photos` in a patch is a reconciliation: the listed photos take that order and
anything left out is deleted. A URL matching none of the caller's photos
rejects the whole request rather than being skipped — on a stale list, quietly
ignoring it would delete every photo the client did not know about.

Reads are batched. One feed of candidates costs five queries, not five per
person, and every profile in the service is assembled by `profile.query` so
there is a single definition of what a profile is.

## Discovery

```
GET /discover?filters=<json>       → ranked profiles
GET /discover/daily-picks          → the top five
GET /search?q=                     → ranked text matches
GET /profiles?ids=a,b,c            → batch lookup for likes and matches screens
GET /profiles/{id}                 → profile + compatibility
GET /profiles/{id}/suggestions     → conversation openers
```

**The database narrows, Go ranks.** SQL excludes only who cannot be shown at
all — yourself, hidden profiles, anyone you already passed, liked or matched —
because those are index lookups against sets that grow with use. Everything
after that runs through `compat`, so the server and both clients score
identically rather than half-expressing the rules in SQL.

Distance is computed in SQL via `earthdistance` so the GiST index is usable.
Unknown coordinates score 0 rather than null: an unplaced profile should still
be visible, and a distance filter it could never satisfy would hide it forever.

Search deliberately ignores the saved filters. Someone typing a name is looking
for that person, not for whoever survives their age range — but their stated
gender preference still holds, because that is a preference rather than a
filter to widen.

## Likes, matches, notifications

```
POST   /passes/{id}
POST   /likes                      → { matched, match? }
GET    /likes/incoming | /likes/outgoing
DELETE /likes/{id}
GET    /matches
GET    /notifications
POST   /notifications/{id}/read
```

A match is one row for the pair, not one per side; `check (user_a < user_b)`
is what makes that enforceable, since otherwise `(a,b)` and `(b,a)` both insert
and the same two people match twice. Creating one is a single transaction: the
pair row, the deletion of the two likes it consumed, and both notifications
have to agree, or someone ends up matched while still listed as a pending like.

A like that is not yet mutual notifies the recipient without naming the sender.
Revealing who is waiting is what the likes screen is for.

`lastMessagePreview` and `unreadCount` are present and zero until the messaging
phase, so the match screens degrade to "matched, no chat yet" rather than
failing to render.

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

## Testing the API

`go test ./...` covers the pure logic and touches no database. Everything
interesting — a block that leaks, a race that loses a match, a query that only
misbehaves with real rows — needs a running server, which is what `apitest` is
for.

```bash
make apitest                 # every check, against 127.0.0.1:8080
make apitest SUITE=safety    # just one
./bin/apitest list           # what the suites cover

./bin/apitest call GET /me --as someone@velora.test
./bin/apitest call POST /likes '{"profileId":"..."}' --as someone@velora.test
./bin/apitest login someone@velora.test    # prints a bearer token
```

`call` signs in for you, registering the address if it is new, so poking one
endpoint never starts with copying a token around. `VELORA_API_URL` overrides
the target; the default is loopback, so running it on the box never reaches the
public interface.

Every check reproduces a behaviour rather than asserting a patch is present. A
fix that compiles is not the same as a fix that holds, and several of these
exist because the behaviour was once wrong. The concurrency cases — two
goroutines spending one refresh token, two people liking each other at the same
instant — are the reason this runs over HTTP at all; neither is reachable from
a unit test, and both were broken when first checked.

It creates throwaway `apitest+…@velora.test` accounts and cleans up after
nothing, because the wreckage of a failed run is usually how you find out what
happened. Point it only at a database you would not mind filling with rows
named "Api SFA".

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

Done: Phase 0 foundations, the compatibility port, Phase 1 auth plus photo
storage, the profile surface, and discovery with likes and matches. Schema at
version 4.

**Not yet run against Postgres.** Everything above compiles, vets and passes
its unit tests, but the queries in `profile`, `discovery` and `social` have not
been exercised against a real database. Migrate the test server and walk one
account through sign-up, onboarding, a like and a match before trusting them.

Next, in order:

1. **Golden vectors** — `TestMatchesDartGoldens` still skips. Until it is green
   the Go scorer is unverified against Dart and the client-side scorers must
   stay.
2. **Chat and presence** — conversations, messages, the dual-consent reveal.
   This is the last mocked surface in the web client.
3. **Safety** — reporting, blocking, moderation.

Deferred deliberately: daily picks are recomputed per request rather than
frozen by a scheduled job, and incoming likes are returned without a premium
gate, matching what the clients already do with seed data.

Blocked on a domain: Google sign-in, and moving photos to R2 behind a CDN.
