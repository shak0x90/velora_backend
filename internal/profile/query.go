package profile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/shak0x90/velora_backend/internal/domain"
)

// Reading profiles — the caller's own and everyone else's.
//
// Discovery goes through here rather than writing its own queries, so exactly
// one place knows how a row becomes a domain.Profile. Reads are batched by
// design: a feed of forty candidates costs five queries, not a hundred and
// sixty.

// profileColumns is shared by every read so the scan order can never drift
// from the select list.
const profileColumns = `
	user_id::text, first_name, birth_date, gender, pronouns,
	city, neighborhood, occupation, education, bio,
	interests, relationship_intent, communication_style, personality_traits,
	exercise, drinking, smoking, diet, sleep_schedule, pets, children,
	religion, languages, height_cm,
	interested_in, min_age, max_age, max_distance_km, intents,
	photo_verified, phone_verified, hidden, incognito, last_active_at`

// Point is a viewer's coordinates, used to measure distance to everyone else.
// Nil means the viewer has no location yet, which is the normal state until a
// client asks for one.
type Point struct{ Lat, Lng float64 }

// distanceExpr measures kilometres from the viewer in SQL, so the database can
// use the GiST index on ll_to_earth rather than shipping every row back.
//
// Unknown coordinates score 0 rather than null: an unplaced profile should
// still be visible, and a distance filter it could never satisfy would hide it
// permanently.
const distanceExpr = `
	case
		when $%d::float8 is null or lat is null or lng is null then 0
		else earth_distance(ll_to_earth($%d, $%d), ll_to_earth(lat, lng)) / 1000
	end`

// query runs a profile read and enriches the results. `where` is interpolated
// but never carries user input: callers pass literal predicates and bind their
// values as arguments.
func (s *Service) query(
	ctx context.Context, where string, args []any, from *Point, limit int,
) ([]domain.Profile, error) {
	var lat, lng any
	if from != nil {
		lat, lng = from.Lat, from.Lng
	}
	args = append(args, lat, lng)
	latArg, lngArg := len(args)-1, len(args)

	statement := fmt.Sprintf(
		`select %s, %s as distance_km from profiles where %s order by last_active_at desc`,
		profileColumns, fmt.Sprintf(distanceExpr, latArg, latArg, lngArg), where,
	)
	if limit > 0 {
		statement += fmt.Sprintf(" limit %d", limit)
	}

	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query profiles: %w", err)
	}
	defer rows.Close()

	profiles := []domain.Profile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.enrich(ctx, profiles)
}

func scanProfile(row pgx.Row) (domain.Profile, error) {
	var p domain.Profile
	var interestedIn, intents []string
	var heightCm *int
	var lastActive time.Time

	err := row.Scan(
		&p.ID, &p.FirstName, &p.DateOfBirth, &p.Gender, &p.Pronouns,
		&p.City, &p.Neighborhood, &p.Occupation, &p.Education, &p.Bio,
		&p.Interests, &p.RelationshipIntent, &p.CommunicationStyle, &p.PersonalityTraits,
		&p.Lifestyle.Exercise, &p.Lifestyle.Drinking, &p.Lifestyle.Smoking,
		&p.Lifestyle.Diet, &p.Lifestyle.SleepSchedule, &p.Lifestyle.Pets,
		&p.Lifestyle.Children, &p.Lifestyle.Religion, &p.Lifestyle.Languages, &heightCm,
		&interestedIn, &p.Preferences.MinAge, &p.Preferences.MaxAge,
		&p.Preferences.MaxDistanceKm, &intents,
		&p.PhotoVerified, &p.PhoneVerified, &p.Hidden, &p.Incognito, &lastActive,
		&p.DistanceKm,
	)
	if err != nil {
		return domain.Profile{}, err
	}

	p.Lifestyle.HeightCm = heightCm
	p.Preferences.InterestedIn = toGenders(interestedIn)
	p.Preferences.Intents = toIntents(intents)
	p.Age = ageOn(p.DateOfBirth, time.Now())
	p.LastActive = lastActive
	p.OnlineStatus = statusFrom(lastActive, time.Now())
	return p, nil
}

// enrich attaches photos, prompts and personality answers in three batched
// queries rather than three per profile.
func (s *Service) enrich(ctx context.Context, profiles []domain.Profile) ([]domain.Profile, error) {
	if len(profiles) == 0 {
		return profiles, nil
	}

	ids := make([]string, 0, len(profiles))
	for _, p := range profiles {
		ids = append(ids, p.ID)
	}

	photos, err := s.photos.URLsFor(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("load photos: %w", err)
	}
	prompts, err := s.promptsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	answers, err := s.answersFor(ctx, ids)
	if err != nil {
		return nil, err
	}

	for i := range profiles {
		id := profiles[i].ID
		// Empty slices, never nil: these serialise as [] rather than null, and
		// the clients index into them without a guard.
		profiles[i].Photos = photos[id]
		if profiles[i].Photos == nil {
			profiles[i].Photos = []string{}
		}
		profiles[i].Prompts = prompts[id]
		if profiles[i].Prompts == nil {
			profiles[i].Prompts = []domain.ProfilePrompt{}
		}
		profiles[i].PersonalityAnswers = answers[id]
		if profiles[i].PersonalityAnswers == nil {
			profiles[i].PersonalityAnswers = []domain.PersonalityAnswer{}
		}
	}
	return profiles, nil
}

