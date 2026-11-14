// Package mobilepush delivers native push notifications (FCM, APNs) to the
// phones people registered with POST /devices.
//
// Deciding and sending are split across the transaction boundary on purpose.
// Enqueue runs inside the transaction that created the event (a message, a
// like, a match) and writes River jobs there, so a rolled-back message never
// pushes and a committed one always does. The worker then re-checks
// everything that could have changed in between — the device, the person's
// preferences, whether the message was already read — before it calls a
// provider.
package mobilepush

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/shak0x90/velora_backend/internal/settings"
)

const (
	PlatformAndroid = "android"
	PlatformIOS     = "ios"
)

// QueueName keeps these jobs away from the chat web-push client, which works
// the default queue and would otherwise pick up a kind it has no worker for.
const QueueName = "mobile_push"

// Events that produce a push, and the preference that gates each.
const (
	EventMessage   = "message"
	EventMatch     = "match"
	EventLike      = "like"
	EventDailyEdit = "dailyEdit"
	EventWeekly    = "weekly"
	EventBlindHour = "blindHour"
	// EventBlindHello tells a blind hello's author that someone was paired
	// with it; it follows the messages switch.
	EventBlindHello = "blindHello"
)

// Allowed reports whether s lets event through. Likes ride on the matches
// switch: the settings screen offers them as one choice.
func Allowed(s settings.Settings, event string) bool {
	switch event {
	case EventMessage:
		return s.Notify.Messages
	case EventMatch, EventLike:
		return s.Notify.Matches
	case EventDailyEdit:
		return s.Notify.DailyEdit
	case EventWeekly:
		return s.Notify.Weekly
	case EventBlindHour:
		return s.Notify.BlindHour
	case EventBlindHello:
		return s.Notify.Messages
	default:
		return false
	}
}

