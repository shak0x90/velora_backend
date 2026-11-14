// Package blindhour runs the daily Blind hour: 21:00–22:00 in each person's
// own time zone, when anonymous chat is promoted.
//
// Anonymous chat pairs two people who are looking at the same moment (the
// chat queue holds someone for 30 seconds at a time). With a small community
// most attempts would find nobody, and the feature would feel broken; a
// shared hour concentrates people so the queue actually pairs them. The app
// shows the real number of people looking right now — never an estimate.
package blindhour

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/chat"
	"github.com/shak0x90/velora_backend/internal/httpx"
	"github.com/shak0x90/velora_backend/internal/settings"
)

// The window, in local wall-clock minutes after midnight.
const (
	startMinute = 21 * 60
	length      = time.Hour
	// remindWithin is how late in the hour a reminder may still go out (the
	// worker checks every minute; this covers restarts and slow minutes).
	remindWithin = 10 * time.Minute
)

// Window is today's Blind hour in loc, or tomorrow's once today's has ended.
func Window(now time.Time, loc *time.Location) (start, end time.Time, active bool) {
	local := now.In(loc)
	start = time.Date(local.Year(), local.Month(), local.Day(), startMinute/60, startMinute%60, 0, 0, loc)
	end = start.Add(length)
	if !local.Before(end) {
		start = time.Date(local.Year(), local.Month(), local.Day()+1, startMinute/60, startMinute%60, 0, 0, loc)
		end = start.Add(length)
	}
	return start, end, !local.Before(start) && local.Before(end)
}

// Status is the GET /chat/anonymous/status answer.
type Status struct {
	// LookingNow is how many people the caller could be paired with are
	// searching for an instant blind chat this moment. Real, not rounded up:
	// someone outside your preferences, or already chatted with, is not
	// counted, because you could never be connected to them.
	LookingNow int `json:"lookingNow"`
	// OnlineNow is how many such people have Thapaw open right now (and are
	// not incognito): who a "someone is looking" alert could reach.
	OnlineNow int `json:"onlineNow"`
	// Hellos is how many hellos on the board the caller can reply to, and
	// NewestHelloAt when the latest of them was posted (for a "new" badge).
	Hellos        int        `json:"hellos"`
	NewestHelloAt *time.Time `json:"newestHelloAt,omitempty"`
	BlindHour     Period     `json:"blindHour"`
	// MyHello is the caller's waiting blind hello, if any.
	MyHello *Hello `json:"myHello"`
	// Practice says the AI practice chat is available (POST /chat/practice).
	Practice bool `json:"practice"`
}

type Hello struct {
	Body      string    `json:"body"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Replies is how many chats it has opened (it closes at five).
	Replies int `json:"replies"`
}

type Period struct {
	Active   bool      `json:"active"`
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`
}

type Service struct {
	pool     *pgxpool.Pool
	guard    func(http.Handler) http.Handler
	now      func() time.Time
	alerts   LookingNotifier
	practice bool
}

// LookingNotifier sends the "someone is looking" push (mobilepush.Queue).
type LookingNotifier interface {
	EnqueueLooking(ctx context.Context, tx pgx.Tx, userID string) error
}

// WithAlerts enables "someone is looking" pushes.
func (s *Service) WithAlerts(n LookingNotifier) *Service {
	s.alerts = n
	return s
}

// WithPractice reports whether the AI practice chat is configured.
func (s *Service) WithPractice(available bool) *Service {
	s.practice = available
	return s
}

func New(pool *pgxpool.Pool, users *auth.Service) *Service {
	return &Service{pool: pool, guard: users.RequireAuth, now: time.Now}
}