func (s *Service) promptsFor(ctx context.Context, ids []string) (map[string][]domain.ProfilePrompt, error) {
	rows, err := s.pool.Query(ctx, `
		select user_id::text, id::text, question, answer from profile_prompts
		where user_id = any($1) order by user_id, position
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("load prompts: %w", err)
	}
	defer rows.Close()

	out := map[string][]domain.ProfilePrompt{}
	for rows.Next() {
		var userID string
		var prompt domain.ProfilePrompt
		if err := rows.Scan(&userID, &prompt.ID, &prompt.Question, &prompt.Answer); err != nil {
			return nil, err
		}
		out[userID] = append(out[userID], prompt)
	}
	return out, rows.Err()
}

func (s *Service) answersFor(ctx context.Context, ids []string) (map[string][]domain.PersonalityAnswer, error) {
	// Ordered by question_id rather than insertion so every profile presents
	// the same questions in the same order.
	rows, err := s.pool.Query(ctx, `
		select user_id::text, question_id, question, answer from personality_answers
		where user_id = any($1) order by user_id, question_id
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("load personality answers: %w", err)
	}
	defer rows.Close()

	out := map[string][]domain.PersonalityAnswer{}
	for rows.Next() {
		var userID string
		var answer domain.PersonalityAnswer
		if err := rows.Scan(&userID, &answer.QuestionID, &answer.Question, &answer.Answer); err != nil {
			return nil, err
		}
		out[userID] = append(out[userID], answer)
	}
	return out, rows.Err()
}

// Load assembles the caller's own profile. Returns ErrNoProfile when
// onboarding has not run.
func (s *Service) Load(ctx context.Context, userID string) (domain.Profile, error) {
	// No viewer point: the distance from you to yourself is zero, and the
	// clients read that field unconditionally.
	found, err := s.query(ctx, "user_id = $1", []any{userID}, nil, 1)
	if err != nil {
		return domain.Profile{}, err
	}
	if len(found) == 0 {
		return domain.Profile{}, ErrNoProfile
	}
	return found[0], nil
}

// LoadOne reads somebody else's profile, measured from the viewer. A hidden
// profile is invisible to everyone but its owner.
func (s *Service) LoadOne(ctx context.Context, viewer domain.Profile, id string) (domain.Profile, error) {
	if id == viewer.ID {
		return s.Load(ctx, id)
	}
	found, err := s.query(ctx,
		"user_id = $1 and hidden = false", []any{id}, s.pointFor(ctx, viewer.ID), 1)
	if err != nil {
		return domain.Profile{}, err
	}
	if len(found) == 0 {
		return domain.Profile{}, ErrNotFound
	}
	return found[0], nil
}

// candidateWhere is who may still appear in a feed: everyone except yourself,
// the hidden, and anyone you have already decided about.
//
// Those decisions are excluded in SQL rather than in Go because they are the
// cheapest filter available — an index lookup per row, against sets that grow
// with use — while every other filter needs the assembled profile anyway.
const candidateWhere = `
	user_id <> $1
	and hidden = false
	and not exists (select 1 from passes where user_id = $1 and target_id = profiles.user_id)
	and not exists (select 1 from likes  where from_user_id = $1 and to_user_id = profiles.user_id)
	and not exists (
		select 1 from matches
		where user_a = least($1::uuid, profiles.user_id)
		  and user_b = greatest($1::uuid, profiles.user_id)
	)`

// Candidates returns everyone the viewer could still be shown, most recently
// active first. Ranking happens in Go afterwards: the scoring engine is shared
// with both clients and must stay one implementation, not two.
func (s *Service) Candidates(ctx context.Context, viewer domain.Profile, limit int) ([]domain.Profile, error) {
	return s.query(ctx, candidateWhere, []any{viewer.ID}, s.pointFor(ctx, viewer.ID), limit)
}

// Search narrows the candidate set by free text before ranking. The needle is
// a bound parameter that SQL concatenates itself, so a value containing a
// quote is data rather than syntax.
func (s *Service) Search(ctx context.Context, viewer domain.Profile, query string, limit int) ([]domain.Profile, error) {
	needle := strings.TrimSpace(query)
	if needle == "" {
		return s.Candidates(ctx, viewer, limit)
	}
	where := candidateWhere + `
		and (
			first_name      ilike '%' || $2 || '%'
			or occupation   ilike '%' || $2 || '%'
			or education    ilike '%' || $2 || '%'
			or neighborhood ilike '%' || $2 || '%'
			or city         ilike '%' || $2 || '%'
			or bio          ilike '%' || $2 || '%'
			or exists (select 1 from unnest(interests) i where i ilike '%' || $2 || '%')
		)`
	return s.query(ctx, where, []any{viewer.ID, needle}, s.pointFor(ctx, viewer.ID), limit)
}

// LoadMany reads a specific set of profiles, measured from the viewer. The
// likes and matches screens use it, where the ids come from another table.
//
// Hidden profiles are excluded on the same terms as LoadOne. Without that, the
// batch form was a way around the visibility check: anyone holding an id could
// read a profile its owner had hidden, simply by asking for several at once.
// The viewer is the exception, since hiding yourself should not hide you from
// yourself.
func (s *Service) LoadMany(ctx context.Context, viewer domain.Profile, ids []string) ([]domain.Profile, error) {
	if len(ids) == 0 {
		return []domain.Profile{}, nil
	}
	return s.query(ctx,
		"user_id = any($1) and (hidden = false or user_id = $2)",
		[]any{ids, viewer.ID}, s.pointFor(ctx, viewer.ID), 0)
}

// pointFor reads the viewer's coordinates. A missing location is not an error
// — distances simply come back as zero — so this never fails a request.
func (s *Service) pointFor(ctx context.Context, userID string) *Point {
	var lat, lng *float64
	if err := s.pool.QueryRow(ctx,
		`select lat, lng from profiles where user_id = $1`, userID).Scan(&lat, &lng); err != nil {
		return nil
	}
	if lat == nil || lng == nil {
		return nil
	}
	return &Point{Lat: *lat, Lng: *lng}
}
