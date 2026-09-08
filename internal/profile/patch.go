package profile

import (
	"fmt"
	"strings"

	"github.com/shak0x90/velora_backend/internal/domain"
)

// ValidationError is a rejected patch. It is separate from httpx.Problem so
// this half of the package stays free of HTTP; handlers.go maps it to a 400.
type ValidationError struct{ Detail string }

func (e ValidationError) Error() string { return e.Detail }

// Patch is a partial profile. Every field is a pointer so "absent" and "set to
// the zero value" are distinguishable — without that, omitting `bio` and
// clearing it would produce the same write.
//
// The field set mirrors the writable half of domain.Profile. Derived fields
// are declared but ignored rather than rejected: both clients type their patch
// as a partial Profile, so those keys arrive as a matter of course, and
// httpx.Decode turns any undeclared field into a 400.
type Patch struct {
	FirstName          *string                     `json:"firstName"`
	DateOfBirth        *string                     `json:"dateOfBirth"`
	Gender             *domain.Gender              `json:"gender"`
	Pronouns           *string                     `json:"pronouns"`
	City               *string                     `json:"city"`
	Neighborhood       *string                     `json:"neighborhood"`
	Lat                *float64                    `json:"lat"`
	Lng                *float64                    `json:"lng"`
	Occupation         *string                     `json:"occupation"`
	Education          *string                     `json:"education"`
	Bio                *string                     `json:"bio"`
	Photos             *[]string                   `json:"photos"`
	Interests          *[]string                   `json:"interests"`
	Prompts            *[]domain.ProfilePrompt     `json:"prompts"`
	RelationshipIntent *domain.RelationshipIntent  `json:"relationshipIntent"`
	Preferences        *PreferencesPatch           `json:"preferences"`
	Lifestyle          *LifestylePatch             `json:"lifestyle"`
	CommunicationStyle *domain.CommunicationStyle  `json:"communicationStyle"`
	PersonalityTraits  *[]string                   `json:"personalityTraits"`
	PersonalityAnswers *[]domain.PersonalityAnswer `json:"personalityAnswers"`
	Incognito          *bool                       `json:"incognito"`
	Hidden             *bool                       `json:"hidden"`

	// Server-derived. Declared so a round-tripped Profile decodes, ignored on
	// write — a client cannot promote itself to verified or age itself down.
	ID            *string  `json:"id"`
	Age           *int     `json:"age"`
	DistanceKm    *float64 `json:"distanceKm"`
	PhotoVerified *bool    `json:"photoVerified"`
	PhoneVerified *bool    `json:"phoneVerified"`
	OnlineStatus  *string  `json:"onlineStatus"`
	LastActive    *string  `json:"lastActive"`
}

type PreferencesPatch struct {
	InterestedIn  *[]domain.Gender             `json:"interestedIn"`
	MinAge        *int                         `json:"minAge"`
	MaxAge        *int                         `json:"maxAge"`
	MaxDistanceKm *int                         `json:"maxDistanceKm"`
	Intents       *[]domain.RelationshipIntent `json:"intents"`
}

type LifestylePatch struct {
	Exercise      *string   `json:"exercise"`
	Drinking      *string   `json:"drinking"`
	Smoking       *string   `json:"smoking"`
	Diet          *string   `json:"diet"`
	SleepSchedule *string   `json:"sleepSchedule"`
	Pets          *string   `json:"pets"`
	Children      *string   `json:"children"`
	Religion      *string   `json:"religion"`
	Languages     *[]string `json:"languages"`
	HeightCm      *int      `json:"heightCm"`
}

// Limits the API enforces. The database has its own constraints for the things
// that must never be wrong (age, uniqueness); these are the product ones, and
// they exist mostly so a broken client cannot fill a column with a megabyte.
const (
	maxFirstNameLen  = 50
	maxBioLen        = 2000
	maxTagLen        = 60
	maxInterests     = 30
	maxTraits        = 20
	maxLanguages     = 15
	maxPrompts       = 6
	maxPromptLen     = 400
	maxAnswers       = 20
	minHeightCm      = 90
	maxHeightCm      = 260
	minPreferredAge  = 18
	maxPreferredAge  = 120
	maxPreferredDist = 500
)

var (
	validGenders = map[domain.Gender]bool{
		domain.GenderWoman: true, domain.GenderMan: true,
		domain.GenderNonBinary: true, domain.GenderOther: true,
	}
	validIntents = map[domain.RelationshipIntent]bool{
		domain.IntentLongTerm: true, domain.IntentSeriousOpen: true,
		domain.IntentCasual: true, domain.IntentNewConnections: true,
		domain.IntentNotSure: true,
	}
	validStyles = map[domain.CommunicationStyle]bool{
		domain.CommsThoughtful: true, domain.CommsPlayful: true,
		domain.CommsDirect: true, domain.CommsSlowBurn: true,
		domain.CommsFrequent: true,
	}
)

