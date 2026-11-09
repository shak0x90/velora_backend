package settings

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/db"
)

// Run with PUSH_TEST_DATABASE_URL=postgres://.../velora_push_test.
func TestStoreAgainstPostgres(t *testing.T) {
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

	user := uuid.NewString()
	if _, err := pool.Exec(ctx, `insert into users (id) values ($1)`, user); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `delete from users where id = $1`, user) })

	store := pgStore{pool: pool}
	got, err := store.Get(ctx, user)
	if err != nil || got != Defaults() {
		t.Fatalf("no row should read as the defaults: %+v, %v", got, err)
	}

	saved, err := store.Update(ctx, user, func(s Settings) (Settings, error) {
		s.Notify.Weekly = false
		s.QuietHours = QuietHours{Start: str("22:00"), End: str("07:00"), TZ: "Asia/Dhaka"}
		return s, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, user)
	if err != nil || got.Notify != saved.Notify || *got.QuietHours.Start != "22:00" || got.QuietHours.TZ != "Asia/Dhaka" {
		t.Fatalf("round trip: %+v, %v", got, err)
	}

	// A failed change writes nothing.
	if _, err := store.Update(ctx, user, func(Settings) (Settings, error) { return Settings{}, ErrInvalid }); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if again, _ := store.Get(ctx, user); again.Notify.Weekly || again.QuietHours.Start == nil {
		t.Fatalf("a failed update changed the row: %+v", again)
	}

	// The schema backs up the validation: half a window is refused.
	if _, err := pool.Exec(ctx, `update user_settings set quiet_end = null where user_id = $1`, user); err == nil {
		t.Error("the database accepted a quiet start without an end")
	}
}
