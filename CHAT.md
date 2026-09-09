# Direct chat implementation

Implemented for the Go backend and React web client. This is a local implementation, not a deployment. The existing `0006_messages.sql` draft is preserved; `0007_chat_runtime.sql` adds runtime support and separates dating, social and anonymous consent contexts.

## What works

- Dating conversations open when a mutual match is created. Existing matches can open a conversation through the API.
- Social introductions permit one text message until the recipient accepts. A decline stays closed; repeated introductions cannot bypass it.
- Anonymous pairing requires both people to opt in, active adult profiles and compatible age/gender preferences. Names and profile IDs stay masked until both participants consent.
- Text messages, replies, paginated history, unread counts, delivered/read cursors, typing, mute/archive, conversation-level blocking and message reports.
- Sends and their per-user events commit in one Postgres transaction. River push jobs join that transaction when push is configured.
- Sender and recipient devices receive metadata invalidations over WebSocket. Current permission checks protect every subsequent content fetch. Missed notifications recover from the database.

## Run

Set `DATABASE_URL`, a stable `TOKEN_SIGNING_KEY` and the exact web `ALLOWED_ORIGINS`. Then run:

```sh
go run ./cmd/velora migrate
go run ./cmd/velora serve
# Separate process, same database; needed for configured browser push:
go run ./cmd/velora work
```

`migrate` applies Goose through version 7 and River's own schema migrations. Run migrations before starting either process. Set `NEXT_PUBLIC_API_URL` in the React environment before starting/building the web app. It may be a direct API origin or a same-origin `/api` prefix.

Optional push requires `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY` and `VAPID_SUBJECT` in both processes. Use a `mailto:` contact or HTTPS contact as the subject. The browser must grant notification permission on HTTPS or localhost. Empty settings leave direct chat functional and disable push enrollment/enqueueing. No external push was sent during verification.

For nginx, adapt [deploy/nginx-chat.conf.example](deploy/nginx-chat.conf.example) to the existing server block. Caddy's existing reverse proxy supports upgrades. Configure TLS for the public web/API origin.

## Contract and recovery

The API explorer/OpenAPI document includes the chat routes. The two cursors have different scopes:

| Value | Scope | Use |
| --- | --- | --- |
| Message `seq` | One conversation | History pagination, gap recovery, receipts |
| Event `cursor` | One signed-in user | Changes across that user's conversations |
| `clientMessageId` | Sender + conversation | Retrying the same send without duplicating it |

All bigint positions are JSON decimal strings. Keep them as strings/BigInt in JavaScript.

1. `GET /chat/bootstrap` returns conversation summaries, a matching event cursor and the total unread count. Follow `nextOffset` for additional inbox pages.
2. `POST /chat/realtime-ticket` authenticates with Bearer and returns a single-use ticket valid for 30 seconds.
3. Open `/chat/ws` and send `{"ticket":"...","after":"cursor"}` within 5 seconds. Credentials never appear in the socket URL.
4. `POST /conversations/{id}/messages` takes `{"clientMessageId":"uuid","body":"Hello","kind":"text"}` and returns the saved message. The socket may arrive before or after this response.
5. A socket frame `{"type":"events","data":{"events":[...],"cursor":"...","hasMore":false}}` invalidates the inbox/history. Frames carry metadata, not message bodies or peer identities.
6. On a reconnect/gap, fetch `/chat/events?after=cursor`, then `/conversations/{id}/messages?afterSeq=seq`. Follow pages until `hasMore` is false. Merge by message ID.
7. Read/delivered writes contain `{"seq":"42"}`. Read is advanced by the web client only for the visible, focused conversation at the bottom of its history.

The hub uses a dedicated LISTEN connection and a five-second fallback wake-up. The web client also reconciles on foreground/online events and every 15 seconds while visible. Access-token expiry closes the socket; reconnect obtains a fresh ticket. Web Locks coordinate refresh-token rotation between tabs on HTTPS/localhost.

## Notification semantics

River claims/retries jobs. Five seconds after enqueue, the worker rechecks unread state, mute, account status, blocks, conversation state and the dating match. A live socket alone does not suppress push: having a connection is not proof the person saw a message. This keeps `serve` and `work` independent of in-memory presence.

Provider delivery is at least once. Jobs are unique by message/recipient/subscription, and browser notifications use a stable message tag. A crash after provider acceptance can still cause another attempt. Notifications contain generic copy; HTTP 404/410 removes expired subscriptions. Supported provider hosts are explicitly allowlisted and redirects are disabled.

## Verification

Use a dedicated database whose name contains `velora_chat_test`. Migrate it before running integration tests; never use a production database.

```sh
DATABASE_URL="$CHAT_TEST_DATABASE_URL" go run ./cmd/velora migrate
go test ./...
go vet ./...
CHAT_LOAD_TEST=1 go test ./internal/chat -run TestLoad500Connections -count=1 -v
```

Set `CHAT_TEST_DATABASE_URL` in the environment for the database tests; without it those tests skip. The load test additionally requires `CHAT_LOAD_TEST=1`.

Verified locally on PostgreSQL 17:

- Concurrent same-ID retries, altered-payload rejection, membership checks, receipts, history pagination, event recovery, request acceptance, cross-thread reply rejection, mutual reveal, anonymous queue, block/unmatch and send/block concurrency.
- Actual WebSocket upgrade through the logging wrapper, a surviving hijacked write deadline, ticket reuse rejection and Origin rejection.
- River enqueue rollback/commit and worker suppression, generic payload, retryable provider error and expired subscription cleanup. Provider calls were replaced with a recorder.
- Two Chrome contexts: messages both directions, replies, offline send, reload, same-ID retry, no duplicates and no page errors. Desktop/mobile layouts inspected.
- Two same-account tabs: concurrent expired-access responses triggered one refresh-token rotation; both sockets connected, and cross-tab logout cleared chat. TypeScript, chat-component lint and the Next.js production build passed.
- 500 distinct authenticated sockets, 50 simultaneous HTTP sends and all 100 sender/recipient message events: local HTTP acknowledgement p95 about **57 ms**, burst-to-event p95 about **62 ms**. This is a short capacity smoke test without WAN/TLS, full browser refetch load or sustained traffic; it is not a production throughput guarantee.

## Remaining boundaries

Text-only direct conversations ship here. Private attachments, voice notes, groups, calls, edits/deletion, reactions and user-facing online presence are later work. Typing stays inside one `serve` instance; durable delivery supports multiple instances through Postgres. Rate limits are per process.

The event log currently has no retention pruning. Add a snapshot/resync contract before pruning old events. Inbox pagination uses offsets; rapidly changing ordering can shift page boundaries, with later refresh reconciling the loaded pages. Failed sends and drafts use per-account sessionStorage, survive reload/navigation in that tab and are cleared on logout; they do not survive closing the tab. At most 20 unconfirmed sends are retained per thread. Anonymous queues expire after 30 seconds without renewal.

Blocked/unmatched conversations stay closed after unblock; reopening old conversations is not implemented. Social requests do not yet expire/cancel automatically. Reverting migration 7 fails safely if multiple consent contexts already share a pair; resolve those records before rollback.
