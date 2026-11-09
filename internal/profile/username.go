package profile

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/shak0x90/velora_backend/internal/domain"
)

// Usernames are the public, unique handle a friend types to find someone.
// They are stored lowercase, so "Shakil" and "shakil" are the same name, and
// the database's unique index is the final word on who holds one.

// ErrUsernameTaken is a username someone else already holds.
var ErrUsernameTaken = errors.New("username taken")

// The same rule as the profiles_username_format check: 3–20 characters of
// a-z, 0-9, "_" and ".", starting with a letter or digit.
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.]{2,19}$`)

// reservedUsernames would let someone pass as staff or as the app itself.
var reservedUsernames = map[string]bool{
	"admin": true, "administrator": true, "thapaw": true, "velora": true,
	"support": true, "help": true, "moderator": true, "mod": true, "staff": true,
	"team": true, "official": true, "security": true, "safety": true,
	"system": true, "root": true, "null": true, "undefined": true, "me": true,
}

// NormalizeUsername is what every input goes through first: surrounding
// space and a leading "@" are dropped and the rest is lowercased, so a person
// can paste "@Shakil " and mean "shakil".
func NormalizeUsername(raw string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "@"))
}

// ValidateUsername explains, in a sentence a person can act on, why a
// normalised username cannot be chosen. Nil means the format is fine; whether
// it is free is a separate question only the database can answer.
func ValidateUsername(name string) error {
	switch {
	case len(name) < 3:
		return ValidationError{Detail: "Usernames need at least 3 characters."}
	case len(name) > 20:
		return ValidationError{Detail: "Usernames can be at most 20 characters."}
	case !usernamePattern.MatchString(name):
		return ValidationError{Detail: "Use only letters, numbers, underscores and dots, starting with a letter or number."}
	case reservedUsernames[name]:
		return ValidationError{Detail: "That username is reserved."}
	}
	return nil
}

// LooksLikeUsername reports whether raw could be someone's username at all,
// which is what decides whether a search tries an exact lookup.
func LooksLikeUsername(raw string) bool {
	return usernamePattern.MatchString(NormalizeUsername(raw))
}

// isUsernameConflict recognises the unique index refusing a write.
func isUsernameConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "profiles_username_key")
}

// Availability is the answer to "can I have this username?".
type Availability struct {
	Username  string `json:"username"`
	Available bool   `json:"available"`
	// Detail says why not, when Available is false.
	Detail string `json:"detail,omitempty"`
}

// UsernameAvailable checks a username for the caller. Their own current
// username counts as available: keeping it is always allowed.
func (s *Service) UsernameAvailable(ctx context.Context, userID, raw string) (Availability, error) {
	name := NormalizeUsername(raw)
	out := Availability{Username: name}
	if err := ValidateUsername(name); err != nil {
		out.Detail = err.Error()
		return out, nil
	}
	var holder string
	err := s.pool.QueryRow(ctx, `select user_id::text from profiles where username = $1`, name).Scan(&holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		out.Available = true
	case err != nil:
		return Availability{}, err
	case holder == userID:
		out.Available = true
	default:
		out.Detail = "That username is taken."
	}
	return out, nil
}

// LoadByUsername finds one person by exact username, under the same
// visibility rules as every other read (hidden, blocked, suspended and
// incognito people stay out of sight). Dating preferences do not apply: the
// viewer asked for this particular person.
func (s *Service) LoadByUsername(ctx context.Context, viewer domain.Profile, raw string) (domain.Profile, error) {
	name := NormalizeUsername(raw)
	if !usernamePattern.MatchString(name) {
		return domain.Profile{}, ErrNotFound
	}
	if name == viewer.Username {
		return viewer, nil
	}
	found, err := s.query(ctx,
		"username = $1 and "+strings.ReplaceAll(visibleWhere, "$1", "$2"),
		[]any{name, viewer.ID}, s.pointFor(ctx, viewer.ID), 1)
	if err != nil {
		return domain.Profile{}, err
	}
	if len(found) == 0 {
		return domain.Profile{}, ErrNotFound
	}
	return found[0].Public(), nil
}
