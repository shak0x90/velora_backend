package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shak0x90/velora_backend/internal/db"
	"google.golang.org/api/idtoken"
)

func TestGoogleClaims(t *testing.T) {
	s := New(nil, Config{GoogleClientIDs: []string{"our-client"}})
	for _, tc := range []struct {
		name, email, hd, aud, iss, sub string
		verified, valid, authoritative bool
	}{
		{"gmail", "person@gmail.com", "", "our-client", "https://accounts.google.com", "subject", true, true, true},
		{"workspace", "person@company.test", "company.test", "our-client", "accounts.google.com", "subject", true, true, true},
		{"third party", "person@example.test", "", "our-client", "accounts.google.com", "subject", true, true, false},
		{"wrong audience", "person@gmail.com", "", "other-client", "accounts.google.com", "subject", true, false, false},
		{"wrong issuer", "person@gmail.com", "", "our-client", "attacker.test", "subject", true, false, false},
		{"unverified", "person@gmail.com", "", "our-client", "accounts.google.com", "subject", false, false, false},
		{"missing subject", "person@gmail.com", "", "our-client", "accounts.google.com", "", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &idtoken.Payload{Audience: tc.aud, Issuer: tc.iss, Subject: tc.sub, Claims: map[string]any{"email": tc.email, "email_verified": tc.verified, "hd": tc.hd}}
			identity, err := s.identityFromGooglePayload(p)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if err == nil && identity.AuthoritativeEmail != tc.authoritative {
				t.Fatal("wrong ownership authority")
			}
		})
	}
}

func TestGoogleOnlyRoutes(t *testing.T) {
	s := New(nil, Config{GoogleOnly: true, GoogleClientIDs: []string{"our-client"}})
	mux := http.NewServeMux()
	s.Routes(mux)
	for _, path := range []string{"/auth/register", "/auth/login"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{"email":"test@gmail.com","password":"password"}`)))
		if w.Code != 404 {
			t.Fatalf("password route %s returned %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/auth/social", strings.NewReader(`{"provider":"google","idToken":"forged"}`)))
	if w.Code != 401 {
		t.Fatalf("forged Google token returned %d", w.Code)
	}
}

func TestGoogleAccountMigration(t *testing.T) {
	dsn := os.Getenv("AUTH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set AUTH_TEST_DATABASE_URL to isolated database ending in _auth_test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.ConnConfig.Database, "_auth_test") {
		t.Fatal("test database required")
	}
	if err = db.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := New(pool, Config{SigningKey: testKey, GoogleOnly: true, AccessTokenTTL: time.Minute, RefreshTokenTTL: time.Hour})
	ids := []string{}
	defer func() {
		for _, id := range ids {
			pool.Exec(ctx, `delete from users where id=$1`, id)
		}
		pool.Close()
	}()
	prefix := uuid.NewString()
	legacyEmail := prefix + "@gmail.com"
	old, err := s.Register(ctx, legacyEmail, "test-password")
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, old.User.ID)
	if _, err = s.ParseAccessToken(old.AccessToken); err == nil {
		t.Fatal("legacy access token accepted")
	}
	if _, err = s.Refresh(ctx, old.RefreshToken); err == nil {
		t.Fatal("legacy refresh token accepted")
	}
	identity := googleIdentity{Subject: prefix, Email: legacyEmail, Name: "Google user", AuthoritativeEmail: true}
	migrated, err := s.signInGoogleIdentity(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.IsNew || migrated.User.ID != old.User.ID {
		t.Fatal("legacy profile duplicated")
	}
	if _, err = s.ParseAccessToken(migrated.AccessToken); err != nil {
		t.Fatal(err)
	}
	refreshed, err := s.Refresh(ctx, migrated.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ParseAccessToken(refreshed.AccessToken); err != nil {
		t.Fatal(err)
	}
	var cleared bool
	if err = pool.QueryRow(ctx, `select password_hash is null from auth_identities where user_id=$1 and method='email'`, old.User.ID).Scan(&cleared); err != nil || !cleared {
		t.Fatal("password retained", err)
	}
	identity.Subject = prefix + "-another-google-account"
	if _, err = s.signInGoogleIdentity(ctx, identity); !errors.Is(err, ErrAccountLinkRequired) {
		t.Fatal("merged different Google subjects", err)
	}
	identity.Subject = prefix
	pool.Exec(ctx, `update users set status='suspended' where id=$1`, old.User.ID)
	if _, err = s.signInGoogleIdentity(ctx, identity); !errors.Is(err, ErrUserSuspended) {
		t.Fatal("suspended sign-in accepted", err)
	}
	if _, err = s.Refresh(ctx, refreshed.RefreshToken); !errors.Is(err, ErrUserSuspended) {
		t.Fatal("suspended refresh accepted", err)
	}

	thirdParty, err := s.Register(ctx, prefix+"@example.test", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, thirdParty.User.ID)
	if _, err = s.signInGoogleIdentity(ctx, googleIdentity{Subject: prefix + "third", Email: prefix + "@example.test"}); !errors.Is(err, ErrAccountLinkRequired) {
		t.Fatal("unproven third-party email linked", err)
	}

	fresh := googleIdentity{Subject: prefix + "fresh", Email: prefix + "fresh@gmail.com", AuthoritativeEmail: true}
	const count = 8
	results := make(chan Session, count)
	failures := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.signInGoogleIdentity(ctx, fresh)
			results <- result
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	userID := ""
	newCount := 0
	for result := range results {
		if userID == "" {
			userID = result.User.ID
			ids = append(ids, userID)
		}
		if result.User.ID != userID {
			t.Fatal("concurrent duplicate account")
		}
		if result.IsNew {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("new accounts=%d", newCount)
	}
}
