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

// Run against an isolated database: PROFILE_TEST_DATABASE_URL=.../velora_privacy_test.
func TestIncognitoVisibility(t *testing.T) {
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
	viewer, subject := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		// Cleanup runs after defers, so use a short-lived connection.
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer p.Close()
		if _, err := p.Exec(ctx, `delete from users where id=any($1::uuid[])`, []string{viewer, subject}); err != nil {
			t.Error(err)
		}
	})
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{viewer, subject} {
		exec(`insert into users(id) values($1)`, id)
		exec(`insert into profiles(user_id,first_name,birth_date,gender) values($1,'Privacy tester','1995-01-01','woman')`, id)
	}
	s := &Service{pool: pool, photos: media.New(pool, media.LocalStore{}, nil)}
	me := domain.Profile{ID: viewer}
	check := func(want bool, candidate bool) {
		t.Helper()
		p, err := s.LoadOne(ctx, me, subject)
		if want {
			if err != nil || p.ID != subject {
				t.Fatalf("direct lookup: %v, %v", p, err)
			}
		} else if !errors.Is(err, ErrNotFound) {
			t.Fatalf("direct lookup exposed profile: %v", err)
		}
		for name, load := range map[string]func() ([]domain.Profile, error){
			"batch":     func() ([]domain.Profile, error) { return s.LoadMany(ctx, me, []string{subject}) },
			"search":    func() ([]domain.Profile, error) { return s.Search(ctx, me, "Privacy", 100) },
			"discovery": func() ([]domain.Profile, error) { return s.Candidates(ctx, me, 100) },
		} {
			profiles, err := load()
			if err != nil {
				t.Fatal(name, err)
			}
			found := false
			for _, p := range profiles {
				if p.ID == subject {
					found = true
				}
			}
			expected := want
			if name == "discovery" {
				expected = candidate
			}
			if found != expected {
				t.Fatalf("%s visibility=%v want %v", name, found, expected)
			}
		}
	}
	check(true, true) // Previously viewed public profile.
	exec(`update profiles set incognito=true where user_id=$1`, subject)
	check(false, false) // Knowing its ID does not preserve access.
	exec(`insert into likes(id,from_user_id,to_user_id) values($1,$2,$3)`, uuid.NewString(), viewer, subject)
	check(false, false) // A stranger cannot grant themselves access by liking.
	exec(`delete from likes where from_user_id=$1`, viewer)
	exec(`insert into likes(id,from_user_id,to_user_id) values($1,$2,$3)`, uuid.NewString(), subject, viewer)
	check(true, true)
	exec(`delete from likes where from_user_id=$1`, subject)
	exec(`insert into matches(id,user_a,user_b) values($1,least($2::uuid,$3::uuid),greatest($2::uuid,$3::uuid))`, uuid.NewString(), viewer, subject)
	check(true, false) // Matching consumes likes; existing matches stay readable.
	for _, pair := range [][2]string{{viewer, subject}, {subject, viewer}} {
		exec(`insert into blocks(user_id,blocked_id) values($1,$2)`, pair[0], pair[1])
		check(false, false)
		exec(`delete from blocks where user_id=$1 and blocked_id=$2`, pair[0], pair[1])
	}
	exec(`update profiles set hidden=true where user_id=$1`, subject)
	check(false, false)
	owner := domain.Profile{ID: subject}
	if p, err := s.LoadOne(ctx, owner, subject); err != nil || !p.Incognito || !p.Hidden {
		t.Fatal("owner cannot read own settings", err)
	}
	if p, err := s.LoadMany(ctx, owner, []string{subject}); err != nil || len(p) != 1 || !p[0].Incognito {
		t.Fatal("owner batch lookup", err)
	}
	exec(`update profiles set hidden=false where user_id=$1`, subject)
	exec(`update users set status='suspended' where id=$1`, subject)
	check(false, false)
	exec(`update users set status='active' where id=$1`, subject)
	exec(`delete from matches where user_a=$1 or user_b=$1`, subject)
	check(false, false)
}
