package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/domain"
	"github.com/shak0x90/velora_backend/internal/profile"
)

func TestEditDayTurnsOverAtNine(t *testing.T) {
	dhaka, _ := time.LoadLocation("Asia/Dhaka")
	ny, _ := time.LoadLocation("America/New_York")
	tests := map[string]struct {
		now      time.Time
		loc      *time.Location
		wantDay  string
		wantNext time.Time
	}{
		"after nine is today": {
			time.Date(2026, 11, 9, 9, 0, 0, 0, dhaka), dhaka,
			"2026-11-09", time.Date(2026, 11, 10, 9, 0, 0, 0, dhaka)},
		"before nine is still yesterday's edit": {
			time.Date(2026, 11, 9, 8, 59, 0, 0, dhaka), dhaka,
			"2026-11-08", time.Date(2026, 11, 9, 9, 0, 0, 0, dhaka)},
		"counted in the person's zone, not UTC": {
			// 02:30 UTC on the 9th is 08:30 in Dhaka: still the 8th's edit.
			time.Date(2026, 11, 9, 2, 30, 0, 0, time.UTC), dhaka,
			"2026-11-08", time.Date(2026, 11, 9, 9, 0, 0, 0, dhaka)},
		"month boundary": {
			time.Date(2026, 12, 1, 7, 0, 0, 0, time.UTC), time.UTC,
			"2026-11-30", time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)},
		"next batch lands at nine local across DST": {
			// US clocks go back on 2026-11-01; 09:00 is still 09:00 local.
			time.Date(2026, 10, 31, 12, 0, 0, 0, ny), ny,
			"2026-10-31", time.Date(2026, 11, 1, 9, 0, 0, 0, ny)},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			day, next := editDay(tc.now, tc.loc)
			if got := day.Format(time.DateOnly); got != tc.wantDay {
				t.Errorf("day = %s, want %s", got, tc.wantDay)
			}
			if !next.Equal(tc.wantNext) || next.In(tc.loc).Hour() != 9 {
				t.Errorf("next = %v, want %v", next, tc.wantNext)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Handler, with the database replaced
// ---------------------------------------------------------------------------

type fakeProfiles struct {
	me         domain.Profile
	noProfile  bool
	candidates []domain.Profile
	// gone are people LoadMany no longer returns (hidden, blocked, ...).
	gone           map[string]bool
	candidateCalls int
	searchResults  []domain.Profile
}

func (f *fakeProfiles) Load(context.Context, string) (domain.Profile, error) {
	if f.noProfile {
		return domain.Profile{}, ErrNoProfile
	}
	return f.me, nil
}

func (f *fakeProfiles) Candidates(context.Context, domain.Profile, int) ([]domain.Profile, error) {
	f.candidateCalls++
	return f.candidates, nil
}

// Search returns whatever the test put in searchResults: the SQL text match
// is not under test here, only what discovery does with its rows.
func (f *fakeProfiles) Search(context.Context, domain.Profile, string, int) ([]domain.Profile, error) {
	return f.searchResults, nil
}

func (f *fakeProfiles) LoadByUsername(_ context.Context, _ domain.Profile, username string) (domain.Profile, error) {
	username = profile.NormalizeUsername(username)
	for _, p := range f.candidates {
		if p.Username == username && !f.gone[p.ID] {
			return p, nil
		}
	}
	return domain.Profile{}, ErrNotFound
}

func (f *fakeProfiles) LoadOne(context.Context, domain.Profile, string) (domain.Profile, error) {
	return domain.Profile{}, ErrNotFound
}

func (f *fakeProfiles) LoadMany(_ context.Context, _ domain.Profile, ids []string) ([]domain.Profile, error) {
	byID := map[string]domain.Profile{}
	for _, p := range f.candidates {
		byID[p.ID] = p
	}
	out := []domain.Profile{}
	for _, id := range ids {
		if p, ok := byID[id]; ok && !f.gone[id] {
			out = append(out, p)
		}
	}
	return out, nil
}

type fakeDaily struct {
	loc     *time.Location
	batches map[string][]StoredPick
	decided map[string]bool
}

func (f *fakeDaily) Location(context.Context, string) (*time.Location, error) { return f.loc, nil }

func (f *fakeDaily) Batch(_ context.Context, userID string, date time.Time, generate func() ([]StoredPick, error)) ([]StoredPick, error) {
	key := userID + "/" + date.Format(time.DateOnly)
	if b, ok := f.batches[key]; ok {
		return b, nil
	}
	b, err := generate()
	if err == nil && len(b) > 0 {
		f.batches[key] = b
	}
	return b, err
}

func (f *fakeDaily) Decided(_ context.Context, _ string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range ids {
		if f.decided[id] {
			out[id] = true
		}
	}
	return out, nil
}

func person(id string) domain.Profile {
	return domain.Profile{ID: id, FirstName: id, Age: 30, Gender: domain.GenderWoman,
		Interests: []string{"hiking"}}
}

func newDailyService(profiles *fakeProfiles, store *fakeDaily, now time.Time) http.Handler {
	s := New(nil, nil)
	s.profiles = profiles
	s.guard = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), "me")))
		})
	}
	s.WithDailyEdits(store)
	s.now = func() time.Time { return now }
	mux := http.NewServeMux()
	s.Routes(mux)
	return mux
}

