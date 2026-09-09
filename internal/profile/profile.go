// Package profile owns the signed-in person's own profile: reading it,
// creating it during onboarding, and patching it afterwards.
//
// Everyone else's profile is discovery's problem, not this package's. What
// lives here is the `/me` surface — the one profile the caller may write.
//
// The row shape is migration 0002. Prompts and personality answers are
// separate tables because they are ordered lists, and photos belong to the
// media service, so assembling one domain.Profile touches four tables plus
// the object store's URL scheme.
package profile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/domain"
	"github.com/shak0x90/velora_backend/internal/media"
)

// ErrNoProfile means the user exists but has not finished onboarding. It is a
// normal state, not a failure: it is exactly what tells a client to show the
// wizard instead of the feed.
var ErrNoProfile = errors.New("no profile yet")

// ErrNotFound is a profile that does not exist, or is hidden from the caller.
// Distinct from ErrNoProfile on purpose: conflating them reports someone
// else's absence as your own unfinished onboarding, which is both the wrong
// status code and a confusing thing to read.
var ErrNotFound = errors.New("profile not found")

type Service struct {
	pool   *pgxpool.Pool
	photos *media.Service
	users  *auth.Service
	guard  func(http.Handler) http.Handler
}

// New wires the service. Photos come from the media service because that is
// where object keys and URL variants live, and users from auth because /me
// returns the account alongside the profile. guard is auth.RequireAuth — every
// route here reads or writes the caller's own row, so none work unauthenticated.
func New(pool *pgxpool.Pool, photos *media.Service, users *auth.Service) *Service {
	return &Service{pool: pool, photos: photos, users: users, guard: users.RequireAuth}
}

// ---------------------------------------------------------------------------
// Write
// ---------------------------------------------------------------------------

