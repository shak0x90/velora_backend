package profile

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shak0x90/velora_backend/internal/domain"
)

// An in-package test on purpose: what is worth pinning here is everything that
// runs before any SQL — validation, the date and age arithmetic, and the SET
// builder — and all of it is unexported. The query paths need a real Postgres
// and are covered by the acceptance run against the test server.

func ptr[T any](v T) *T { return &v }

func TestValidateAcceptsARealisticPatch(t *testing.T) {
	patch := Patch{
		FirstName:          ptr("Sam"),
		Bio:                ptr("Ceramics, long walks, and consistently bad puns."),
		Interests:          ptr([]string{"climbing", "ceramics"}),
		RelationshipIntent: ptr(domain.IntentLongTerm),
		CommunicationStyle: ptr(domain.CommsThoughtful),
		Preferences: &PreferencesPatch{
			InterestedIn:  ptr([]domain.Gender{domain.GenderWoman, domain.GenderNonBinary}),
			MinAge:        ptr(27),
			MaxAge:        ptr(38),
			MaxDistanceKm: ptr(40),
			Intents:       ptr([]domain.RelationshipIntent{domain.IntentLongTerm}),
		},
		Lifestyle: &LifestylePatch{Languages: ptr([]string{"English"}), HeightCm: ptr(170)},
		Prompts: ptr([]domain.ProfilePrompt{
			{ID: "p1_local", Question: "A perfect Sunday", Answer: "Kiln, then tacos."},
		}),
		PersonalityAnswers: ptr([]domain.PersonalityAnswer{
			{QuestionID: "q1", Question: "Recharge how?", Answer: "Alone"},
		}),
		// Server-derived fields ride along on a round-tripped Profile and must
		// not be a reason to reject the write.
		ID: ptr("ignored"), Age: ptr(99), PhotoVerified: ptr(true),
	}
	if err := patch.validate(); err != nil {
		t.Fatalf("expected the patch to validate, got %v", err)
	}
}

func TestValidateRejections(t *testing.T) {
	tests := map[string]struct {
		patch Patch
		want  string
	}{
		"blank first name": {
			Patch{FirstName: ptr("   ")}, "first name cannot be empty",
		},
		"unknown gender": {
			Patch{Gender: ptr(domain.Gender("androgynous"))}, "not a gender we recognise",
		},
		"unknown intent": {
			Patch{RelationshipIntent: ptr(domain.RelationshipIntent("marriage"))},
			"not an intent we recognise",
		},
		"unknown communication style": {
			Patch{CommunicationStyle: ptr(domain.CommunicationStyle("terse"))},
			"not a communication style we recognise",
		},
		"too many interests": {
			Patch{Interests: ptr(make([]string, maxInterests+1))}, "at most 30 interests",
		},
		"prompt with no question": {
			Patch{Prompts: ptr([]domain.ProfilePrompt{{Answer: "Tacos."}})}, "needs a question",
		},
		"duplicate personality answer": {
			Patch{PersonalityAnswers: ptr([]domain.PersonalityAnswer{
				{QuestionID: "q1", Answer: "Alone"},
				{QuestionID: "q1", Answer: "With people"},
			})},
			"answered twice",
		},
		"implausible height": {
			Patch{Lifestyle: &LifestylePatch{HeightCm: ptr(12)}}, "between 90 and 260 cm",
		},
		"age range below adulthood": {
			Patch{Preferences: &PreferencesPatch{MinAge: ptr(15)}}, "minAge must be between 18",
		},
		"inverted age range": {
			Patch{Preferences: &PreferencesPatch{MinAge: ptr(40), MaxAge: ptr(30)}},
			"cannot be above the oldest",
		},
		"distance out of range": {
			Patch{Preferences: &PreferencesPatch{MaxDistanceKm: ptr(0)}}, "between 1 and 500 km",
		},
		"latitude off the globe": {
			Patch{Lat: ptr(91.0)}, "between -90 and 90",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := test.patch.validate()
			if err == nil {
				t.Fatalf("expected a rejection mentioning %q", test.want)
			}
			var invalid ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("expected a ValidationError, got %T", err)
			}
			if !strings.Contains(invalid.Detail, test.want) {
				t.Fatalf("detail %q does not mention %q", invalid.Detail, test.want)
			}
		})
	}
}

func TestParseDateAcceptsBothWireForms(t *testing.T) {
	want := time.Date(1994, 3, 12, 0, 0, 0, 0, time.UTC)
	for _, raw := range []string{"1994-03-12", "1994-03-12T00:00:00Z", " 1994-03-12 "} {
		got, err := parseDate(raw)
		if err != nil {
			t.Fatalf("parseDate(%q): %v", raw, err)
		}
		if !got.Equal(want) {
			t.Fatalf("parseDate(%q) = %s, want %s", raw, got, want)
		}
	}
	if _, err := parseDate("12/03/1994"); err == nil {
		t.Fatal("expected a slash-separated date to be rejected")
	}
}

func TestAgeOnCountsCompletedYears(t *testing.T) {
	birth := time.Date(1994, 3, 12, 0, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		now  time.Time
		want int
	}{
		"day before the birthday": {time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC), 31},
		"on the birthday":         {time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC), 32},
		"day after":               {time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC), 32},
		"earlier month":           {time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), 31},
	}
	for name, test := range tests {
		if got := ageOn(birth, test.now); got != test.want {
			t.Errorf("%s: ageOn = %d, want %d", name, got, test.want)
		}
	}
}

// A leap-day birthday is what a day-of-year comparison gets wrong: in a
// non-leap year every date after February shifts by one.
func TestAgeOnHandlesLeapDayBirthdays(t *testing.T) {
	birth := time.Date(2000, 2, 29, 0, 0, 0, 0, time.UTC)
	if got := ageOn(birth, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); got != 26 {
		t.Errorf("ageOn just after a leap-day birthday = %d, want 26", got)
	}
	if got := ageOn(birth, time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)); got != 25 {
		t.Errorf("ageOn just before a leap-day birthday = %d, want 25", got)
	}
}

func TestStatusFromBuckets(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	tests := map[time.Duration]domain.OnlineStatus{
		30 * time.Second:    domain.StatusOnline,
		2 * time.Hour:       domain.StatusRecently,
		3 * 24 * time.Hour:  domain.StatusThisWeek,
		30 * 24 * time.Hour: domain.StatusOffline,
	}
	for ago, want := range tests {
		if got := statusFrom(now.Add(-ago), now); got != want {
			t.Errorf("%s ago = %q, want %q", ago, got, want)
		}
	}
}

func TestAssignmentsWritesOnlySuppliedColumns(t *testing.T) {
	set := &assignments{}
	set.addString("bio", ptr("  hello  "))
	set.addString("city", nil)
	set.addStrings("interests", ptr([]string{"climbing", "", "climbing", "ceramics"}))

	statement, args := set.statement("user-1")

	if strings.Contains(statement, "city") {
		t.Fatalf("an absent field was written: %s", statement)
	}
	want := "update profiles set bio = $1, interests = $2, updated_at = now() where user_id = $3"
	if statement != want {
		t.Fatalf("statement = %q, want %q", statement, want)
	}
	if args[0] != "hello" {
		t.Errorf("string values should be trimmed, got %q", args[0])
	}
	interests, ok := args[1].([]string)
	if !ok || len(interests) != 2 || interests[0] != "climbing" || interests[1] != "ceramics" {
		t.Errorf("tag lists should drop blanks and duplicates in order, got %v", args[1])
	}
	if args[2] != "user-1" {
		t.Errorf("the user id must be the last argument, got %v", args[2])
	}
}
