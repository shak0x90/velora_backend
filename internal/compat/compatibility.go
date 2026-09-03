// Package compat is the deterministic compatibility engine.
//
// It is a line-for-line port of `CompatibilityService` from the Flutter client
// (lib/services/compatibility_service.dart) and its TypeScript twin in the web
// client. Determinism is the product claim — "we tell you why" only works if
// the same two people always score the same — so this package is verified
// against golden vectors generated from the Dart implementation rather than
// re-derived by hand. See compatibility_test.go.
//
// Import rule: standard library plus internal/domain. No database, no HTTP,
// no clock. Keep it that way; it is what makes this testable.
package compat

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/shak0x90/velora_backend/internal/domain"
)

// Weights must stay identical across Dart, TypeScript, and Go.
// Changing one changes every score in the product.
const (
	WeightPersonality   = 0.25
	WeightLifestyle     = 0.20
	WeightInterests     = 0.20
	WeightRelationship  = 0.20
	WeightCommunication = 0.10
	WeightLocation      = 0.05
)

func toScore(v float64) int {
	return clampInt(int(math.Round(v*100)), 0, 100)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

// jaccard returns 0.7 for empty inputs, matching the Dart fallback — a neutral
// score rather than zero, so a sparse profile is not punished as a mismatch.
func jaccard(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0.7
	}
	left := lowerSet(a)
	right := lowerSet(b)
	union := make(map[string]struct{}, len(left)+len(right))
	for k := range left {
		union[k] = struct{}{}
	}
	for k := range right {
		union[k] = struct{}{}
	}
	if len(union) == 0 {
		return 0.7
	}
	shared := 0
	for k := range left {
		if _, ok := right[k]; ok {
			shared++
		}
	}
	return float64(shared) / float64(len(union))
}

func lowerSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[strings.ToLower(v)] = struct{}{}
	}
	return out
}

func answerOverlap(a, b domain.Profile) float64 {
	if len(a.PersonalityAnswers) == 0 || len(b.PersonalityAnswers) == 0 {
		return 0.7
	}
	byID := make(map[string]string, len(b.PersonalityAnswers))
	for _, ans := range b.PersonalityAnswers {
		byID[ans.QuestionID] = ans.Answer
	}
	same, compared := 0, 0
	for _, ans := range a.PersonalityAnswers {
		other, ok := byID[ans.QuestionID]
		if !ok {
			continue
		}
		compared++
		if other == ans.Answer {
			same++
		}
	}
	if compared == 0 {
		return 0.7
	}
	return 0.45 + (float64(same)/float64(compared))*0.55
}

// synonyms let lifestyle answers that mean the same thing match even when the
// wording differs ("Socially" vs "Wine with dinner"). Order and contents are
// copied from the Dart source; adding a group changes scores.
var synonyms = [][]string{
	{"socially", "occasionally", "wine with dinner"},
	{"rarely", "never"},
	{"early bird", "early-ish", "early after shows"},
	{"night owl"},
	{"dog person", "both"},
	{"cat person", "both"},
	{"want someday", "have and open to more"},
}

func softMatch(a, b string) bool {
	left := strings.ToLower(a)
	right := strings.ToLower(b)
	if left == right {
		return true
	}
	for _, group := range synonyms {
		inLeft, inRight := false, false
		for _, word := range group {
			if strings.Contains(left, word) {
				inLeft = true
			}
			if strings.Contains(right, word) {
				inRight = true
			}
		}
		if inLeft && inRight {
			return true
		}
	}
	return false
}

func PersonalityScore(a, b domain.Profile) int {
	traits := jaccard(a.PersonalityTraits, b.PersonalityTraits)
	return toScore(traits*0.55 + answerOverlap(a, b)*0.45)
}

func LifestyleScore(a, b domain.Profile) int {
	mapA := a.Lifestyle.AsMap()
	mapB := b.Lifestyle.AsMap()
	matches, compared := 0, 0
	for key, valueA := range mapA {
		compared++
		if softMatch(valueA, mapB[key]) {
			matches++
		}
	}
	bonus := 0.0
	if sharesAny(a.Lifestyle.Languages, b.Lifestyle.Languages) {
		bonus = 0.08
	}
	raw := 0.7
	if compared != 0 {
		raw = float64(matches) / float64(compared)
	}
	return toScore(clamp01(raw + bonus))
}

func sharesAny(a, b []string) bool {
	set := make(map[string]struct{}, len(b))
	for _, v := range b {
		set[v] = struct{}{}
	}
	for _, v := range a {
		if _, ok := set[v]; ok {
			return true
		}
	}
	return false
}

func InterestScore(a, b domain.Profile) int {
	return toScore(jaccard(a.Interests, b.Interests))
}

var intentNeighbors = map[domain.RelationshipIntent][]domain.RelationshipIntent{
	domain.IntentLongTerm:       {domain.IntentSeriousOpen},
	domain.IntentSeriousOpen:    {domain.IntentLongTerm, domain.IntentNotSure},
	domain.IntentCasual:         {domain.IntentNewConnections, domain.IntentNotSure},
	domain.IntentNewConnections: {domain.IntentCasual, domain.IntentNotSure},
	domain.IntentNotSure:        {domain.IntentSeriousOpen, domain.IntentNewConnections},
}

func RelationshipScore(a, b domain.Profile) int {
	if a.RelationshipIntent == b.RelationshipIntent {
		return 98
	}
	for _, neighbor := range intentNeighbors[a.RelationshipIntent] {
		if neighbor == b.RelationshipIntent {
			return 78
		}
	}
	return 42
}

