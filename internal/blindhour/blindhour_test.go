package blindhour

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/db"
)

func TestWindow(t *testing.T) {
	dhaka, err := time.LoadLocation("Asia/Dhaka")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 11, 11, h, m, 0, 0, dhaka) }
	for _, tc := range []struct {
		name      string
		now       time.Time
		active    bool
		startDay  int
		startHour int
	}{
		{"afternoon: today's is next", at(15, 0), false, 11, 21},
		{"opens at nine", at(21, 0), true, 11, 21},
		{"inside", at(21, 59), true, 11, 21},
		{"just ended: tomorrow's", at(22, 0), false, 12, 21},
		{"after midnight", at(0, 30), false, 11, 21},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, end, active := Window(tc.now, dhaka)
			if active != tc.active || start.Day() != tc.startDay || start.Hour() != tc.startHour {
				t.Fatalf("Window(%s) = %s, active %v", tc.now, start, active)
			}
			if end.Sub(start) != time.Hour {
				t.Fatalf("length %s", end.Sub(start))
			}
		})
	}
	// The same instant is a different local time elsewhere.
	utc9pm := time.Date(2026, 11, 11, 21, 0, 0, 0, time.UTC)
	if _, _, active := Window(utc9pm, dhaka); active {
		t.Fatal("9 PM UTC is 3 AM in Dhaka, not Blind hour")
	}
}

type recorder struct {
	mu    sync.Mutex
	users []string
}

func (r *recorder) EnqueueBlindHour(_ context.Context, _ pgx.Tx, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = append(r.users, userID)
	return nil
}

// Run with PUSH_TEST_DATABASE_URL=postgres://.../velora_push_test.
func TestRemindersOncePerDayInLocalTime(t *testing.T) {
	dsn := os.Getenv("PUSH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PUSH_TEST_DATABASE_URL to an isolated *_push_test database")
	}
	if !strings.Contains(dsn, "_push_test") {
		t.Fatal("database name must end in _push_test")
	}
	if err := db.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	dhaka, off, noPhone := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, u := range []string{dhaka, off, noPhone} {
		if _, err := pool.Exec(ctx, `insert into users (id) values ($1)`, u); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `insert into user_settings (user_id, tz, notify_blind_hour) values ($1, 'Asia/Dhaka', $2)`, u, u != off); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []string{dhaka, off} {
		if _, err := pool.Exec(ctx, `insert into devices (id, user_id, platform, token) values ($1, $2, 'android', $3)`, uuid.New(), u, "tok-"+u); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `delete from users where id = any($1::uuid[])`, []string{dhaka, off, noPhone})
	})

	loc, _ := time.LoadLocation("Asia/Dhaka")
	rec := &recorder{}
	run := func(h, m int) {
		t.Helper()
		if err := remindDue(ctx, pool, rec, time.Date(2026, 11, 11, h, m, 0, 0, loc)); err != nil {
			t.Fatal(err)
		}
	}
	mine := func() int {
		n := 0
		for _, u := range rec.users {
			if u == dhaka || u == off || u == noPhone {
				n++
			}
		}
		return n
	}

	run(20, 59)
	if mine() != 0 {
		t.Fatal("reminded before 9 PM")
	}
	run(21, 2)
	if mine() != 1 || rec.users[len(rec.users)-1] != dhaka {
		t.Fatalf("want exactly the opted-in person with a phone, got %v", rec.users)
	}
	run(21, 4)
	if mine() != 1 {
		t.Fatal("reminded twice on the same day")
	}
	run(21, 30)
	if mine() != 1 {
		t.Fatal("reminded late in the hour")
	}
}