// Args is one push to one device.
type Args struct {
	DeviceID  string            `json:"deviceId"`
	UserID    string            `json:"userId"`
	Event     string            `json:"event"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Data      map[string]string `json:"data,omitempty"`
	MessageID string            `json:"messageId,omitempty"`
	Collapse  string            `json:"collapse,omitempty"`
}

func (Args) Kind() string { return "mobile_push" }

func (Args) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueName, MaxAttempts: 8}
}

// Note is what a caller wants to say; Enqueue fans it out to devices.
type Note struct {
	Event     string
	Title     string
	Body      string
	Data      map[string]string
	MessageID string
	Collapse  string
}

type Queue struct {
	client *river.Client[pgx.Tx]
	pool   *pgxpool.Pool
	router Router
	now    func() time.Time
}

// New builds the queue. With work set it also runs the worker on QueueName;
// the API process inserts only.
func New(pool *pgxpool.Pool, router Router, work bool) (*Queue, error) {
	q := &Queue{pool: pool, router: router, now: time.Now}
	cfg := &river.Config{}
	if work {
		workers := river.NewWorkers()
		river.AddWorker(workers, &Worker{q: q})
		cfg.Workers = workers
		cfg.Queues = map[string]river.QueueConfig{QueueName: {MaxWorkers: 5}}
	}
	client, err := river.NewClient(riverpgxv5.New(pool), cfg)
	q.client = client
	return q, err
}

func (q *Queue) Start(ctx context.Context) error { return q.client.Start(ctx) }
func (q *Queue) Stop(ctx context.Context) error  { return q.client.Stop(ctx) }

// messageDelay gives a reader on another device a moment to open the thread,
// which the worker's unread check then turns into no push at all.
const messageDelay = 5 * time.Second

// Enqueue schedules note for every device userID has registered on a
// configured platform, inside tx. Turned-off categories enqueue nothing;
// quiet hours delay the job to the end of the window rather than dropping it.
// EnqueueLooking tells someone that a compatible person is looking for a
// blind chat right now. Same switch as the Blind hour reminder, and like it
// skipped (not delayed) during quiet hours.
func (q *Queue) EnqueueLooking(ctx context.Context, tx pgx.Tx, userID string) error {
	return q.Enqueue(ctx, tx, userID, Note{
		Event:    EventBlindHour,
		Title:    "Someone's looking for a blind chat 🎭",
		Body:     "Tap to talk now. No photos, no names, until you both say yes.",
		Data:     map[string]string{"type": "blindLooking"},
		Collapse: "blind-looking",
	})
}

// EnqueueHelloAnswered tells a blind hello's author their blind chat began.
func (q *Queue) EnqueueHelloAnswered(ctx context.Context, tx pgx.Tx, userID, conversationID string) error {
	return q.Enqueue(ctx, tx, userID, Note{
		Event: EventBlindHello,
		Title: "Someone answered your blind hello 🎭",
		Body:  "Your blind chat has started.",
		Data:  map[string]string{"type": "blindHello", "conversationId": conversationID},
	})
}

// EnqueueBlindHour queues the daily "Blind hour is open" reminder; tapping
// it opens anonymous chat. Satisfies blindhour.Notifier.
func (q *Queue) EnqueueBlindHour(ctx context.Context, tx pgx.Tx, userID string) error {
	return q.Enqueue(ctx, tx, userID, Note{
		Event:    EventBlindHour,
		Title:    "Blind hour is open 🎭",
		Body:     "Talk first, see each other later. People are looking for a blind chat now.",
		Data:     map[string]string{"type": "blindHour"},
		Collapse: "blind-hour",
	})
}

func (q *Queue) Enqueue(ctx context.Context, tx pgx.Tx, userID string, note Note) error {
	if q == nil || !q.router.Any() {
		return nil
	}
	prefs, err := settings.Load(ctx, tx, userID)
	if err != nil {
		return err
	}
	if !Allowed(prefs, note.Event) {
		return nil
	}
	rows, err := tx.Query(ctx, `select id::text, platform from devices where user_id = $1`, userID)
	if err != nil {
		return err
	}
	type device struct{ id, platform string }
	var devices []device
	for rows.Next() {
		var d device
		if err = rows.Scan(&d.id, &d.platform); err != nil {
			rows.Close()
			return err
		}
		devices = append(devices, d)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}

	at := q.now()
	if note.Event == EventMessage {
		at = at.Add(messageDelay)
	}
	if until, quiet := prefs.QuietUntil(at); quiet {
		at = until
	}
	for _, d := range devices {
		if _, ok := q.router.For(d.platform); !ok {
			continue
		}
		args := Args{
			DeviceID: d.id, UserID: userID, Event: note.Event, Title: note.Title, Body: note.Body,
			Data: note.Data, MessageID: note.MessageID, Collapse: note.Collapse,
		}
		opts := &river.InsertOpts{ScheduledAt: at}
		if note.MessageID != "" {
			// A retried send must not push the same message twice.
			opts.UniqueOpts = river.UniqueOpts{ByArgs: true}
		}
		if _, err = q.client.InsertTx(ctx, tx, args, opts); err != nil {
			return err
		}
	}
	return nil
}

// MessageHook adapts Enqueue to the chat service's hook. The text is generic
// on purpose: a lock screen is not private, and the app shows the message
// once it is opened.
func (q *Queue) MessageHook(ctx context.Context, tx pgx.Tx, messageID, recipientID string, _ int64) error {
	return q.Enqueue(ctx, tx, recipientID, Note{
		Event: EventMessage, Title: "New message", Body: "You have a new message on Thapaw.",
		Data: map[string]string{"type": EventMessage, "messageId": messageID}, MessageID: messageID,
	})
}

// SocialHook adapts Enqueue to the social service's notification hook: it
// receives the same title and body as the in-app notification row.
func (q *Queue) SocialHook(ctx context.Context, tx pgx.Tx, userID, kind, title, body, aboutID string) error {
	var event string
	switch kind {
	case "match":
		event = EventMatch
	case "like":
		event = EventLike
	default:
		return nil
	}
	data := map[string]string{"type": event}
	if aboutID != "" {
		data["profileId"] = aboutID
	}
	return q.Enqueue(ctx, tx, userID, Note{Event: event, Title: title, Body: body, Data: data, Collapse: event})
}

// ---------------------------------------------------------------------------
// Worker
// ---------------------------------------------------------------------------

type Worker struct {
	river.WorkerDefaults[Args]
	q *Queue
}

func (w *Worker) Work(ctx context.Context, job *river.Job[Args]) error {
	a := job.Args
	pool := w.q.pool

	var platform, token, appVersion string
	err := pool.QueryRow(ctx, `
		select d.platform, d.token, d.app_version from devices d join users u on u.id = d.user_id
		where d.id = $1 and d.user_id = $2 and u.status = 'active'
	`, a.DeviceID, a.UserID).Scan(&platform, &token, &appVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // unregistered, moved to another account, or account gone
	}
	if err != nil {
		return err
	}
	sender, ok := w.q.router.For(platform)
	if !ok {
		return nil // provider switched off since enqueue
	}

	prefs, err := settings.Load(ctx, pool, a.UserID)
	if err != nil {
		return err
	}
	if !Allowed(prefs, a.Event) {
		slog.Info("push skipped", "device", a.DeviceID, "event", a.Event, "reason", "turned off in settings")
		return nil
	}
	if until, quiet := prefs.QuietUntil(w.q.now()); quiet {
		if a.Event == EventBlindHour {
			// "It's open now" delivered after quiet hours would be wrong.
			slog.Info("push skipped", "device", a.DeviceID, "event", a.Event, "reason", "quiet hours")
			return nil
		}
		return river.JobSnooze(until.Sub(w.q.now()))
	}

	data := a.Data
	if a.Event == EventMessage {
		cid, unread, deliver, err := deliverableMessage(ctx, pool, a.MessageID, a.UserID)
		if err != nil {
			return err
		}
		if !deliver {
			slog.Info("push skipped", "device", a.DeviceID, "event", a.Event, "reason", "already read, muted or chat closed")
			return nil
		}
		data = copyWith(data, "conversationId", cid)
		a.Collapse = "chat-" + cid
		a.Body = messageBody(unread)
	}

	// Apps that report their version also confirm receipt (POST /push/ack).
	tracked := appVersion != ""
	_, err = SendTracked(ctx, pool, sender, a.DeviceID, tracked,
		Message{Token: token, Title: a.Title, Body: a.Body, Data: data, Collapse: a.Collapse})
	switch {
	case err == nil:
		slog.Info("push sent", "device", a.DeviceID, "event", a.Event)
		_, _ = pool.Exec(ctx, `update devices set last_seen_at = now() where id = $1`, a.DeviceID)
		return nil
	case errors.Is(err, ErrUnregistered):
		// Token-scoped, so a token re-registered under a new row survives.
		_, err = pool.Exec(ctx, `delete from devices where id = $1 and token = $2`, a.DeviceID, token)
		return err
	case errors.Is(err, ErrPermanent):
		slog.Warn("push rejected", "device", a.DeviceID, "event", a.Event, "error", err)
		return river.JobCancel(err)
	default:
		return err
	}
}

// deliverableMessage is the chat web-push suppression rule, for native
// devices: still unread by the recipient, thread not muted, message not
// deleted, conversation live, both people active, no block either way, and a
// match conversation still backed by a match.
// SendTracked hands m to sender. When tracked, it first records a receipt and
// adds its id to the push data as "pushId"; the app confirms it with
// POST /push/ack, and receipts left unconfirmed mean the phone dropped the
// push. Returns the receipt id ("" when untracked or on failure).
func SendTracked(ctx context.Context, pool *pgxpool.Pool, sender Sender, deviceID string, tracked bool, m Message) (string, error) {
	if !tracked {
		return "", sender.Send(ctx, m)
	}
	id := uuid.NewString()
	m.Data = copyWith(m.Data, "pushId", id)
	if _, err := pool.Exec(ctx, `insert into push_receipts (id, device_id) values ($1, $2)`, id, deviceID); err != nil {
		return "", err
	}
	if err := sender.Send(ctx, m); err != nil {
		_, _ = pool.Exec(ctx, `delete from push_receipts where id = $1`, id)
		return "", err
	}
	_, _ = pool.Exec(ctx, `delete from push_receipts where device_id = $1 and sent_at < now() - interval '14 days'`, deviceID)
	return id, nil
}

func deliverableMessage(ctx context.Context, pool *pgxpool.Pool, messageID, recipientID string) (string, int, bool, error) {
	var cid string
	var unread int
	err := pool.QueryRow(ctx, `
		select c.id::text,
		       (select count(*) from messages u
		         where u.conversation_id = c.id and u.seq > p.last_read_seq
		           and u.sender_id <> $2 and u.deleted_at is null)
		from messages m
		join conversations c on c.id = m.conversation_id
		join conversation_participants p on p.conversation_id = c.id and p.user_id = $2
		where m.id = $1 and p.last_read_seq < m.seq and not p.muted
		  and m.deleted_at is null and c.state in ('open', 'pending')
		  and (select count(*) = 2 from users where id in (c.user_a, c.user_b) and status = 'active')
		  and not exists (select 1 from blocks
		                  where (user_id = c.user_a and blocked_id = c.user_b)
		                     or (user_id = c.user_b and blocked_id = c.user_a))
		  and (c.origin <> 'match'
		       or exists (select 1 from matches where user_a = c.user_a and user_b = c.user_b))
	`, messageID, recipientID).Scan(&cid, &unread)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, false, nil
	}
	return cid, unread, err == nil, err
}

// messageBody is the lock-screen text for a chat push. Each conversation's
// push replaces the one before it (same tag), so it counts the unread
// messages instead of stacking one identical notification per message.
func messageBody(unread int) string {
	if unread > 1 {
		return fmt.Sprintf("You have %d new messages on Thapaw.", unread)
	}
	return "You have a new message on Thapaw."
}

func copyWith(in map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	out[key] = value
	return out
}
