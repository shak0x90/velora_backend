package discovery

import (
	"testing"

	"github.com/shak0x90/velora_backend/internal/domain"
)

// The parsing here is the only place a client's raw query string reaches the
// ranking engine, and the failure mode is silent: a filter set that matches
// nobody renders as an empty feed rather than as an error.

func TestParseFiltersDefaultsWhenAbsent(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		got, err := parseFilters(raw)
		if err != nil {
			t.Fatalf("parseFilters(%q): %v", raw, err)
		}
		// The zero value would carry MaxAge 0, which excludes everyone alive.
		if got.MinAge != domain.DefaultFilters().MinAge ||
			got.MaxAge != domain.DefaultFilters().MaxAge ||
			got.MaxDistanceKm != domain.DefaultFilters().MaxDistanceKm {
			t.Errorf("parseFilters(%q) = %+v, want the defaults", raw, got)
		}
	}
}

func TestParseFiltersKeepsDefaultsForOmittedFields(t *testing.T) {
	got, err := parseFilters(`{"minAge":30,"genders":["woman"]}`)
	if err != nil {
		t.Fatalf("parseFilters: %v", err)
	}
	if got.MinAge != 30 {
		t.Errorf("MinAge = %d, want 30", got.MinAge)
	}
	// Omitted, so it must keep the default rather than collapsing to zero.
	if want := domain.DefaultFilters().MaxAge; got.MaxAge != want {
		t.Errorf("MaxAge = %d, want the default %d", got.MaxAge, want)
	}
	if want := domain.DefaultFilters().MaxDistanceKm; got.MaxDistanceKm != want {
		t.Errorf("MaxDistanceKm = %d, want the default %d", got.MaxDistanceKm, want)
	}
	if len(got.Genders) != 1 || got.Genders[0] != domain.GenderWoman {
		t.Errorf("Genders = %v, want [woman]", got.Genders)
	}
}

func TestParseFiltersRejectsGarbage(t *testing.T) {
	if _, err := parseFilters("not json"); err == nil {
		t.Fatal("expected a rejection for a malformed filters parameter")
	}
}

func TestSplitIDs(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want []string
	}{
		"empty":            {"", nil},
		"single":           {"a", []string{"a"}},
		"trims and splits": {" a , b ", []string{"a", "b"}},
		"drops blanks":     {"a,,b,", []string{"a", "b"}},
		"dedupes":          {"a,b,a", []string{"a", "b"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := splitIDs(test.raw)
			if len(got) != len(test.want) {
				t.Fatalf("splitIDs(%q) = %v, want %v", test.raw, got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("splitIDs(%q) = %v, want %v", test.raw, got, test.want)
				}
			}
		})
	}
}

// Search opens up age and distance but must not override who the viewer said
// they want to see — that is a preference, not a filter to widen.
func TestWideFiltersKeepGenderPreference(t *testing.T) {
	me := domain.Profile{Preferences: domain.DatingPreferences{
		InterestedIn: []domain.Gender{domain.GenderNonBinary},
	}}
	got := wideFilters(me)

	if len(got.Genders) != 1 || got.Genders[0] != domain.GenderNonBinary {
		t.Errorf("Genders = %v, want [nonBinary]", got.Genders)
	}
	if got.MinAge != 18 {
		t.Errorf("MinAge = %d, want 18 — search should not enforce an age range", got.MinAge)
	}
	if got.MaxDistanceKm < 10000 {
		t.Errorf("MaxDistanceKm = %d, want search to ignore distance", got.MaxDistanceKm)
	}
}
