package compat

import (
	"sort"
	"strings"

	"github.com/shak0x90/velora_backend/internal/domain"
)

// Profile-completion weights, matching ProfileCompletionWeights in Dart/TS.
const (
	CompletionBasics       = 0.20
	CompletionPhotos       = 0.25
	CompletionBio          = 0.15
	CompletionPrompts      = 0.15
	CompletionInterests    = 0.10
	CompletionPreferences  = 0.10
	CompletionVerification = 0.05
)

// PassesFilters applies the hard filters. This runs in Go rather than SQL on a
// candidate set the query has already narrowed — expressing all fifteen
// predicates in SQL would fight the planner for no gain at this scale.
func PassesFilters(p domain.Profile, f domain.DiscoverFilters, current domain.Profile) bool {
	if p.Hidden {
		return false
	}
	if p.Age < f.MinAge || p.Age > f.MaxAge {
		return false
	}
	if p.DistanceKm > float64(f.MaxDistanceKm) {
		return false
	}
	if len(f.Genders) > 0 && !containsGender(f.Genders, p.Gender) {
		return false
	}
	if len(f.Intents) > 0 && !containsIntent(f.Intents, p.RelationshipIntent) {
		return false
	}
	if len(f.Interests) > 0 && !sharesAny(f.Interests, p.Interests) {
		return false
	}
	if f.Education != nil && !containsFold(p.Education, *f.Education) {
		return false
	}
	if f.Occupation != nil && !containsFold(p.Occupation, *f.Occupation) {
		return false
	}
	if f.Smoking != nil && p.Lifestyle.Smoking != *f.Smoking {
		return false
	}
	if f.Drinking != nil && p.Lifestyle.Drinking != *f.Drinking {
		return false
	}
	if f.Children != nil && p.Lifestyle.Children != *f.Children {
		return false
	}
	if f.Religion != nil && !containsFold(p.Lifestyle.Religion, *f.Religion) {
		return false
	}
	if len(f.Languages) > 0 && !sharesAny(f.Languages, p.Lifestyle.Languages) {
		return false
	}
	if h := p.Lifestyle.HeightCm; h != nil {
		if f.MinHeightCm != nil && *h < *f.MinHeightCm {
			return false
		}
		if f.MaxHeightCm != nil && *h > *f.MaxHeightCm {
			return false
		}
	}
	// Fall back to the viewer's own stated preference only when no explicit
	// gender filter is set — mirrors the Dart behavior exactly.
	if !containsGender(current.Preferences.InterestedIn, p.Gender) && len(f.Genders) == 0 {
		return false
	}
	return true
}

func containsGender(list []domain.Gender, v domain.Gender) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func containsIntent(list []domain.RelationshipIntent, v domain.RelationshipIntent) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func roughCompleteness(p domain.Profile) float64 {
	value := 0.0
	if len(p.Photos) >= 3 {
		value += 0.3
	}
	if len(p.Bio) > 40 {
		value += 0.2
	}
	if len(p.Prompts) >= 2 {
		value += 0.2
	}
	if len(p.Interests) >= 4 {
		value += 0.2
	}
	if p.PhotoVerified {
		value += 0.1
	}
	return value
}

// rankScore layers engagement signals on top of raw compatibility so an
// identical score from an active, verified, complete profile ranks higher.
func rankScore(current, candidate domain.Profile, overall, shared int) float64 {
	score := float64(overall)
	if current.RelationshipIntent == candidate.RelationshipIntent {
		score += 6
	}
	score += float64(shared) * 1.4

	proximity := 100 - candidate.DistanceKm*1.2
	if proximity < 0 {
		proximity = 0
	}
	if proximity > 12 {
		proximity = 12
	}
	score += proximity

	if candidate.PhotoVerified {
		score += 4
	}
	if candidate.PhoneVerified {
		score += 2
	}
	switch candidate.OnlineStatus {
	case domain.StatusOnline:
		score += 5
	case domain.StatusRecently:
		score += 3
	case domain.StatusThisWeek:
		score += 1
	}
	score += roughCompleteness(candidate) * 8
	return score
}

// Recommend filters, scores, and ranks. Sorting is stable and falls back to ID
// so equal scores keep a deterministic order across requests.
func Recommend(current domain.Profile, candidates []domain.Profile, f domain.DiscoverFilters) []domain.RankedProfile {
	out := make([]domain.RankedProfile, 0, len(candidates))
	for _, candidate := range candidates {
		if !PassesFilters(candidate, f, current) {
			continue
		}
		result := Calculate(current, candidate)
		out = append(out, domain.RankedProfile{
			Profile:       candidate,
			Compatibility: result,
			RankScore:     rankScore(current, candidate, result.OverallScore, len(result.CommonInterests)),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RankScore != out[j].RankScore {
			return out[i].RankScore > out[j].RankScore
		}
		return out[i].Profile.ID < out[j].Profile.ID
	})
	return out
}

// DailyPicks is the top of the ranked feed. The scheduled job that freezes
// these for the day will call this and persist the resulting IDs.
func DailyPicks(current domain.Profile, candidates []domain.Profile, limit int) []domain.RankedProfile {
	ranked := Recommend(current, candidates, domain.DefaultFilters())
	if limit > len(ranked) {
		limit = len(ranked)
	}
	return ranked[:limit]
}

// CompletionAdvice powers the "profile strength" meter and its nudges.
type CompletionAdvice struct {
	Percent int      `json:"percent"`
	Tips    []string `json:"tips"`
}

func EvaluateCompletion(p domain.Profile) CompletionAdvice {
	score := 0.0
	tips := []string{}

	basicsReady := p.FirstName != "" && p.Occupation != "" && p.Education != ""
	if basicsReady {
		score += CompletionBasics
	} else {
		tips = append(tips, "Finish your basic details.")
	}

	score += minFloat(1, float64(len(p.Photos))/4) * CompletionPhotos
	if len(p.Photos) < 4 {
		tips = append(tips, "Add another photo so people can get a fuller read.")
	}

	if len(strings.TrimSpace(p.Bio)) >= 40 {
		score += CompletionBio
	} else {
		score += CompletionBio * 0.4
		tips = append(tips, "Give your bio a little more room to breathe.")
	}

	score += minFloat(1, float64(len(p.Prompts))/3) * CompletionPrompts
	if len(p.Prompts) < 3 {
		tips = append(tips, "Add one more prompt to start better conversations.")
	}

	score += minFloat(1, float64(len(p.Interests))/6) * CompletionInterests
	if len(p.Interests) < 6 {
		tips = append(tips, "A few more interests help the compatibility read.")
	}

	score += CompletionPreferences

	if p.PhotoVerified || p.PhoneVerified {
		score += CompletionVerification
	} else {
		tips = append(tips, "Verify your photo to help people feel at ease.")
	}

	if len(tips) > 3 {
		tips = tips[:3]
	}
	return CompletionAdvice{Percent: toScore(score), Tips: tips}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
