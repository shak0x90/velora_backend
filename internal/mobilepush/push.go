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
	"log/slog"
	"time"

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

	var platform, token string
	err := pool.QueryRow(ctx, `
		select d.platform, d.token from devices d join users u on u.id = d.user_id
		where d.id = $1 and d.user_id = $2 and u.status = 'active'
	`, a.DeviceID, a.UserID).Scan(&platform, &token)
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
		return nil
	}
	if until, quiet := prefs.QuietUntil(w.q.now()); quiet {
		return river.JobSnooze(until.Sub(w.q.now()))
	}

	data := a.Data
	if a.Event == EventMessage {
		cid, deliver, err := deliverableMessage(ctx, pool, a.MessageID, a.UserID)
		if err != nil {
			return err
		}
		if !deliver {
			return nil
		}
		data = copyWith(data, "conversationId", cid)
		a.Collapse = "chat-" + cid
	}

	err = sender.Send(ctx, Message{Token: token, Title: a.Title, Body: a.Body, Data: data, Collapse: a.Collapse})
	switch {
	case err == nil:
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
func deliverableMessage(ctx context.Context, pool *pgxpool.Pool, messageID, recipientID string) (string, bool, error) {
	var cid string
	err := pool.QueryRow(ctx, `
		select c.id::text
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
	`, messageID, recipientID).Scan(&cid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return cid, err == nil, err
}

func copyWith(in map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	out[key] = value
	return out
}
