package discovery

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/db"
)

// Run with PUSH_TEST_DATABASE_URL=postgres://.../velora_push_test.
func TestDailyStoreAgainstPostgres(t *testing.T) {
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
	defer pool.Close()

	me := uuid.NewString()
	others := []string{uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()}
	all := append([]string{me}, others...)
	for _, id := range all {
		if _, err := pool.Exec(ctx, `insert into users (id) values ($1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `delete from users where id = any($1::uuid[])`, all) })

	store := NewDailyStore(pool)
	if loc, err := store.Location(ctx, me); err != nil || loc != time.UTC {
		t.Fatalf("default zone = %v, %v; want UTC", loc, err)
	}
	if _, err := pool.Exec(ctx, `insert into user_settings (user_id, tz) values ($1, 'Asia/Dhaka')`, me); err != nil {
		t.Fatal(err)
	}
	if loc, err := store.Location(ctx, me); err != nil || loc.String() != "Asia/Dhaka" {
		t.Fatalf("zone = %v, %v; want Asia/Dhaka", loc, err)
	}

	day := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	calls := 0
	generate := func() ([]StoredPick, error) {
		calls++
		return []StoredPick{{others[2], 0.9}, {others[0], 0.8}, {others[1], 0.7}}, nil
	}
	first, err := store.Batch(ctx, me, day, generate)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Batch(ctx, me, day, func() ([]StoredPick, error) {
		t.Fatal("regenerated a stored batch")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(second) != 3 || second[0].ProfileID != others[2] || second[2].ProfileID != others[1] || second[0].RankScore != 0.9 {
		t.Fatalf("stored batch %+v, first %+v", second, first)
	}

	// A new day gets a new batch; one past the retention window is pruned.
	if _, err := pool.Exec(ctx, `insert into daily_edits (user_id, edit_date, profile_id, position) values ($1, '2026-10-01', $2, 0)`, me, others[3]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Batch(ctx, me, day.AddDate(0, 0, 1), generate); err != nil || calls != 2 {
		t.Fatalf("next day: calls=%d err=%v", calls, err)
	}
	var old int
	_ = pool.QueryRow(ctx, `select count(*) from daily_edits where user_id = $1 and edit_date = '2026-10-01'`, me).Scan(&old)
	if old != 0 {
		t.Error("an edit past retention was kept")
	}

	// Decided: a pass, a pending like, and a match all count.
	low, high := me, others[2]
	if low > high {
		low, high = high, low
	}
	for _, sql := range []string{
		`insert into passes (user_id, target_id) values ('` + me + `', '` + others[0] + `')`,
		`insert into likes (id, from_user_id, to_user_id) values ('` + uuid.NewString() + `', '` + me + `', '` + others[1] + `')`,
		`insert into matches (id, user_a, user_b) values ('` + uuid.NewString() + `', '` + low + `', '` + high + `')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(sql, err)
		}
	}
	decided, err := store.Decided(ctx, me, others)
	if err != nil {
		t.Fatal(err)
	}
	if !decided[others[0]] || !decided[others[1]] || !decided[others[2]] || decided[others[3]] {
		t.Fatalf("decided = %v", decided)
	}
}
