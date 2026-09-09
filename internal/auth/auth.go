// Package auth handles identity: verifying a Google ID token, mapping it to a
// user, and issuing our own tokens.
//
// There are no passwords anywhere in this package, by design. Sign-up and
// sign-in are the same action — if the Google identity is new we create the
// user, otherwise we return the existing one — so there is no separate
// registration flow, nothing to hash, and nothing to reset.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/api/idtoken"
)

var (
	ErrInvalidToken     = errors.New("invalid or expired token")
	ErrEmailNotVerified = errors.New("google account has no verified email")
	ErrUserSuspended    = errors.New("account is suspended")
	// ErrTokenReuse means a refresh token that was already spent came back.
	// The realistic cause is theft, so the whole family is revoked.
	ErrTokenReuse = errors.New("refresh token reuse detected")
)

// AuthUser is the shape both clients already expect.
type AuthUser struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Phone       string `json:"phone"`
	Method      string `json:"method"`
	IsPremium   bool   `json:"isPremium"`
}

type Session struct {
	User         AuthUser `json:"user"`
	AccessToken  string   `json:"accessToken"`
	RefreshToken string   `json:"refreshToken"`
	ExpiresAt    string   `json:"expiresAt"`
	IsNew        bool     `json:"isNew"`
}

type Config struct {
	SigningKey      []byte
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	// GoogleClientIDs are the OAuth client IDs we accept as `aud`: one each for
	// web, Android, and iOS. A token minted for someone else's app must not be
	// usable here, which is exactly what checking this prevents.
	GoogleClientIDs []string
}

type Service struct {
	pool *pgxpool.Pool
	cfg  Config
}

func New(pool *pgxpool.Pool, cfg Config) *Service {
	return &Service{pool: pool, cfg: cfg}
}

// ---------------------------------------------------------------------------
// Google
// ---------------------------------------------------------------------------

type googleIdentity struct {
	Subject string
	Email   string
	Name    string
}

