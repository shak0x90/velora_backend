package profile

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/db"
	"github.com/shak0x90/velora_backend/internal/domain"
	"github.com/shak0x90/velora_backend/internal/media"
)

func TestNormalizeUsername(t *testing.T) {
	for in, want := range map[string]string{
		"Shakil": "shakil", "@shakil": "shakil", "  @Rafi.K ": "rafi.k", "alex_99": "alex_99",
	} {
		if got := NormalizeUsername(in); got != want {
			t.Errorf("NormalizeUsername(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateUsername(t *testing.T) {
	for _, ok := range []string{"shakil", "abc", "rafi.k", "alex_99", "9lives", strings.Repeat("a", 20)} {
		if err := ValidateUsername(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"ab", strings.Repeat("a", 21), "_alex", ".alex", "al ex", "al-ex", "শাকিল", "admin", "thapaw", "support"} {
		if err := ValidateUsername(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPatchValidatesUsername(t *testing.T) {
	bad := "x!"
	if err := (Patch{Username: &bad}).validate(); err == nil {
		t.Error("a bad username passed patch validation")
	}
	good := "@New_Name"
	if err := (Patch{Username: &good}).validate(); err != nil {
		t.Errorf("a valid username failed: %v", err)
	}
}

// Run against an isolated database: PROFILE_TEST_DATABASE_URL=.../velora_privacy_test.
func TestUsernamesAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("PROFILE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set PROFILE_TEST_DATABASE_URL to an isolated velora_privacy_test database")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.ConnConfig.Database, "_privacy_test") {
		t.Fatal("database name must end in _privacy_test")
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

	// A first name nobody else in the test database has, so the expected
	// suggestions do not depend on what earlier runs left behind.
	name := "Zq" + uuid.NewString()[:8]
	base := strings.ToLower(name)
	a, b, c := uuid.NewString(), uuid.NewString(), uuid.NewString()
	all := []string{a, b, c}
	for _, id := range all {
		if _, err := pool.Exec(ctx, `insert into users (id) values ($1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `delete from users where id = any($1::uuid[])`, all) })

	s := &Service{pool: pool, photos: media.New(pool, media.LocalStore{Root: t.TempDir()}, nil)}
	dob := "1995-05-05"
	onboard := func(id string, username *string) (string, error) {
		g := domain.GenderWoman
		p, err := s.Create(ctx, id, Patch{FirstName: &name, DateOfBirth: &dob, Gender: &g, Username: username})
		return p.Username, err
	}

	// Same first name, no choice made: base, then base2.
	if got, err := onboard(a, nil); err != nil || got != base {
		t.Fatalf("first: %q %v, want %q", got, err, base)
	}
	if got, err := onboard(b, nil); err != nil || got != base+"2" {
		t.Fatalf("second: %q %v, want %q", got, err, base+"2")
	}
	// Re-running onboarding keeps the username.
	if got, err := onboard(a, nil); err != nil || got != base {
		t.Fatalf("restarted wizard changed the username to %q (%v)", got, err)
	}
	// Choosing a taken one is a conflict, not a silent rename.
	taken := strings.ToUpper(base)
	if _, err := onboard(c, &taken); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("taken username at onboarding: %v", err)
	}
	// Choosing a free one, with "@" and capitals, stores it normalised.
	chosen := "@" + strings.ToUpper(base) + "_c"
	if got, err := onboard(c, &chosen); err != nil || got != base+"_c" {
		t.Fatalf("chosen username: %q %v", got, err)
	}

	// PATCH /me rename: taken → conflict, free → stored.
	if _, err := s.Update(ctx, b, Patch{Username: &taken}); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("rename onto a taken username: %v", err)
	}
	renamed := base + ".b"
	if p, err := s.Update(ctx, b, Patch{Username: &renamed}); err != nil || p.Username != renamed {
		t.Fatalf("rename: %q %v", p.Username, err)
	}

	// Availability: own name is fine, someone else's is taken, bad format says why.
	for raw, want := range map[string]bool{base: true, renamed: false, base + "free": true, "a!": false} {
		got, err := s.UsernameAvailable(ctx, a, raw)
		if err != nil || got.Available != want {
			t.Errorf("available(%q) = %+v, %v; want %v", raw, got, err, want)
		}
	}

	// Lookup is exact, case-insensitive, and respects hidden.
	viewer, err := s.Load(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.LoadByUsername(ctx, viewer, "@"+strings.ToUpper(renamed)); err != nil || p.ID != b {
		t.Fatalf("lookup: %v %v", p.ID, err)
	}
	if p, err := s.LoadByUsername(ctx, viewer, renamed); err != nil || p.DateOfBirth != nil {
		t.Fatalf("lookup must return the public profile: %+v %v", p, err)
	}
	if _, err := pool.Exec(ctx, `update profiles set hidden = true where user_id = $1`, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadByUsername(ctx, viewer, renamed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a hidden profile was found by username: %v", err)
	}

	// Search by username prefix works through the text match too.
	if _, err := pool.Exec(ctx, `update profiles set hidden = false where user_id = $1`, b); err != nil {
		t.Fatal(err)
	}
	found, err := s.Search(ctx, viewer, "@"+renamed, 50)
	if err != nil || len(found) != 1 || found[0].ID != b {
		t.Fatalf("search by @username: %v %v", found, err)
	}

	// Raw inserts (fixtures, scripts) get a username from the trigger.
	d := uuid.NewString()
	all = append(all, d)
	if _, err := pool.Exec(ctx, `insert into users (id) values ($1)`, d); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := pool.QueryRow(ctx, `insert into profiles (user_id, first_name, birth_date, gender) values ($1, 'Al', '1990-01-01', 'man') returning username`, d).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "aluser") {
		t.Errorf("a two-letter name should be padded, got %q", got)
	}
}