// Routes registers GET /chat/anonymous/status.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.Handle("GET /chat/anonymous/status", s.guard(http.HandlerFunc(s.handleStatus)))
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := auth.UserIDFrom(ctx)
	prefs, err := settings.Load(ctx, s.pool, uid)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	var looking, online, hellos int
	var newest *time.Time
	if err := s.pool.QueryRow(ctx, `
		select
		  (select count(*) from chat_queue q
		     join users u on u.id = q.user_id and u.status = 'active'
		     join profiles p on p.user_id = q.user_id
		    where q.user_id <> me.user_id and q.expires_at > now() and `+chat.CompatibleSQL+`),
		  (select count(*) from profiles p
		     join users u on u.id = p.user_id and u.status = 'active'
		    where p.user_id <> me.user_id and not p.incognito
		      and p.last_active_at > now() - interval '3 minutes' and `+chat.CompatibleSQL+`),
		  o.n, o.newest
		from profiles me
		cross join lateral (`+openHellosSQL+`) o
		where me.user_id = $1`, uid).Scan(&looking, &online, &hellos, &newest); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, r, err)
		return
	}
	var hello *Hello
	var h Hello
	err = s.pool.QueryRow(ctx, `select body, expires_at, reply_count from blind_hellos where user_id = $1 and expires_at > now()`, uid).
		Scan(&h.Body, &h.ExpiresAt, &h.Replies)
	switch {
	case err == nil:
		hello = &h
	case !errors.Is(err, pgx.ErrNoRows):
		httpx.Error(w, r, err)
		return
	}
	start, end, active := Window(s.now(), prefs.Location())
	httpx.JSON(w, http.StatusOK, Status{
		LookingNow:    looking,
		OnlineNow:     online,
		Hellos:        hellos,
		NewestHelloAt: newest,
		BlindHour:     Period{Active: active, StartsAt: start.UTC(), EndsAt: end.UTC()},
		MyHello:       hello,
		Practice:      s.practice,
	})
}

// openHellosSQL counts the board hellos that "me" (a profiles row in the
// enclosing query) can answer, and when the newest was posted: the same
// visibility rule as chat's hello board. Yields one row (n, newest).
const openHellosSQL = `select count(*) as n, max(h.created_at) as newest from blind_hellos h
		     join users u on u.id = h.user_id and u.status = 'active'
		     join profiles p on p.user_id = h.user_id
		    where h.user_id <> me.user_id and h.expires_at > now() and h.reply_count < 5 and h.report_count < 3
		      and not exists (select 1 from blind_hello_replies r where r.hello_id = h.id and r.user_id = me.user_id)
		      and not exists (select 1 from blind_hello_reports x where x.hello_id = h.id and x.user_id = me.user_id)
		      and ` + chat.CompatibleSQL

// alertsPerSearch and alertEvery bound the "someone is looking" pushes: a
// search alerts at most this many people, and nobody gets one more often.
const (
	alertsPerSearch = 10
	alertEvery      = 3 * time.Hour
)

// Looking tells up to alertsPerSearch compatible people (switch on, a phone,
// not searching themselves, not alerted recently) that someone is looking
// for a blind chat right now, so a small community still pairs. Used as the
// chat package's BlindHooks.Looking; errors are logged, never surfaced.
func (s *Service) Looking(ctx context.Context, searcher string) {
	if s.alerts == nil {
		return
	}
	rows, err := s.pool.Query(ctx, `
		select p.user_id::text from profiles p
		join users u on u.id = p.user_id and u.status = 'active'
		join profiles me on me.user_id = $1
		left join user_settings st on st.user_id = p.user_id
		where p.user_id <> $1 and coalesce(st.notify_blind_hour, true)
		  and exists (select 1 from devices d where d.user_id = p.user_id)
		  and not exists (select 1 from chat_queue q where q.user_id = p.user_id and q.expires_at > now())
		  and not exists (select 1 from blind_alerts a where a.recipient = p.user_id and a.sent_at > now() - $2::interval)
		  and `+chat.CompatibleSQL+`
		order by p.last_active_at desc limit $3
	`, searcher, alertEvery.String(), alertsPerSearch)
	if err != nil {
		slog.Warn("blind alerts", "error", err)
		return
	}
	var recipients []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			recipients = append(recipients, id)
		}
	}
	rows.Close()
	for _, id := range recipients {
		if err := s.alertOne(ctx, id); err != nil {
			slog.Warn("blind alert", "error", err)
		}
	}
	_, _ = s.pool.Exec(ctx, `delete from blind_alerts where sent_at < now() - interval '1 day'`)
}

func (s *Service) alertOne(ctx context.Context, recipient string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `insert into blind_alerts (recipient) values ($1)`, recipient); err != nil {
		return err
	}
	if err := s.alerts.EnqueueLooking(ctx, tx, recipient); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Notifier is the one thing the reminder loop needs from mobilepush; an