// verifyGoogle validates the signature against Google's published keys, then
// checks the audience ourselves. We pass an empty audience to Validate and do
// the check here because we accept several client IDs (web/Android/iOS) and
// Validate only takes one.
func (s *Service) verifyGoogle(ctx context.Context, rawToken string) (googleIdentity, error) {
	if len(s.cfg.GoogleClientIDs) == 0 {
		return googleIdentity{}, errors.New("no GOOGLE_CLIENT_IDS configured")
	}

	payload, err := idtoken.Validate(ctx, rawToken, "")
	if err != nil {
		return googleIdentity{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	audienceOK := false
	for _, allowed := range s.cfg.GoogleClientIDs {
		if payload.Audience == allowed {
			audienceOK = true
			break
		}
	}
	if !audienceOK {
		return googleIdentity{}, fmt.Errorf("%w: audience %q not allowed", ErrInvalidToken, payload.Audience)
	}

	if payload.Issuer != "https://accounts.google.com" && payload.Issuer != "accounts.google.com" {
		return googleIdentity{}, fmt.Errorf("%w: unexpected issuer %q", ErrInvalidToken, payload.Issuer)
	}

	// An unverified Google email must not be trusted: it would let someone
	// claim an address they do not control.
	if verified, _ := payload.Claims["email_verified"].(bool); !verified {
		return googleIdentity{}, ErrEmailNotVerified
	}

	email, _ := payload.Claims["email"].(string)
	name, _ := payload.Claims["given_name"].(string)
	if name == "" {
		name, _ = payload.Claims["name"].(string)
	}
	if email == "" {
		return googleIdentity{}, fmt.Errorf("%w: no email claim", ErrInvalidToken)
	}

	return googleIdentity{
		Subject: payload.Subject,
		Email:   strings.ToLower(email),
		Name:    name,
	}, nil
}

// SignInWithGoogle is both sign-up and sign-in.
func (s *Service) SignInWithGoogle(ctx context.Context, rawToken string) (Session, error) {
	identity, err := s.verifyGoogle(ctx, rawToken)
	if err != nil {
		return Session{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID uuid.UUID
	var status string
	var isPremium bool
	isNew := false

	// Look the user up by the Google subject, not the email: an email can be
	// reassigned by a workspace admin, but the subject is stable forever.
	err = tx.QueryRow(ctx, `
		select u.id, u.status, u.is_premium
		from auth_identities ai
		join users u on u.id = ai.user_id
		where ai.method = 'google' and ai.identifier = $1
	`, identity.Subject).Scan(&userID, &status, &isPremium)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		isNew = true
		userID = uuid.New()
		status = "active"
		if _, err := tx.Exec(ctx,
			`insert into users (id, status) values ($1, 'active')`, userID); err != nil {
			return Session{}, fmt.Errorf("create user: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into auth_identities (user_id, method, identifier, verified_at)
			values ($1, 'google', $2, now())
		`, userID, identity.Subject); err != nil {
			return Session{}, fmt.Errorf("create identity: %w", err)
		}
		// Record the email as its own identity so a future email sign-in path
		// resolves to the same user instead of creating a duplicate.
		if _, err := tx.Exec(ctx, `
			insert into auth_identities (user_id, method, identifier, verified_at)
			values ($1, 'email', $2, now())
			on conflict (method, identifier) do nothing
		`, userID, identity.Email); err != nil {
			return Session{}, fmt.Errorf("record email identity: %w", err)
		}
	case err != nil:
		return Session{}, fmt.Errorf("lookup identity: %w", err)
	}

	if status == "suspended" || status == "deleted" {
		return Session{}, ErrUserSuspended
	}

	session, err := s.issue(ctx, tx, userID, uuid.New())
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}

	session.IsNew = isNew
	session.User = AuthUser{
		ID:          userID.String(),
		DisplayName: identity.Name,
		Email:       identity.Email,
		Method:      "google",
		IsPremium:   isPremium,
	}
	return session, nil
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

func (s *Service) issue(ctx context.Context, tx pgx.Tx, userID, familyID uuid.UUID) (Session, error) {
	now := time.Now().UTC()
	accessExp := now.Add(s.cfg.AccessTokenTTL)

	claims := jwt.MapClaims{
		"sub": userID.String(),
		"iat": now.Unix(),
		"exp": accessExp.Unix(),
	}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.cfg.SigningKey)
	if err != nil {
		return Session{}, fmt.Errorf("sign access token: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, fmt.Errorf("generate refresh token: %w", err)
	}
	refresh := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(refresh))

	if _, err := tx.Exec(ctx, `
		insert into refresh_tokens (id, user_id, family_id, token_hash, expires_at)
		values ($1, $2, $3, $4, $5)
	`, uuid.New(), userID, familyID, sum[:], now.Add(s.cfg.RefreshTokenTTL)); err != nil {
		return Session{}, fmt.Errorf("store refresh token: %w", err)
	}

	return Session{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    accessExp.Format(time.RFC3339),
	}, nil
}

// Refresh rotates the token. Presenting one that was already spent revokes the
// entire family: the legitimate holder and the thief both get logged out,
// which is the correct outcome when we cannot tell them apart.
func (s *Service) Refresh(ctx context.Context, presented string) (Session, error) {
	sum := sha256.Sum256([]byte(presented))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id, userID, familyID uuid.UUID
	var expiresAt time.Time
	var usedAt, revokedAt *time.Time

	// `for update` is load-bearing, not a precaution. Reading the row and then
	// marking it spent are two statements: without the lock, two concurrent
	// refreshes both observe used_at as null and both issue a session, so one
	// token buys two. The second caller now blocks until the first commits and
	// then takes the reuse path below, which is the honest outcome — we cannot
	// tell a racing client from a stolen token.
	err = tx.QueryRow(ctx, `
		select id, user_id, family_id, expires_at, used_at, revoked_at
		from refresh_tokens where token_hash = $1
		for update
	`, sum[:]).Scan(&id, &userID, &familyID, &expiresAt, &usedAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrInvalidToken
	}
	if err != nil {
		return Session{}, err
	}

	if usedAt != nil {
		if _, revokeErr := tx.Exec(ctx, `
			update refresh_tokens set revoked_at = now()
			where family_id = $1 and revoked_at is null
		`, familyID); revokeErr != nil {
			return Session{}, revokeErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return Session{}, commitErr
		}
		return Session{}, ErrTokenReuse
	}
	if revokedAt != nil || time.Now().After(expiresAt) {
		return Session{}, ErrInvalidToken
	}

	if _, err := tx.Exec(ctx,
		`update refresh_tokens set used_at = now() where id = $1`, id); err != nil {
		return Session{}, err
	}

	session, err := s.issue(ctx, tx, userID, familyID)
	if err != nil {
		return Session{}, err
	}

	var email, name, method string
	var isPremium bool
	if err := tx.QueryRow(ctx, primaryIdentityQuery, userID).
		Scan(&email, &name, &method, &isPremium); err != nil {
		return Session{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}

	session.User = AuthUser{
		ID: userID.String(), DisplayName: name, Email: email,
		Method: method, IsPremium: isPremium,
	}
	return session, nil
}

// primaryIdentityQuery reports how the account actually signs in. Google wins
// when both exist, because that is the passwordless path the person chose.
const primaryIdentityQuery = `
	select coalesce((select identifier from auth_identities
	                 where user_id = u.id and method = 'email' limit 1), ''),
	       coalesce((select first_name from profiles where user_id = u.id), ''),
	       coalesce((select method from auth_identities
	                 where user_id = u.id
	                 order by (method = 'google') desc limit 1), 'email'),
	       u.is_premium
	from users u where u.id = $1
`

// SignOut revokes the whole family, so every device sharing that chain is out.
func (s *Service) SignOut(ctx context.Context, presented string) error {
	sum := sha256.Sum256([]byte(presented))
	_, err := s.pool.Exec(ctx, `
		with revoked as (update refresh_tokens set revoked_at = now()
		where family_id = (select family_id from refresh_tokens where token_hash = $1)
		  and revoked_at is null returning user_id),
		cleared as (delete from chat_tickets where user_id in(select user_id from revoked))
		select pg_notify('velora_chat','logout:' || user_id::text) from revoked group by user_id
	`, sum[:])
	return err
}

// ParseAccessToken returns the user id carried by a valid access token.
func (s *Service) ParseAccessToken(raw string) (string, error) {
	token, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		return s.cfg.SigningKey, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !token.Valid {
		return "", ErrInvalidToken
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", ErrInvalidToken
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", ErrInvalidToken
	}
	return sub, nil
}

// CurrentUser loads the user behind an access token's subject.
func (s *Service) CurrentUser(ctx context.Context, userID string) (AuthUser, error) {
	var out AuthUser
	err := s.pool.QueryRow(ctx, `
		select u.id::text,
		       coalesce((select identifier from auth_identities
		                 where user_id = u.id and method = 'email' limit 1), ''),
		       coalesce((select first_name from profiles where user_id = u.id), ''),
		       coalesce((select method from auth_identities
		                 where user_id = u.id
		                 order by (method = 'google') desc limit 1), 'email'),
		       u.is_premium
		from users u
		where u.id = $1 and u.status = 'active'
	`, userID).Scan(&out.ID, &out.Email, &out.DisplayName, &out.Method, &out.IsPremium)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthUser{}, ErrInvalidToken
	}
	return out, err
}

// DecodeSigningKey accepts the base64 value from TOKEN_SIGNING_KEY, falling
// back to raw bytes so a plain string in .env still works in development.
func DecodeSigningKey(value string) []byte {
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil && len(decoded) >= 32 {
		return decoded
	}
	return []byte(value)
}
