package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/shak0x90/velora_backend/internal/httpx"
)

// Password auth exists so accounts can be created before the Google OAuth
// clients are configured — those need a real domain, which the test server
// does not have yet. The Google path in auth.go stays in place and dormant.

const (
	minPasswordLength = 8
	// bcrypt silently ignores everything past 72 bytes. Rejecting longer input
	// is better than accepting a password whose tail does nothing.
	maxPasswordBytes = 72
)

var (
	ErrEmailTaken       = errors.New("email already registered")
	ErrBadCredentials   = errors.New("email or password is incorrect")
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", minPasswordLength)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
)

func normalizeEmail(raw string) (string, error) {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	if trimmed == "" {
		return "", errors.New("email is required")
	}
	if _, err := mail.ParseAddress(trimmed); err != nil {
		return "", errors.New("that does not look like an email address")
	}
	return trimmed, nil
}

func checkPassword(raw string) error {
	if len([]rune(raw)) < minPasswordLength {
		return ErrPasswordTooShort
	}
	if len(raw) > maxPasswordBytes {
		return ErrPasswordTooLong
	}
	return nil
}

// Register creates a user with an email/password identity.
func (s *Service) Register(ctx context.Context, rawEmail, rawPassword string) (Session, error) {
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		return Session{}, err
	}
	if err := checkPassword(rawPassword); err != nil {
		return Session{}, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(rawPassword), bcrypt.DefaultCost)
	if err != nil {
		return Session{}, fmt.Errorf("hash password: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// An email row may already exist without a password. Google sign-in records
	// one as a side effect, so that a later email sign-in resolves to the same
	// person instead of creating a second account for one address.
	//
	// Registration must not adopt it. Signing up proves someone can type an
	// address, not that they own it — so setting a password on an existing
	// identity and handing back that user's session is account takeover by
	// anyone who knows their email address, and guessing which addresses have
	// signed in with Google is not hard.
	//
	// Both cases are therefore refused identically, which also keeps the answer
	// free of a new way to tell registered addresses apart. Adding a password
	// to a Google account is a real need, but it belongs behind proof of
	// ownership: an authenticated "set a password" endpoint, or a verification
	// link once there is any way to send mail. Until one of those exists, the
	// honest answer is that the address is taken.
	var userID uuid.UUID
	var existingHash []byte
	err = tx.QueryRow(ctx, `
		select user_id, password_hash from auth_identities
		where method = 'email' and identifier = $1
	`, email).Scan(&userID, &existingHash)

	switch {
	case err == nil:
		return Session{}, ErrEmailTaken
	case errors.Is(err, pgx.ErrNoRows):
		userID = uuid.New()
		if _, err := tx.Exec(ctx,
			`insert into users (id, status) values ($1, 'active')`, userID); err != nil {
			return Session{}, fmt.Errorf("create user: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into auth_identities (user_id, method, identifier, password_hash, password_set_at)
			values ($1, 'email', $2, $3, now())
		`, userID, email, hash); err != nil {
			return Session{}, fmt.Errorf("create identity: %w", err)
		}
	default:
		return Session{}, err
	}

	session, err := s.issue(ctx, tx, userID, uuid.New())
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}

	session.IsNew = true
	session.User = AuthUser{ID: userID.String(), Email: email, Method: "email"}
	return session, nil
}

// Login verifies an email/password pair.
func (s *Service) Login(ctx context.Context, rawEmail, rawPassword string) (Session, error) {
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		return Session{}, ErrBadCredentials
	}

	var userID uuid.UUID
	var hash []byte
	var status string
	var isPremium bool
	err = s.pool.QueryRow(ctx, `
		select ai.user_id, ai.password_hash, u.status, u.is_premium
		from auth_identities ai
		join users u on u.id = ai.user_id
		where ai.method = 'email' and ai.identifier = $1
	`, email).Scan(&userID, &hash, &status, &isPremium)

	if errors.Is(err, pgx.ErrNoRows) || (err == nil && hash == nil) {
		// Compare against a dummy hash anyway. Returning early here would make
		// "unknown email" measurably faster than "wrong password", which is
		// enough to enumerate who has an account.
		_ = bcrypt.CompareHashAndPassword(
			[]byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"),
			[]byte(rawPassword),
		)
		return Session{}, ErrBadCredentials
	}
	if err != nil {
		return Session{}, err
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(rawPassword)); err != nil {
		return Session{}, ErrBadCredentials
	}
	if status != "active" {
		return Session{}, ErrUserSuspended
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	session, err := s.issue(ctx, tx, userID, uuid.New())
	if err != nil {
		return Session{}, err
	}

	var name string
	if err := tx.QueryRow(ctx,
		`select coalesce((select first_name from profiles where user_id = $1), '')`,
		userID).Scan(&name); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}

	session.User = AuthUser{
		ID: userID.String(), DisplayName: name, Email: email,
		Method: "email", IsPremium: isPremium,
	}
	return session, nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	session, err := s.Register(r.Context(), req.Email, req.Password)
	if err != nil {
		httpx.Error(w, r, translatePassword(err))
		return
	}
	httpx.JSON(w, http.StatusCreated, session)
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	session, err := s.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		httpx.Error(w, r, translatePassword(err))
		return
	}
	httpx.JSON(w, http.StatusOK, session)
}

func translatePassword(err error) error {
	switch {
	case errors.Is(err, ErrBadCredentials):
		// Deliberately identical for unknown email and wrong password.
		return httpx.Unauthorized("Email or password is incorrect.")
	case errors.Is(err, ErrEmailTaken):
		return httpx.Problem{
			Type: "about:blank#email-taken", Title: "Email already registered",
			Status: http.StatusConflict,
			Detail: "An account already uses that email. Try signing in instead.",
		}
	case errors.Is(err, ErrPasswordTooShort), errors.Is(err, ErrPasswordTooLong):
		return httpx.BadRequest(err.Error())
	case errors.Is(err, ErrUserSuspended):
		return httpx.Forbidden("This account is not available.")
	default:
		if strings.Contains(err.Error(), "email") {
			return httpx.BadRequest(err.Error())
		}
		return err
	}
}
