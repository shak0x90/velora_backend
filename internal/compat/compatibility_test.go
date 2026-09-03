package compat_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/shak0x90/velora_backend/internal/compat"
	"github.com/shak0x90/velora_backend/internal/domain"
)

// vector is one (viewer, candidate) pair and the result the Dart engine
// produced for it. Generate testdata/vectors.json from the Flutter repo with
// tool/dump_vectors.dart, then commit it. Never hand-edit expectations to make
// a test pass: if Go disagrees with Dart, Go is wrong.
type vector struct {
	A        domain.Profile             `json:"a"`
	B        domain.Profile             `json:"b"`
	Expected domain.CompatibilityResult `json:"expected"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	path := filepath.Join("testdata", "vectors.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("no golden vectors yet: generate %s from the Flutter repo", path)
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var vectors []vector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(vectors) == 0 {
		t.Fatalf("%s is empty", path)
	}
	return vectors
}

// TestMatchesDartGoldens is the contract that lets us delete the client-side
// scorers. Every sub-score, the overall, the shared-interest list, and the
// generated copy must match byte for byte.
func TestMatchesDartGoldens(t *testing.T) {
	for i, v := range loadVectors(t) {
		got := compat.Calculate(v.A, v.B)
		if !reflect.DeepEqual(got, v.Expected) {
			t.Errorf("vector %d (%s -> %s): mismatch\n got: %+v\nwant: %+v",
				i, v.A.ID, v.B.ID, got, v.Expected)
		}
	}
}

// TestDeterministic guards the property the product actually sells: the same
// two people always score the same, no matter when you ask.
func TestDeterministic(t *testing.T) {
	a, b := sampleProfiles()
	first := compat.Calculate(a, b)
	for i := 0; i < 50; i++ {
		if !reflect.DeepEqual(compat.Calculate(a, b), first) {
			t.Fatalf("Calculate is not deterministic (run %d)", i)
		}
	}
}

// TestLocationIsAsymmetric documents deliberate behavior: location is scored
// against the *viewer's* max distance, so the reading differs by direction.
func TestLocationIsAsymmetric(t *testing.T) {
	a, b := sampleProfiles()
	a.Preferences.MaxDistanceKm = 5
	b.Preferences.MaxDistanceKm = 100
	a.DistanceKm = 40
	b.DistanceKm = 40

	if compat.LocationScore(a, b) == compat.LocationScore(b, a) {
		t.Fatal("expected location score to differ by direction")
	}
}

func TestEmptyProfilesScoreNeutrally(t *testing.T) {
	var a, b domain.Profile
	a.Preferences.MaxDistanceKm = 25
	got := compat.Calculate(a, b)
	if got.OverallScore <= 0 || got.OverallScore > 100 {
		t.Fatalf("empty profiles should still land in range, got %d", got.OverallScore)
	}
	if got.Summary == "" {
		t.Fatal("summary should never be empty")
	}
}

func sampleProfiles() (domain.Profile, domain.Profile) {
	a := domain.Profile{
		ID:                 "user_alex",
		FirstName:          "Alex",
		Age:                29,
		DistanceKm:         0,
		Interests:          []string{"Travel", "Live Music", "Books", "Coffee"},
		RelationshipIntent: domain.IntentLongTerm,
		CommunicationStyle: domain.CommsThoughtful,
		PersonalityTraits:  []string{"curious", "warm", "intentional"},
		PersonalityAnswers: []domain.PersonalityAnswer{{QuestionID: "weekend", Answer: "A"}},
		Preferences: domain.DatingPreferences{
			InterestedIn:  []domain.Gender{domain.GenderWoman, domain.GenderNonBinary},
			MaxDistanceKm: 25,
		},
		Lifestyle: domain.Lifestyle{
			Exercise: "A few times a week", Drinking: "Socially", Smoking: "Never",
			Diet: "Everything", SleepSchedule: "Night owl", Pets: "Dog person",
			Children: "Want someday", Religion: "Open / spiritual",
			Languages: []string{"English", "French"},
		},
	}
	b := domain.Profile{
		ID:                 "maya",
		FirstName:          "Maya",
		Age:                28,
		DistanceKm:         4.2,
		Gender:             domain.GenderWoman,
		Interests:          []string{"Travel", "Art", "Coffee"},
		RelationshipIntent: domain.IntentLongTerm,
		CommunicationStyle: domain.CommsThoughtful,
		PersonalityTraits:  []string{"curious", "warm", "creative"},
		PersonalityAnswers: []domain.PersonalityAnswer{{QuestionID: "weekend", Answer: "A"}},
		Preferences:        domain.DatingPreferences{MaxDistanceKm: 25},
		Lifestyle: domain.Lifestyle{
			Exercise: "Yoga most mornings", Drinking: "Socially", Smoking: "Never",
			Diet: "Everything", SleepSchedule: "Early-ish", Pets: "Cat person",
			Children: "Want someday", Religion: "Open / spiritual",
			Languages: []string{"English", "Mandarin"},
		},
	}
	return a, b
}
