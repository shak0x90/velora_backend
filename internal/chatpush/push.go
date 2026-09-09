package chatpush

import (
	"context"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

type Config struct{ PublicKey, PrivateKey, Subject string }
type Args struct {
	MessageID      string `json:"messageId"`
	RecipientID    string `json:"recipientId"`
	SubscriptionID string `json:"subscriptionId"`
}

func (Args) Kind() string { return "chat_push" }

type Queue struct {
	client *river.Client[pgx.Tx]
	pool   *pgxpool.Pool
	cfg    Config
	send   func(context.Context, []byte, *webpush.Subscription, *webpush.Options) (*http.Response, error)
}

func New(pool *pgxpool.Pool, cfg Config, work bool) (*Queue, error) {
	q := &Queue{pool: pool, cfg: cfg, send: webpush.SendNotificationWithContext}
	config := &river.Config{}
	if work {
		workers := river.NewWorkers()
		river.AddWorker(workers, &Worker{q: q})
		config.Workers = workers
		config.Queues = map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 5}}
	}
	c, err := river.NewClient(riverpgxv5.New(pool), config)
	q.client = c
	return q, err
}
func (q *Queue) Start(ctx context.Context) error { return q.client.Start(ctx) }
func (q *Queue) Stop(ctx context.Context) error  { return q.client.Stop(ctx) }
func (q *Queue) enabled() bool {
	return q.cfg.PublicKey != "" && q.cfg.PrivateKey != "" && q.cfg.Subject != ""
}
func (q *Queue) Enqueue(ctx context.Context, tx pgx.Tx, message, recipient string, _ int64) error {
	if !q.enabled() {
		return nil
	}
	rows, err := tx.Query(ctx, `select id::text from chat_push_subscriptions where user_id=$1`, recipient)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = q.client.InsertTx(ctx, tx, Args{message, recipient, id}, &river.InsertOpts{MaxAttempts: 8, ScheduledAt: time.Now().Add(5 * time.Second), UniqueOpts: river.UniqueOpts{ByArgs: true}}); err != nil {
			return err
		}
	}
	return nil
}
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	_, err = m.Migrate(ctx, rivermigrate.DirectionUp, nil)
	return err
}

type Worker struct {
	river.WorkerDefaults[Args]
	q *Queue
}

func (w *Worker) Work(ctx context.Context, job *river.Job[Args]) error {
	if !w.q.enabled() {
		return river.JobSnooze(time.Hour)
	}
	var sub webpush.Subscription
	var cid string
	// Socket presence is deliberately not a suppression signal. Only durable state is.
	err := w.q.pool.QueryRow(ctx, `select s.endpoint,s.p256dh,s.auth,c.id::text
 from chat_push_subscriptions s join messages m on m.id=$2 join conversations c on c.id=m.conversation_id
 join conversation_participants p on p.conversation_id=c.id and p.user_id=s.user_id
 where s.id=$1 and s.user_id=$3 and p.last_read_seq<m.seq and not p.muted
 and m.deleted_at is null and c.state in ('open','pending')
 and (select count(*)=2 from users where id in(c.user_a,c.user_b) and status='active')
 and not exists(select 1 from blocks where (user_id=c.user_a and blocked_id=c.user_b) or (user_id=c.user_b and blocked_id=c.user_a))
 and (c.origin<>'match' or exists(select 1 from matches where user_a=c.user_a and user_b=c.user_b))`, job.Args.SubscriptionID, job.Args.MessageID, job.Args.RecipientID).Scan(&sub.Endpoint, &sub.Keys.P256dh, &sub.Keys.Auth, &cid)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !allowedEndpoint(sub.Endpoint) {
		return river.JobCancel(errors.New("unsupported push endpoint"))
	}
	data, _ := json.Marshal(map[string]string{"title": "Velora", "body": "New message on Velora", "tag": "chat-" + job.Args.MessageID, "url": "/messages?c=" + cid})
	resp, err := w.q.send(ctx, data, &sub, &webpush.Options{Subscriber: w.q.cfg.Subject, VAPIDPublicKey: w.q.cfg.PublicKey, VAPIDPrivateKey: w.q.cfg.PrivateKey, TTL: 300, HTTPClient: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}})
	if err != nil {
		return errors.New("push delivery failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 || resp.StatusCode == 410 {
		_, err = w.q.pool.Exec(ctx, `delete from chat_push_subscriptions where id=$1`, job.Args.SubscriptionID)
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("push provider status %d", resp.StatusCode)
}

// Subscription endpoints are user input: never turn this worker into an SSRF proxy.
func allowedEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "fcm.googleapis.com" || host == "updates.push.services.mozilla.com" || host == "web.push.apple.com" || strings.HasSuffix(host, ".notify.windows.com")
}
func (q *Queue) Routes(mux *http.ServeMux, users *auth.Service) {
	mux.Handle("GET /chat/push", users.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := ""
		if q.enabled() {
			key = q.cfg.PublicKey
		}
		httpx.JSON(w, 200, map[string]string{"publicKey": key})
	})))
	mux.Handle("POST /chat/push", users.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !q.enabled() {
			httpx.Error(w, r, httpx.BadRequest("Browser notifications are not configured yet."))
			return
		}
		var sub webpush.Subscription
		if err := httpx.Decode(r, &sub); err != nil {
			httpx.Error(w, r, err)
			return
		}
		authKey, e1 := base64.RawURLEncoding.DecodeString(sub.Keys.Auth)
		pkey, e2 := base64.RawURLEncoding.DecodeString(sub.Keys.P256dh)
		x, _ := elliptic.Unmarshal(elliptic.P256(), pkey)
		if !allowedEndpoint(sub.Endpoint) || len(sub.Endpoint) > 2048 || e1 != nil || e2 != nil || len(authKey) != 16 || x == nil {
			httpx.Error(w, r, httpx.BadRequest("Invalid browser subscription."))
			return
		}
		_, err := q.pool.Exec(r.Context(), `insert into chat_push_subscriptions(id,user_id,endpoint,p256dh,auth) values($1,$2,$3,$4,$5) on conflict(endpoint) do update set user_id=excluded.user_id,p256dh=excluded.p256dh,auth=excluded.auth`, uuid.New(), auth.UserIDFrom(r.Context()), sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.JSON(w, 204, nil)
	})))
	mux.Handle("DELETE /chat/push", users.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Endpoint string `json:"endpoint"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			httpx.Error(w, r, err)
			return
		}
		_, err := q.pool.Exec(r.Context(), `delete from chat_push_subscriptions where user_id=$1 and endpoint=$2`, auth.UserIDFrom(r.Context()), in.Endpoint)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.JSON(w, 204, nil)
	})))
}
