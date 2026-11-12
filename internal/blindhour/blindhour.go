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
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/auth"
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
	// LookingNow is how many people are in the anonymous queue this moment
	// (excluding the caller). Real, not rounded up.
	LookingNow int    `json:"lookingNow"`
	BlindHour  Period `json:"blindHour"`
}

type Period struct {
	Active   bool      `json:"active"`
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`
}

type Service struct {
	pool  *pgxpool.Pool
	guard func(http.Handler) http.Handler
	now   func() time.Time
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
	var looking int
	if err := s.pool.QueryRow(ctx,
		`select count(*) from chat_queue where expires_at > now() and user_id <> $1`, uid).Scan(&looking); err != nil {
		httpx.Error(w, r, err)
		return
	}
	start, end, active := Window(s.now(), prefs.Location())
	httpx.JSON(w, http.StatusOK, Status{
		LookingNow: looking,
		BlindHour:  Period{Active: active, StartsAt: start.UTC(), EndsAt: end.UTC()},
	})
}

// Notifier is the one thing the reminder loop needs from mobilepush; an
// interface so this package does not import it (and tests can record).
type Notifier interface {
	EnqueueBlindHour(ctx context.Context, tx pgx.Tx, userID string) error
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