var commsCompatible = map[domain.CommunicationStyle][]domain.CommunicationStyle{
	domain.CommsThoughtful: {domain.CommsSlowBurn, domain.CommsDirect},
	domain.CommsPlayful:    {domain.CommsFrequent, domain.CommsDirect},
	domain.CommsDirect:     {domain.CommsThoughtful, domain.CommsPlayful},
	domain.CommsSlowBurn:   {domain.CommsThoughtful},
	domain.CommsFrequent:   {domain.CommsPlayful},
}

func CommunicationScore(a, b domain.Profile) int {
	if a.CommunicationStyle == b.CommunicationStyle {
		return 94
	}
	for _, style := range commsCompatible[a.CommunicationStyle] {
		if style == b.CommunicationStyle {
			return 82
		}
	}
	return 58
}

// LocationScore is asymmetric on purpose: it is scored against the *viewer's*
// max distance, so the same pair can read differently in each direction.
func LocationScore(current, candidate domain.Profile) int {
	max := float64(current.Preferences.MaxDistanceKm)
	distance := candidate.DistanceKm
	switch {
	case distance <= max*0.35:
		return 96
	case distance <= max:
		ratio := 1 - distance/(max+0.01)
		return toScore(0.7 + ratio*0.25)
	case distance <= max*1.4:
		return 62
	default:
		return 40
	}
}

// CommonInterests preserves exact case and sorts, matching Dart's
// `generateCommonInterests`. The sort makes the output stable for goldens.
func CommonInterests(a, b domain.Profile) []string {
	set := make(map[string]struct{}, len(b.Interests))
	for _, v := range b.Interests {
		set[v] = struct{}{}
	}
	out := []string{}
	for _, v := range a.Interests {
		if _, ok := set[v]; ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func sharedTraits(a, b domain.Profile) string {
	set := make(map[string]struct{}, len(b.PersonalityTraits))
	for _, v := range b.PersonalityTraits {
		set[v] = struct{}{}
	}
	var out []string
	for _, v := range a.PersonalityTraits {
		if _, ok := set[v]; ok {
			out = append(out, v)
			if len(out) == 2 {
				break
			}
		}
	}
	return strings.Join(out, " + ")
}

var intentShortLabel = map[domain.RelationshipIntent]string{
	domain.IntentLongTerm:       "Long-term",
	domain.IntentSeriousOpen:    "Serious",
	domain.IntentCasual:         "Casual",
	domain.IntentNewConnections: "New connections",
	domain.IntentNotSure:        "Exploring",
}

func buildInsights(current, candidate domain.Profile, shared []string, relationship, communication, location, lifestyle int) []string {
	insights := []string{}

	if relationship >= 90 {
		insights = append(insights, fmt.Sprintf(
			"You both want a %s connection",
			strings.ToLower(intentShortLabel[candidate.RelationshipIntent]),
		))
	} else if relationship >= 70 {
		insights = append(insights, "Your dating intents sit comfortably next to each other")
	}
	if len(shared) == 1 {
		insights = append(insights, fmt.Sprintf("You both enjoy %s", shared[0]))
	} else if len(shared) > 1 {
		insights = append(insights, fmt.Sprintf("You share %d interests", len(shared)))
	}
	if communication >= 80 {
		insights = append(insights, "Similar communication preferences")
	}
	if lifestyle >= 75 {
		insights = append(insights, "Your day-to-day lifestyles line up")
	}
	if location >= 80 {
		insights = append(insights, fmt.Sprintf(
			"%.0f km away — within your range", candidate.DistanceKm,
		))
	}
	if traits := sharedTraits(current, candidate); traits != "" {
		insights = append(insights, "Overlapping personality notes: "+traits)
	}
	if len(insights) > 5 {
		insights = insights[:5]
	}
	return insights
}

func buildSummary(name string, score int, shared []string) string {
	if len(shared) >= 3 {
		return fmt.Sprintf(
			"You and %s share %s — and a %d%% compatibility read.",
			name, strings.ToLower(strings.Join(shared[:3], ", ")), score,
		)
	}
	if len(shared) > 0 {
		return fmt.Sprintf(
			"You both love %s, and the rest of your profiles agree more than they argue.",
			strings.ToLower(shared[0]),
		)
	}
	return "Your values and pacing are closer than your hobbies suggest."
}

// Calculate scores candidate from current's point of view. Pure: same inputs
// always produce the same result, with no dependency on wall-clock time.
func Calculate(current, candidate domain.Profile) domain.CompatibilityResult {
	personality := PersonalityScore(current, candidate)
	lifestyle := LifestyleScore(current, candidate)
	interests := InterestScore(current, candidate)
	relationship := RelationshipScore(current, candidate)
	communication := CommunicationScore(current, candidate)
	location := LocationScore(current, candidate)
	shared := CommonInterests(current, candidate)

	overall := clampInt(int(math.Round(
		float64(personality)*WeightPersonality+
			float64(lifestyle)*WeightLifestyle+
			float64(interests)*WeightInterests+
			float64(relationship)*WeightRelationship+
			float64(communication)*WeightCommunication+
			float64(location)*WeightLocation,
	)), 0, 100)

	return domain.CompatibilityResult{
		OverallScore:       overall,
		PersonalityScore:   personality,
		LifestyleScore:     lifestyle,
		InterestScore:      interests,
		RelationshipScore:  relationship,
		CommunicationScore: communication,
		LocationScore:      location,
		CommonInterests:    shared,
		Insights:           buildInsights(current, candidate, shared, relationship, communication, location, lifestyle),
		Summary:            buildSummary(candidate.FirstName, overall, shared),
	}
}
