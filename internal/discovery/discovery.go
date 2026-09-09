// Package discovery turns a pool of candidates into a ranked feed.
//
// The split that matters: the database narrows, Go ranks. SQL excludes the
// people who cannot be shown at all — yourself, hidden profiles, anyone you
// already decided about — and everything after that runs through the shared
// compat engine, because that scoring has to stay one implementation across
// the server and both clients rather than being half-expressed in SQL.
package discovery

import (
	"context"
	"net/http"

	"github.com/shak0x90/velora_backend/internal/compat"
	"github.com/shak0x90/velora_backend/internal/domain"
	"github.com/shak0x90/velora_backend/internal/profile"
)

// candidatePool is how many people are considered before ranking. Scoring is
// cheap per profile but assembling one is not, so this bounds the read rather
// than the arithmetic — well above any feed a person scrolls in one session.
const candidatePool = 300

// dailyPickCount matches the clients' daily picks strip.
const dailyPickCount = 5

type Service struct {
	profiles *profile.Service
	guard    func(http.Handler) http.Handler
}

// New wires the service. Profiles are read through the profile package rather
// than queried here, so there stays exactly one definition of what a profile
// is and how it is assembled.
func New(profiles *profile.Service, guard func(http.Handler) http.Handler) *Service {
	return &Service{profiles: profiles, guard: guard}
}

// Re-exported so handlers can tell the two apart without importing the profile
// package for a pair of sentinels: ErrNoProfile is the caller's own onboarding
// being unfinished, ErrNotFound is somebody else being absent or hidden.
var (
	ErrNoProfile = profile.ErrNoProfile
	ErrNotFound  = profile.ErrNotFound
)

// viewer loads the caller's own profile, which every ranking needs: a score is
// always relative to somebody.
func (s *Service) viewer(ctx context.Context, userID string) (domain.Profile, error) {
	return s.profiles.Load(ctx, userID)
}

// Feed ranks everyone the viewer could still see, applying the client's
// filters. Those are applied in Go by compat.PassesFilters, inside Recommend,
// so the server and the clients agree on what each filter means.
func (s *Service) Feed(ctx context.Context, userID string, filters domain.DiscoverFilters) ([]domain.RankedProfile, error) {
	me, err := s.viewer(ctx, userID)
	if err != nil {
		return nil, err
	}
	candidates, err := s.profiles.Candidates(ctx, me, candidatePool)
	if err != nil {
		return nil, err
	}
	return compat.Recommend(me, candidates, filters), nil
}

// DailyPicks is the top of the default-filter feed.
//
// It is computed per request rather than frozen for the day. Freezing is a
// scheduled job that needs somewhere to write, and until that table exists a
// stale pick would be worse than a recomputed one.
func (s *Service) DailyPicks(ctx context.Context, userID string) ([]domain.RankedProfile, error) {
	me, err := s.viewer(ctx, userID)
	if err != nil {
		return nil, err
	}
	candidates, err := s.profiles.Candidates(ctx, me, candidatePool)
	if err != nil {
		return nil, err
	}
	return compat.DailyPicks(me, candidates, dailyPickCount), nil
}

// Search ranks the text-matched subset. It deliberately ignores the saved
// discover filters: someone typing a name is looking for that person, not for
// whoever happens to survive their age range.
func (s *Service) Search(ctx context.Context, userID, query string) ([]domain.RankedProfile, error) {
	me, err := s.viewer(ctx, userID)
	if err != nil {
		return nil, err
	}
	matched, err := s.profiles.Search(ctx, me, query, candidatePool)
	if err != nil {
		return nil, err
	}
	return compat.Recommend(me, matched, wideFilters(me)), nil
}

// Detail is one profile plus the compatibility read that explains it.
func (s *Service) Detail(ctx context.Context, userID, id string) (domain.RankedProfile, error) {
	me, err := s.viewer(ctx, userID)
	if err != nil {
		return domain.RankedProfile{}, err
	}
	other, err := s.profiles.LoadOne(ctx, me, id)
	if err != nil {
		return domain.RankedProfile{}, err
	}
	return domain.RankedProfile{
		Profile:       other,
		Compatibility: compat.Calculate(me, other),
	}, nil
}

// People resolves a set of ids to profiles. The likes, matches and
// notification screens all hold ids and need someone to render; without this
// each of them would fetch profiles one at a time.
func (s *Service) People(ctx context.Context, userID string, ids []string) ([]domain.Profile, error) {
	me, err := s.viewer(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.profiles.LoadMany(ctx, me, ids)
}

// Suggestions proposes conversation openers for one pair.
func (s *Service) Suggestions(
	ctx context.Context, userID, id string, tone domain.SuggestionTone,
) ([]domain.ConversationSuggestion, error) {
	me, err := s.viewer(ctx, userID)
	if err != nil {
		return nil, err
	}
	other, err := s.profiles.LoadOne(ctx, me, id)
	if err != nil {
		return nil, err
	}
	return compat.SuggestionsFor(me, other, tone), nil
}

// wideFilters is the loosest filter set that still respects who the viewer
// said they want to see. Age and distance are opened right up because a search
// is an explicit request for a particular person, but gender preference is a
// floor rather than a filter, so it stays.
func wideFilters(me domain.Profile) domain.DiscoverFilters {
	return domain.DiscoverFilters{
		MinAge:        18,
		MaxAge:        120,
		MaxDistanceKm: 20000,
		Genders:       me.Preferences.InterestedIn,
	}
}
