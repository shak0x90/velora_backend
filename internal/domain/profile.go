// Package domain holds the shapes the whole service agrees on.
//
// These mirror the Dart models in the Flutter client and the TypeScript types
// in the web client field for field. Enum values are the exact Dart enum names
// so the JSON wire format is identical across all three.
//
// Nothing here imports anything outside the standard library — the scoring
// engine depends on this package and must stay pure.
package domain

import "time"

type Gender string

const (
	GenderWoman     Gender = "woman"
	GenderMan       Gender = "man"
	GenderNonBinary Gender = "nonBinary"
	GenderOther     Gender = "other"
)

type RelationshipIntent string

const (
	IntentLongTerm       RelationshipIntent = "longTerm"
	IntentSeriousOpen    RelationshipIntent = "seriousOpen"
	IntentCasual         RelationshipIntent = "casual"
	IntentNewConnections RelationshipIntent = "newConnections"
	IntentNotSure        RelationshipIntent = "notSure"
)

type CommunicationStyle string

const (
	CommsThoughtful CommunicationStyle = "thoughtful"
	CommsPlayful    CommunicationStyle = "playful"
	CommsDirect     CommunicationStyle = "direct"
	CommsSlowBurn   CommunicationStyle = "slowBurn"
	CommsFrequent   CommunicationStyle = "frequent"
)

type OnlineStatus string

const (
	StatusOnline   OnlineStatus = "online"
	StatusRecently OnlineStatus = "recently"
	StatusThisWeek OnlineStatus = "thisWeek"
	StatusOffline  OnlineStatus = "offline"
)

type AuthMethod string

const (
	AuthPhone  AuthMethod = "phone"
	AuthEmail  AuthMethod = "email"
	AuthGoogle AuthMethod = "google"
	AuthApple  AuthMethod = "apple"
)

type PersonalityAnswer struct {
	QuestionID string `json:"questionId"`
	Question   string `json:"question"`
	Answer     string `json:"answer"`
}

type ProfilePrompt struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

type Lifestyle struct {
	Exercise      string   `json:"exercise"`
	Drinking      string   `json:"drinking"`
	Smoking       string   `json:"smoking"`
	Diet          string   `json:"diet"`
	SleepSchedule string   `json:"sleepSchedule"`
	Pets          string   `json:"pets"`
	Children      string   `json:"children"`
	Religion      string   `json:"religion"`
	Languages     []string `json:"languages"`
	HeightCm      *int     `json:"heightCm,omitempty"`
}

// AsMap returns the eight fields the lifestyle sub-score compares, keyed
// exactly as Dart's `Lifestyle.asMap` does. The key set is load-bearing:
// the score is matches/len(map), so adding a key changes every score.
func (l Lifestyle) AsMap() map[string]string {
	return map[string]string{
		"exercise": l.Exercise,
		"drinking": l.Drinking,
		"smoking":  l.Smoking,
		"diet":     l.Diet,
		"sleep":    l.SleepSchedule,
		"pets":     l.Pets,
		"children": l.Children,
		"religion": l.Religion,
	}
}

type DatingPreferences struct {
	InterestedIn  []Gender             `json:"interestedIn"`
	MinAge        int                  `json:"minAge"`
	MaxAge        int                  `json:"maxAge"`
	MaxDistanceKm int                  `json:"maxDistanceKm"`
	Intents       []RelationshipIntent `json:"intents"`
}

