package mobilepush

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/shak0x90/velora_backend/internal/chatpush"
	"github.com/shak0x90/velora_backend/internal/db"
)

// testPool connects to an isolated, migrated database, or skips.
// Run with PUSH_TEST_DATABASE_URL=postgres://.../velora_push_test.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("PUSH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PUSH_TEST_DATABASE_URL to an isolated *_push_test database")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.ConnConfig.Database, "_push_test") {
		t.Fatal("database name must end in _push_test")
	}
	if err := db.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := chatpush.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

type recorder struct {
	mu   sync.Mutex
	sent []Message
	err  error
}

func (r *recorder) Send(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, m)
	return r.err
}

func TestDevicesEnqueueAndWorkAgainstPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	a, b := uuid.NewString(), uuid.NewString()
	for _, id := range []string{a, b} {
		if _, err := pool.Exec(ctx, `insert into users (id) values ($1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `delete from river_job where args->>'userId' = any($1)`, []string{a, b})
		_, _ = pool.Exec(ctx, `delete from users where id = any($1::uuid[])`, []string{a, b})
	})
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(sql, err)
		}
		return n
	}

	// Registering moves a token between accounts; unregistering is scoped.
	store := pgDevices{pool: pool}
	token := "tok-" + a
	if err := store.Register(ctx, a, Device{Platform: "android", Token: token, AppVersion: "1.0"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(ctx, b, Device{Platform: "android", Token: token, AppVersion: "1.1"}); err != nil {
		t.Fatal(err)
	}
	if n := count(`select count(*) from devices where token = $1 and user_id = $2 and app_version = '1.1'`, token, b); n != 1 {
		t.Fatalf("token did not move to the new account (%d)", n)
	}
	if err := store.Unregister(ctx, a, token); err != nil {
		t.Fatal(err)
	}
	if n := count(`select count(*) from devices where token = $1`, token); n != 1 {
		t.Fatal("another account removed the device")
	}
	if err := store.Register(ctx, a, Device{Platform: "android", Token: token}); err != nil {
		t.Fatal(err)
	}
	var deviceID string
	if err := pool.QueryRow(ctx, `select id::text from devices where token = $1`, token).Scan(&deviceID); err != nil {
		t.Fatal(err)
	}

	rec := &recorder{}
	q, err := New(pool, Router{Android: rec}, false)
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(n Note) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Enqueue(ctx, tx, a, n); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	jobs := func() int {
		return count(`select count(*) from river_job where kind = 'mobile_push' and queue = $1 and args->>'deviceId' = $2`, QueueName, deviceID)
	}

	enqueue(Note{Event: EventLike, Title: "Someone liked you", Body: "Open your likes."})
	if n := jobs(); n != 1 {
		t.Fatalf("want 1 job on the mobile queue, got %d", n)
	}

	// Switching matches off stops likes at the source.
	if _, err := pool.Exec(ctx, `insert into user_settings (user_id, notify_matches) values ($1, false)`, a); err != nil {
		t.Fatal(err)
	}
	enqueue(Note{Event: EventLike, Title: "x", Body: "y"})
	if n := jobs(); n != 1 {
		t.Fatalf("a disabled category still enqueued (%d jobs)", n)
	}

	// Quiet hours delay rather than drop: the job is scheduled at the end.
	if _, err := pool.Exec(ctx, `update user_settings set notify_matches = true, quiet_start = '00:00', quiet_end = '23:59', tz = 'UTC' where user_id = $1`, a); err != nil {
		t.Fatal(err)
	}
	q.now = func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }
	enqueue(Note{Event: EventMatch, Title: "You have a new match", Body: "You and B liked each other."})
	var scheduled time.Time
	if err := pool.QueryRow(ctx, `select scheduled_at from river_job where args->>'deviceId' = $1 and args->>'event' = 'match'`, deviceID).Scan(&scheduled); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 9, 23, 59, 0, 0, time.UTC); !scheduled.Equal(want) {
		t.Errorf("scheduled at %v, want the end of quiet hours %v", scheduled, want)
	}

	// The worker re-checks preferences: inside quiet hours it snoozes.
	w := &Worker{q: q}
	job := &river.Job[Args]{Args: Args{DeviceID: deviceID, UserID: a, Event: EventMatch, Title: "t", Body: "b"}}
	var snooze *river.JobSnoozeError
	if err := w.Work(ctx, job); !errors.As(err, &snooze) {
		t.Fatalf("want a snooze inside quiet hours, got %v", err)
	}
	if _, err := pool.Exec(ctx, `update user_settings set quiet_start = null, quiet_end = null where user_id = $1`, a); err != nil {
		t.Fatal(err)
	}
	if err := w.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	if len(rec.sent) != 1 || rec.sent[0].Token != token || rec.sent[0].Title != "t" {
		t.Fatalf("sent %+v", rec.sent)
	}

	// A message push is dropped when the message does not exist (or was read).
	if err := w.Work(ctx, &river.Job[Args]{Args: Args{DeviceID: deviceID, UserID: a, Event: EventMessage, MessageID: uuid.NewString()}}); err != nil {
		t.Fatal(err)
	}
	if len(rec.sent) != 1 {
		t.Fatal("pushed a message that is not deliverable")
	}

	// The provider saying the token is dead removes the device.
	rec.err = ErrUnregistered
	if err := w.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
	if n := count(`select count(*) from devices where id = $1`, deviceID); n != 0 {
		t.Fatal("unregistered token was kept")
	}
	// And a job for a device that is gone is a quiet no-op.
	if err := w.Work(ctx, job); err != nil {
		t.Fatal(err)
	}
}