// validate checks everything before a single column is written, so a rejected
// patch leaves the row exactly as it was.
func (p Patch) validate() error {
	if p.FirstName != nil {
		name := strings.TrimSpace(*p.FirstName)
		if name == "" {
			return ValidationError{Detail: "Your first name cannot be empty."}
		}
		if len([]rune(name)) > maxFirstNameLen {
			return ValidationError{
				Detail: fmt.Sprintf("Keep your first name under %d characters.", maxFirstNameLen),
			}
		}
	}
	if p.Gender != nil && !validGenders[*p.Gender] {
		return ValidationError{Detail: fmt.Sprintf("%q is not a gender we recognise.", *p.Gender)}
	}
	if p.RelationshipIntent != nil && !validIntents[*p.RelationshipIntent] {
		return ValidationError{
			Detail: fmt.Sprintf("%q is not an intent we recognise.", *p.RelationshipIntent),
		}
	}
	if p.CommunicationStyle != nil && !validStyles[*p.CommunicationStyle] {
		return ValidationError{
			Detail: fmt.Sprintf("%q is not a communication style we recognise.", *p.CommunicationStyle),
		}
	}
	if p.Bio != nil && len([]rune(*p.Bio)) > maxBioLen {
		return ValidationError{
			Detail: fmt.Sprintf("Your bio is longer than %d characters.", maxBioLen),
		}
	}
	if err := checkTags("interests", p.Interests, maxInterests); err != nil {
		return err
	}
	if err := checkTags("personality traits", p.PersonalityTraits, maxTraits); err != nil {
		return err
	}
	if p.Lat != nil && (*p.Lat < -90 || *p.Lat > 90) {
		return ValidationError{Detail: "Latitude must be between -90 and 90."}
	}
	if p.Lng != nil && (*p.Lng < -180 || *p.Lng > 180) {
		return ValidationError{Detail: "Longitude must be between -180 and 180."}
	}
	if err := p.validatePrompts(); err != nil {
		return err
	}
	if err := p.validateAnswers(); err != nil {
		return err
	}
	if l := p.Lifestyle; l != nil {
		if err := checkTags("languages", l.Languages, maxLanguages); err != nil {
			return err
		}
		if l.HeightCm != nil && (*l.HeightCm < minHeightCm || *l.HeightCm > maxHeightCm) {
			return ValidationError{
				Detail: fmt.Sprintf("Height must be between %d and %d cm.", minHeightCm, maxHeightCm),
			}
		}
	}
	return p.validatePreferences()
}

func (p Patch) validatePrompts() error {
	if p.Prompts == nil {
		return nil
	}
	if len(*p.Prompts) > maxPrompts {
		return ValidationError{
			Detail: fmt.Sprintf("You can have at most %d prompts.", maxPrompts),
		}
	}
	for _, prompt := range *p.Prompts {
		if strings.TrimSpace(prompt.Question) == "" {
			return ValidationError{Detail: "Every prompt needs a question."}
		}
		if len([]rune(prompt.Answer)) > maxPromptLen {
			return ValidationError{
				Detail: fmt.Sprintf("Prompt answers are limited to %d characters.", maxPromptLen),
			}
		}
	}
	return nil
}

func (p Patch) validateAnswers() error {
	if p.PersonalityAnswers == nil {
		return nil
	}
	if len(*p.PersonalityAnswers) > maxAnswers {
		return ValidationError{
			Detail: fmt.Sprintf("You can have at most %d personality answers.", maxAnswers),
		}
	}
	seen := make(map[string]bool, len(*p.PersonalityAnswers))
	for _, answer := range *p.PersonalityAnswers {
		id := strings.TrimSpace(answer.QuestionID)
		if id == "" {
			return ValidationError{Detail: "Every personality answer needs a questionId."}
		}
		// The table is keyed on (user_id, question_id), so a duplicate would
		// silently overwrite its twin instead of failing.
		if seen[id] {
			return ValidationError{Detail: fmt.Sprintf("Question %q was answered twice.", id)}
		}
		seen[id] = true
	}
	return nil
}

func (p Patch) validatePreferences() error {
	prefs := p.Preferences
	if prefs == nil {
		return nil
	}

	if prefs.InterestedIn != nil {
		for _, gender := range *prefs.InterestedIn {
			if !validGenders[gender] {
				return ValidationError{
					Detail: fmt.Sprintf("%q is not a gender we recognise.", gender),
				}
			}
		}
	}
	if prefs.Intents != nil {
		for _, intent := range *prefs.Intents {
			if !validIntents[intent] {
				return ValidationError{
					Detail: fmt.Sprintf("%q is not an intent we recognise.", intent),
				}
			}
		}
	}
	if prefs.MinAge != nil && !inAgeRange(*prefs.MinAge) {
		return ValidationError{Detail: ageRangeDetail("minAge")}
	}
	if prefs.MaxAge != nil && !inAgeRange(*prefs.MaxAge) {
		return ValidationError{Detail: ageRangeDetail("maxAge")}
	}
	// Only comparable when both arrive together. A one-sided change that
	// inverts the range against the stored value matches nobody rather than
	// corrupting anything, and the preferences screen always sends the pair.
	if prefs.MinAge != nil && prefs.MaxAge != nil && *prefs.MinAge > *prefs.MaxAge {
		return ValidationError{Detail: "The youngest age you'll see cannot be above the oldest."}
	}
	if prefs.MaxDistanceKm != nil &&
		(*prefs.MaxDistanceKm < 1 || *prefs.MaxDistanceKm > maxPreferredDist) {
		return ValidationError{
			Detail: fmt.Sprintf("Distance must be between 1 and %d km.", maxPreferredDist),
		}
	}
	return nil
}

func inAgeRange(value int) bool {
	return value >= minPreferredAge && value <= maxPreferredAge
}

func ageRangeDetail(field string) string {
	return fmt.Sprintf("%s must be between %d and %d.", field, minPreferredAge, maxPreferredAge)
}

func checkTags(label string, values *[]string, limit int) error {
	if values == nil {
		return nil
	}
	if len(*values) > limit {
		return ValidationError{Detail: fmt.Sprintf("You can have at most %d %s.", limit, label)}
	}
	for _, value := range *values {
		if len([]rune(value)) > maxTagLen {
			return ValidationError{
				Detail: fmt.Sprintf("Each entry in %s must be under %d characters.", label, maxTagLen),
			}
		}
	}
	return nil
}