// Create runs the onboarding write: the first row for this user.
//
// Re-running it updates the identity fields rather than failing, because a
// wizard that is abandoned and restarted is normal, and a 409 partway through
// would strand the account with no profile at all.
func (s *Service) Create(ctx context.Context, userID string, patch Patch) (domain.Profile, error) {
	if patch.FirstName == nil || patch.DateOfBirth == nil || patch.Gender == nil {
		return domain.Profile{}, ValidationError{
			Detail: "firstName, dateOfBirth and gender are required to create a profile.",
		}
	}
	if err := patch.validate(); err != nil {
		return domain.Profile{}, err
	}

	birth, err := parseDate(*patch.DateOfBirth)
	if err != nil {
		return domain.Profile{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Profile{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		insert into profiles (user_id, first_name, birth_date, gender)
		values ($1, $2, $3, $4)
		on conflict (user_id) do update set
			first_name = excluded.first_name,
			birth_date = excluded.birth_date,
			gender     = excluded.gender,
			updated_at = now()
	`, userID, strings.TrimSpace(*patch.FirstName), birth, *patch.Gender); err != nil {
		return domain.Profile{}, translateConstraint(err)
	}

	if err := s.applyTx(ctx, tx, userID, patch); err != nil {
		return domain.Profile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Profile{}, err
	}

	if err := s.reconcilePhotos(ctx, userID, patch); err != nil {
		return domain.Profile{}, err
	}
	return s.Load(ctx, userID)
}

// Update applies a partial change to an existing profile.
func (s *Service) Update(ctx context.Context, userID string, patch Patch) (domain.Profile, error) {
	if err := patch.validate(); err != nil {
		return domain.Profile{}, err
	}

	var exists bool
	err := s.pool.QueryRow(ctx, `select true from profiles where user_id = $1`, userID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Profile{}, ErrNoProfile
	}
	if err != nil {
		return domain.Profile{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Profile{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := s.applyTx(ctx, tx, userID, patch); err != nil {
		return domain.Profile{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Profile{}, err
	}

	if err := s.reconcilePhotos(ctx, userID, patch); err != nil {
		return domain.Profile{}, err
	}
	return s.Load(ctx, userID)
}

// applyTx writes every supplied field. It assumes the row already exists and
// that the patch has been validated.
func (s *Service) applyTx(ctx context.Context, tx pgx.Tx, userID string, patch Patch) error {
	set := &assignments{}

	if patch.FirstName != nil {
		set.add("first_name", strings.TrimSpace(*patch.FirstName))
	}
	if patch.DateOfBirth != nil {
		birth, err := parseDate(*patch.DateOfBirth)
		if err != nil {
			return err
		}
		set.add("birth_date", birth)
	}
	if patch.Gender != nil {
		set.add("gender", string(*patch.Gender))
	}
	set.addString("pronouns", patch.Pronouns)
	set.addString("city", patch.City)
	set.addString("neighborhood", patch.Neighborhood)
	set.addString("occupation", patch.Occupation)
	set.addString("education", patch.Education)
	set.addString("bio", patch.Bio)
	set.addStrings("interests", patch.Interests)
	set.addStrings("personality_traits", patch.PersonalityTraits)

	if patch.RelationshipIntent != nil {
		set.add("relationship_intent", string(*patch.RelationshipIntent))
	}
	if patch.CommunicationStyle != nil {
		set.add("communication_style", string(*patch.CommunicationStyle))
	}
	if patch.Lat != nil {
		set.add("lat", *patch.Lat)
	}
	if patch.Lng != nil {
		set.add("lng", *patch.Lng)
	}
	if patch.Incognito != nil {
		set.add("incognito", *patch.Incognito)
	}
	if patch.Hidden != nil {
		set.add("hidden", *patch.Hidden)
	}

	if l := patch.Lifestyle; l != nil {
		set.addString("exercise", l.Exercise)
		set.addString("drinking", l.Drinking)
		set.addString("smoking", l.Smoking)
		set.addString("diet", l.Diet)
		set.addString("sleep_schedule", l.SleepSchedule)
		set.addString("pets", l.Pets)
		set.addString("children", l.Children)
		set.addString("religion", l.Religion)
		set.addStrings("languages", l.Languages)
		if l.HeightCm != nil {
			// The column is nullable because "prefer not to say" is a real
			// answer; sending null clears it rather than storing a zero.
			set.add("height_cm", *l.HeightCm)
		}
	}

	if p := patch.Preferences; p != nil {
		if p.InterestedIn != nil {
			set.add("interested_in", fromGenders(*p.InterestedIn))
		}
		if p.Intents != nil {
			set.add("intents", fromIntents(*p.Intents))
		}
		if p.MinAge != nil {
			set.add("min_age", *p.MinAge)
		}
		if p.MaxAge != nil {
			set.add("max_age", *p.MaxAge)
		}
		if p.MaxDistanceKm != nil {
			set.add("max_distance_km", *p.MaxDistanceKm)
		}
	}

	if len(set.clauses) > 0 {
		statement, args := set.statement(userID)
		if _, err := tx.Exec(ctx, statement, args...); err != nil {
			return translateConstraint(err)
		}
	}

	// Prompts and answers arrive as complete lists, so replace rather than
	// merge: an entry the client dropped should disappear, and reconciling
	// partial lists by position is how orderings get silently corrupted.
	if patch.Prompts != nil {
		if err := replacePrompts(ctx, tx, userID, *patch.Prompts); err != nil {
			return err
		}
	}
	if patch.PersonalityAnswers != nil {
		if err := replaceAnswers(ctx, tx, userID, *patch.PersonalityAnswers); err != nil {
			return err
		}
	}
	return nil
}

func replacePrompts(ctx context.Context, tx pgx.Tx, userID string, prompts []domain.ProfilePrompt) error {
	if _, err := tx.Exec(ctx,
		`delete from profile_prompts where user_id = $1`, userID); err != nil {
		return err
	}
	for index, prompt := range prompts {
		// Client-side ids are local scratch values ("p2_1725489..."), not
		// uuids, so the server mints the real one and the client adopts it on
		// the next read.
		if _, err := tx.Exec(ctx, `
			insert into profile_prompts (id, user_id, position, question, answer)
			values ($1, $2, $3, $4, $5)
		`, uuid.New(), userID, index,
			strings.TrimSpace(prompt.Question), strings.TrimSpace(prompt.Answer)); err != nil {
			return fmt.Errorf("write prompt: %w", err)
		}
	}
	return nil
}

func replaceAnswers(ctx context.Context, tx pgx.Tx, userID string, answers []domain.PersonalityAnswer) error {
	if _, err := tx.Exec(ctx,
		`delete from personality_answers where user_id = $1`, userID); err != nil {
		return err
	}
	for _, answer := range answers {
		if _, err := tx.Exec(ctx, `
			insert into personality_answers (user_id, question_id, question, answer)
			values ($1, $2, $3, $4)
			on conflict (user_id, question_id) do update set
				question = excluded.question, answer = excluded.answer
		`, userID, answer.QuestionID, answer.Question, answer.Answer); err != nil {
			return fmt.Errorf("write personality answer: %w", err)
		}
	}
	return nil
}

// reconcilePhotos runs after the transaction commits, because dropping a photo
// also deletes bytes from the object store and that cannot be rolled back.
func (s *Service) reconcilePhotos(ctx context.Context, userID string, patch Patch) error {
	if patch.Photos == nil {
		return nil
	}
	err := s.photos.Reconcile(ctx, userID, *patch.Photos)
	if errors.Is(err, media.ErrUnknownPhoto) {
		return ValidationError{
			Detail: "Your photos changed somewhere else. Reload and try again.",
		}
	}
	return err
}

// Touch records activity, which is what drives the online indicator. Best
// effort: a failed write must never fail the request that triggered it.
func (s *Service) Touch(ctx context.Context, userID string) {
	_, _ = s.pool.Exec(ctx,
		`update profiles set last_active_at = now() where user_id = $1`, userID)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// assignments builds the SET list for a partial update, so a patch touching
// three fields writes three columns instead of rewriting the row.
type assignments struct {
	clauses []string
	args    []any
}

func (a *assignments) add(column string, value any) {
	a.args = append(a.args, value)
	a.clauses = append(a.clauses, fmt.Sprintf("%s = $%d", column, len(a.args)))
}

func (a *assignments) addString(column string, value *string) {
	if value != nil {
		a.add(column, strings.TrimSpace(*value))
	}
}

func (a *assignments) addStrings(column string, value *[]string) {
	if value != nil {
		a.add(column, cleaned(*value))
	}
}

func (a *assignments) statement(userID string) (string, []any) {
	a.args = append(a.args, userID)
	return fmt.Sprintf(
		"update profiles set %s, updated_at = now() where user_id = $%d",
		strings.Join(a.clauses, ", "), len(a.args),
	), a.args
}

// cleaned drops blanks and duplicates from a tag-style list, preserving order.
func cleaned(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// parseDate accepts either a plain date or the RFC 3339 timestamp the API
// itself emits, so a client can send back exactly what it was given.
func parseDate(raw string) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return time.Date(parsed.Year(), parsed.Month(), parsed.Day(),
				0, 0, 0, 0, time.UTC), nil
		}
	}
	return time.Time{}, ValidationError{Detail: "Use YYYY-MM-DD for your date of birth."}
}

// ageOn counts completed years, so a birthday later this year has not happened
// yet. Subtracting the years alone is off by one for most of the calendar.
func ageOn(birth, now time.Time) int {
	years := now.Year() - birth.Year()
	if now.Month() < birth.Month() ||
		(now.Month() == birth.Month() && now.Day() < birth.Day()) {
		years--
	}
	if years < 0 {
		return 0
	}
	return years
}

// statusFrom maps a last-seen timestamp onto the four buckets the clients
// render. The thresholds match the labels: "online" has to mean now.
func statusFrom(lastActive, now time.Time) domain.OnlineStatus {
	switch elapsed := now.Sub(lastActive); {
	case elapsed < 5*time.Minute:
		return domain.StatusOnline
	case elapsed < 24*time.Hour:
		return domain.StatusRecently
	case elapsed < 7*24*time.Hour:
		return domain.StatusThisWeek
	default:
		return domain.StatusOffline
	}
}

func toGenders(values []string) []domain.Gender {
	out := make([]domain.Gender, 0, len(values))
	for _, value := range values {
		out = append(out, domain.Gender(value))
	}
	return out
}

func fromGenders(values []domain.Gender) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func toIntents(values []string) []domain.RelationshipIntent {
	out := make([]domain.RelationshipIntent, 0, len(values))
	for _, value := range values {
		out = append(out, domain.RelationshipIntent(value))
	}
	return out
}

func fromIntents(values []domain.RelationshipIntent) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

// translateConstraint turns the schema's own guards into something a person
// can act on. The adult-only check is the one a real user can trip.
func translateConstraint(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "profiles_adult_only") {
		return ValidationError{Detail: "You need to be 18 or older to use Velora."}
	}
	return err
}