func getEdit(t *testing.T, h http.Handler) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/discover/daily-edit", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func pickIDs(body map[string]any) []string {
	var ids []string
	for _, p := range body["picks"].([]any) {
		ids = append(ids, p.(map[string]any)["profile"].(map[string]any)["id"].(string))
	}
	return ids
}

func TestDailyEditIsStableForTheDay(t *testing.T) {
	var people []domain.Profile
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		people = append(people, person(id))
	}
	profiles := &fakeProfiles{me: person("me"), candidates: people}
	store := &fakeDaily{loc: time.UTC, batches: map[string][]StoredPick{}, decided: map[string]bool{}}
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	h := newDailyService(profiles, store, now)

	code, first := getEdit(t, h)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, first)
	}
	firstIDs := pickIDs(first)
	if len(firstIDs) != dailyPickCount {
		t.Fatalf("picks = %v, want %d", firstIDs, dailyPickCount)
	}
	if first["date"] != "2026-11-09" || first["nextBatchAt"] != "2026-11-10T09:00:00Z" {
		t.Errorf("date %v, nextBatchAt %v", first["date"], first["nextBatchAt"])
	}
	if first["remaining"] != float64(5) || first["total"] != float64(5) {
		t.Errorf("remaining %v total %v", first["remaining"], first["total"])
	}

	// New people joining later in the day must not reshuffle the batch.
	profiles.candidates = append([]domain.Profile{person("zz-newcomer")}, profiles.candidates...)
	_, second := getEdit(t, h)
	if got := pickIDs(second); len(got) != 5 || got[0] != firstIDs[0] || got[4] != firstIDs[4] {
		t.Fatalf("batch changed within the day: %v then %v", firstIDs, got)
	}
	if profiles.candidateCalls != 1 {
		t.Errorf("ranked %d times; the stored batch should be reused", profiles.candidateCalls)
	}
}

func TestDailyEditSkipsDecidedAndUnavailablePeople(t *testing.T) {
	profiles := &fakeProfiles{
		me:         person("me"),
		candidates: []domain.Profile{person("a"), person("b"), person("c"), person("d"), person("e")},
		gone:       map[string]bool{"d": true}, // went hidden after the batch was made
	}
	store := &fakeDaily{
		loc: time.UTC,
		batches: map[string][]StoredPick{"me/2026-11-09": {
			{"a", 0.9}, {"b", 0.8}, {"c", 0.7}, {"d", 0.6}, {"e", 0.5},
		}},
		decided: map[string]bool{"b": true, "e": true}, // passed one, liked one
	}
	h := newDailyService(profiles, store, time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC))

	code, body := getEdit(t, h)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	got := pickIDs(body)
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("picks = %v, want [a c] in stored order", got)
	}
	if body["remaining"] != float64(2) || body["total"] != float64(5) {
		t.Errorf("remaining %v total %v, want 2 and 5", body["remaining"], body["total"])
	}
	first := body["picks"].([]any)[0].(map[string]any)
	if first["rankScore"] != 0.9 {
		t.Errorf("rankScore = %v, want the stored 0.9", first["rankScore"])
	}
	if _, ok := first["compatibility"].(map[string]any); !ok {
		t.Error("picks must carry a compatibility read like the other feeds")
	}
}

func TestDailyEditCompleteHasEmptyPicks(t *testing.T) {
	profiles := &fakeProfiles{me: person("me"), candidates: []domain.Profile{person("a")}}
	store := &fakeDaily{loc: time.UTC,
		batches: map[string][]StoredPick{"me/2026-11-09": {{"a", 1}}},
		decided: map[string]bool{"a": true}}
	code, body := getEdit(t, newDailyService(profiles, store, time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)))
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if picks, ok := body["picks"].([]any); !ok || len(picks) != 0 || body["remaining"] != float64(0) {
		t.Fatalf("want picks: [] and remaining 0, got %v", body)
	}
}

func TestDailyEditNeedsAProfile(t *testing.T) {
	profiles := &fakeProfiles{noProfile: true}
	store := &fakeDaily{loc: time.UTC, batches: map[string][]StoredPick{}}
	code, body := getEdit(t, newDailyService(profiles, store, time.Now()))
	if code != http.StatusConflict || body["type"] != "about:blank#no-profile" {
		t.Fatalf("status %d body %v, want 409 #no-profile", code, body)
	}
}