type Profile struct {
	ID        string `json:"id"`
	FirstName string `json:"firstName"`
	Age       int    `json:"age"`
	// DateOfBirth appears only on your own profile. Age is what a viewer
	// needs; the exact date belongs to its owner, and Public strips it.
	DateOfBirth        *time.Time          `json:"dateOfBirth,omitempty"`
	Gender             Gender              `json:"gender"`
	Pronouns           string              `json:"pronouns"`
	City               string              `json:"city"`
	Neighborhood       string              `json:"neighborhood"`
	DistanceKm         float64             `json:"distanceKm"`
	Occupation         string              `json:"occupation"`
	Education          string              `json:"education"`
	Bio                string              `json:"bio"`
	Photos             []string            `json:"photos"`
	Interests          []string            `json:"interests"`
	Prompts            []ProfilePrompt     `json:"prompts"`
	RelationshipIntent RelationshipIntent  `json:"relationshipIntent"`
	Preferences        DatingPreferences   `json:"preferences"`
	Lifestyle          Lifestyle           `json:"lifestyle"`
	CommunicationStyle CommunicationStyle  `json:"communicationStyle"`
	PersonalityTraits  []string            `json:"personalityTraits"`
	PersonalityAnswers []PersonalityAnswer `json:"personalityAnswers"`
	PhotoVerified      bool                `json:"photoVerified"`
	PhoneVerified      bool                `json:"phoneVerified"`
	OnlineStatus       OnlineStatus        `json:"onlineStatus"`
	// LastActive appears only on your own profile, for the same reason as
	// DateOfBirth. OnlineStatus carries the coarse version everyone else sees.
	LastActive *time.Time `json:"lastActive,omitempty"`
	Incognito  bool       `json:"incognito,omitempty"`
	Hidden     bool       `json:"hidden,omitempty"`
}

// Public is the profile as somebody else may see it.
//
// Age and OnlineStatus already answer the questions a viewer has: how old
// someone is, and roughly whether they are around. An exact birth date and the
// precise minute of a last visit answer nothing anyone browsing needs, and both
// are the kind of detail that turns identifying the moment it is combined with
// anything else — a birth date is a permanent, unchangeable key to a person.
//
// Incognito and Hidden go too. They are the owner's settings rather than facts
// about them, and leaking "this person is browsing invisibly" would defeat the
// setting by announcing it.
//
// This is a copy, not a mutation: the caller keeps the full profile it loaded,
// which matters because the compatibility engine runs before serialisation.
func (p Profile) Public() Profile {
	p.DateOfBirth = nil
	p.LastActive = nil
	p.Incognito = false
	p.Hidden = false
	return p
}

// CompatibilityResult is what the client renders as "why this match".
// Insights and Summary are generated server-side so all clients agree.
type CompatibilityResult struct {
	OverallScore       int      `json:"overallScore"`
	PersonalityScore   int      `json:"personalityScore"`
	LifestyleScore     int      `json:"lifestyleScore"`
	InterestScore      int      `json:"interestScore"`
	RelationshipScore  int      `json:"relationshipScore"`
	CommunicationScore int      `json:"communicationScore"`
	LocationScore      int      `json:"locationScore"`
	CommonInterests    []string `json:"commonInterests"`
	Insights           []string `json:"insights"`
	Summary            string   `json:"summary"`
}

type RankedProfile struct {
	Profile       Profile             `json:"profile"`
	Compatibility CompatibilityResult `json:"compatibility"`
	RankScore     float64             `json:"rankScore"`
}

type DiscoverFilters struct {
	MinAge        int                  `json:"minAge"`
	MaxAge        int                  `json:"maxAge"`
	MaxDistanceKm int                  `json:"maxDistanceKm"`
	Genders       []Gender             `json:"genders"`
	Intents       []RelationshipIntent `json:"intents"`
	Interests     []string             `json:"interests"`
	Education     *string              `json:"education,omitempty"`
	Occupation    *string              `json:"occupation,omitempty"`
	Smoking       *string              `json:"smoking,omitempty"`
	Drinking      *string              `json:"drinking,omitempty"`
	Children      *string              `json:"children,omitempty"`
	Religion      *string              `json:"religion,omitempty"`
	Languages     []string             `json:"languages"`
	MinHeightCm   *int                 `json:"minHeightCm,omitempty"`
	MaxHeightCm   *int                 `json:"maxHeightCm,omitempty"`
}

// DefaultFilters matches `emptyFilters` in the web client and
// `DiscoverFilters()` in Dart.
func DefaultFilters() DiscoverFilters {
	return DiscoverFilters{MinAge: 21, MaxAge: 40, MaxDistanceKm: 40}
}
