package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shak0x90/velora_backend/internal/domain"
)

func man(id, username string) domain.Profile {
	p := person(id)
	p.Gender, p.Username = domain.GenderMan, username
	return p
}

func searchService(profiles *fakeProfiles) *Service {
	s := New(nil, nil)
	s.profiles = profiles
	return s
}

func get(t *testing.T, h http.Handler, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// The reported bug: Shakil, a man set to see women, searched for his friend
// Alex (a man) and was told nobody by that name exists.
func TestSearchByNameIgnoresDatingPreferences(t *testing.T) {
	me := man("me", "shakil")
	me.Preferences.InterestedIn = []domain.Gender{domain.GenderWoman}
	alex := man("alex-id", "alex")
	alex.Age = 55 // outside any default age range, too
	profiles := &fakeProfiles{me: me, searchResults: []domain.Profile{alex}}

	got, err := searchService(profiles).Search(context.Background(), "me", "alex")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Profile.ID != "alex-id" {
		t.Fatalf("search for a friend outside my preferences returned %v", got)
	}
}

func TestEmptySearchStillBrowsesWithPreferences(t *testing.T) {
	me := man("me", "shakil")
	me.Preferences.InterestedIn = []domain.Gender{domain.GenderWoman}
	profiles := &fakeProfiles{me: me, searchResults: []domain.Profile{man("m", "mo1"), person("w")}}

	got, err := searchService(profiles).Search(context.Background(), "me", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Profile.ID != "w" {
		t.Fatalf("an empty box should browse like the feed: %v", got)
	}
}

func TestSearchPutsExactUsernameFirst(t *testing.T) {
	me := man("me", "shakil")
	// Two Alexes match the text; the exact username must lead whatever the
	// compatibility ranking says.
	alexA := man("a", "alexander")
	alexB := man("b", "alex")
	alexB.Interests = nil
	profiles := &fakeProfiles{me: me, searchResults: []domain.Profile{alexA, alexB}}

	for _, q := range []string{"alex", "@Alex", " ALEX "} {
		got, err := searchService(profiles).Search(context.Background(), "me", q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Profile.Username != "alex" {
			t.Fatalf("q=%q: want @alex first, got %v", q, got)
		}
	}
}

func TestSearchFetchesExactUsernameOutsideTheTextMatches(t *testing.T) {
	me := man("me", "shakil")
	friend := man("f", "rafi.k")
	profiles := &fakeProfiles{me: me, candidates: []domain.Profile{friend}}

	got, err := searchService(profiles).Search(context.Background(), "me", "@rafi.k")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Profile.ID != "f" {
		t.Fatalf("want the exact username fetched directly, got %v", got)
	}

	// Someone unavailable (hidden, blocked...) is not conjured up.
	profiles.gone = map[string]bool{"f": true}
	got, err = searchService(profiles).Search(context.Background(), "me", "@rafi.k")
	if err != nil || len(got) != 0 {
		t.Fatalf("an unavailable person leaked into search: %v %v", got, err)
	}
}

func TestByUsernameRoute(t *testing.T) {
	profiles := &fakeProfiles{me: man("me", "shakil"), candidates: []domain.Profile{man("f", "alex")}}
	store := &fakeDaily{loc: time.UTC, batches: map[string][]StoredPick{}}
	h := newDailyService(profiles, store, time.Now())

	for path, want := range map[string]int{
		"/usernames/alex":   200,
		"/usernames/@ALEX":  200,
		"/usernames/nobody": 404,
	} {
		code, body := get(t, h, path)
		if code != want {
			t.Errorf("%s: status %d, want %d (%v)", path, code, want, body)
		}
		if code == 200 && body["profile"].(map[string]any)["username"] != "alex" {
			t.Errorf("%s: body %v", path, body)
		}
	}
}
