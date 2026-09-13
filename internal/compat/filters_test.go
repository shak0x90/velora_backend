package compat_test

import (
	"testing"

	"github.com/shak0x90/velora_backend/internal/compat"
	"github.com/shak0x90/velora_backend/internal/domain"
)

// A viewer who never chose a gender preference must still be shown people.
//
// This is the regression that emptied the whole app. An empty InterestedIn was
// read as a filter matching no gender, so the feed, the daily picks and search
// all came back empty for anyone who had not answered a question onboarding
// never asked. The symptom was a search insisting nobody by that name existed
// while the profile sat in the database, visible to everybody else.
func TestEmptyPreferenceMeansNoPreference(t *testing.T) {
	viewer := domain.Profile{
		ID:     "viewer",
		Age:    25,
		Gender: domain.GenderMan,
		// Exactly what onboarding leaves behind today.
		Preferences: domain.DatingPreferences{},
	}
	wide := domain.DiscoverFilters{MinAge: 18, MaxAge: 120, MaxDistanceKm: 20000}

	for _, gender := range []domain.Gender{
		domain.GenderWoman, domain.GenderMan, domain.GenderNonBinary, domain.GenderOther,
	} {
		candidate := domain.Profile{ID: "candidate", Age: 25, Gender: gender}
		if !compat.PassesFilters(candidate, wide, viewer) {
			t.Errorf("a viewer with no stated preference was shown nobody of gender %q", gender)
		}
	}
}

// A stated preference is still a filter. Opening up the empty case must not
// open up the case where someone actually answered.
func TestStatedPreferenceStillFilters(t *testing.T) {
	viewer := domain.Profile{
		ID:     "viewer",
		Age:    25,
		Gender: domain.GenderMan,
		Preferences: domain.DatingPreferences{
			InterestedIn: []domain.Gender{domain.GenderWoman},
		},
	}
	wide := domain.DiscoverFilters{MinAge: 18, MaxAge: 120, MaxDistanceKm: 20000}

	wanted := domain.Profile{ID: "wanted", Age: 25, Gender: domain.GenderWoman}
	if !compat.PassesFilters(wanted, wide, viewer) {
		t.Error("a candidate of the wanted gender was excluded")
	}

	unwanted := domain.Profile{ID: "unwanted", Age: 25, Gender: domain.GenderMan}
	if compat.PassesFilters(unwanted, wide, viewer) {
		t.Error("a candidate outside the stated preference was included")
	}
}

// An explicit filter from the client overrides the viewer's saved preference,
// which is what lets someone widen a search without editing their profile.
func TestExplicitFilterOverridesSavedPreference(t *testing.T) {
	viewer := domain.Profile{
		ID:     "viewer",
		Age:    25,
		Gender: domain.GenderMan,
		Preferences: domain.DatingPreferences{
			InterestedIn: []domain.Gender{domain.GenderWoman},
		},
	}
	filters := domain.DiscoverFilters{
		MinAge: 18, MaxAge: 120, MaxDistanceKm: 20000,
		Genders: []domain.Gender{domain.GenderMan},
	}
	candidate := domain.Profile{ID: "candidate", Age: 25, Gender: domain.GenderMan}
	if !compat.PassesFilters(candidate, filters, viewer) {
		t.Error("an explicit gender filter did not override the saved preference")
	}
}