// interface so this package does not import it (and tests can record).
type Notifier interface {
	EnqueueBlindHour(ctx context.Context, tx pgx.Tx, userID string) error
	EnqueueHelloDigest(ctx context.Context, tx pgx.Tx, userID string, hellos int) error
}

// RunReminders sends "Blind hour is open" once a day to each person with a
// registered phone, the switch on and a known time zone, in the first
// minutes of their local Blind hour. It blocks until ctx ends.
func RunReminders(ctx context.Context, pool *pgxpool.Pool, n Notifier) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if err := remindDue(ctx, pool, n, time.Now()); err != nil && ctx.Err() == nil {
			slog.Warn("blind hour reminders", "error", err)
		}
		if err := digestDue(ctx, pool, n, time.Now()); err != nil && ctx.Err() == nil {
			slog.Warn("hello digest", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// remindDue is one pass. People without a settings row have never had their
// time zone sent by the app, so their 9 PM is unknown and they are skipped.
func remindDue(ctx context.Context, pool *pgxpool.Pool, n Notifier, now time.Time) error {
	rows, err := pool.Query(ctx, `
		select s.user_id::text, s.tz from user_settings s
		join users u on u.id = s.user_id and u.status = 'active'
		where s.notify_blind_hour and exists (select 1 from devices d where d.user_id = s.user_id)
	`)
	if err != nil {
		return err
	}
	type due struct {
		user string
		day  time.Time
	}
	var list []due
	for rows.Next() {
		var user, tz string
		if err := rows.Scan(&user, &tz); err != nil {
			rows.Close()
			return err
		}
		prefs := settings.Defaults()
		prefs.QuietHours.TZ = tz
		start, _, active := Window(now, prefs.Location())
		if active && now.Sub(start) < remindWithin {
			list = append(list, due{user, start})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range list {
		if err := remindOne(ctx, pool, n, d.user, d.day); err != nil {
			return err
		}
	}
	return nil
}

func remindOne(ctx context.Context, pool *pgxpool.Pool, n Notifier, user string, day time.Time) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `insert into blind_hour_reminders (user_id, day) values ($1, $2) on conflict do nothing`,
		user, day.Format(time.DateOnly))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // already reminded today
	}
	if err := n.EnqueueBlindHour(ctx, tx, user); err != nil {
		return err
	}
	_, _ = tx.Exec(ctx, `delete from blind_hour_reminders where user_id = $1 and day < current_date - 7`, user)
	return tx.Commit(ctx)
}

// The hello digest: once a day, in the first minutes after 6 PM local time,
// people with new hellos they can answer (posted in the last day) are told
// how many. Nothing is sent when there are none.
const (
	digestHour   = 18
	digestWithin = 10 * time.Minute
)

func digestDue(ctx context.Context, pool *pgxpool.Pool, n Notifier, now time.Time) error {
	rows, err := pool.Query(ctx, `
		select s.user_id::text, s.tz from user_settings s
		join users u on u.id = s.user_id and u.status = 'active'
		where s.notify_blind_hour and exists (select 1 from devices d where d.user_id = s.user_id)
	`)
	if err != nil {
		return err
	}
	type due struct {
		user string
		day  time.Time
	}
	var list []due
	for rows.Next() {
		var user, tz string
		if err := rows.Scan(&user, &tz); err != nil {
			rows.Close()
			return err
		}
		prefs := settings.Defaults()
		prefs.QuietHours.TZ = tz
		local := now.In(prefs.Location())
		start := time.Date(local.Year(), local.Month(), local.Day(), digestHour, 0, 0, 0, local.Location())
		if !local.Before(start) && local.Sub(start) < digestWithin {
			list = append(list, due{user, start})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range list {
		if err := digestOne(ctx, pool, n, d.user, d.day); err != nil {
			return err
		}
	}
	return nil
}

func digestOne(ctx context.Context, pool *pgxpool.Pool, n Notifier, user string, day time.Time) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `insert into hello_digests (user_id, day) values ($1, $2) on conflict do nothing`,
		user, day.Format(time.DateOnly))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // already handled today
	}
	var count int
	err = tx.QueryRow(ctx, `select o.n from profiles me cross join lateral (`+openHellosSQL+`
		      and h.created_at > now() - interval '24 hours') o where me.user_id = $1`, user).Scan(&count)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if count > 0 {
		if err := n.EnqueueHelloDigest(ctx, tx, user, count); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `delete from hello_digests where day < current_date - 7`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
