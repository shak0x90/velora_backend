package compat

import (
	"fmt"
	"strings"

	"github.com/shak0x90/velora_backend/internal/domain"
)

// Conversation openers — a port of `lib/velora/compat/suggestions.ts` and the
// Dart ConversationSuggestionService it came from.
//
// These are templates, not generation: nothing here writes as the user or
// sends anything. It proposes questions a person can choose, edit, or ignore,
// which is why it is a lookup table rather than a model call.

// sharedTemplates is keyed by interest, using the exact labels in the clients'
// INTERESTS list — a key that matches no interest simply never fires, so the
// two lists have to stay in step.
var sharedTemplates = map[string][]domain.ConversationSuggestion{
	"Travel": {
		{Text: "What is the best place you have ever traveled to?", Tone: domain.ToneShared, Reason: "You both like travel"},
		{Text: "If we had a long weekend and one carry-on, where are we going?", Tone: domain.TonePlayful, Reason: "Shared travel interest"},
	},
	"Live Music": {
		{Text: "What is the best concert you have ever been to?", Tone: domain.ToneShared, Reason: "You both like live music"},
	},
	"Concerts": {
		{Text: "Who is the last artist that completely rewired your week?", Tone: domain.ToneOpener, Reason: "Shared concerts interest"},
	},
	"Food": {
		{Text: "You get to pick dinner anywhere tonight — where are we going?", Tone: domain.TonePlayful, Reason: "You both like food"},
	},
	"Cooking": {
		{Text: "What is the dish you make when you actually want to impress someone?", Tone: domain.ToneSincere, Reason: "Shared cooking interest"},
	},
	"Hiking": {
		{Text: "Easy trail with a view, or the one that makes us earn it?", Tone: domain.TonePlayful, Reason: "You both like hiking"},
	},
	"Books": {
		{Text: "What book are you pretending you have finished?", Tone: domain.TonePlayful, Reason: "Shared books interest"},
		{Text: "Which book quietly changed how you date?", Tone: domain.ToneDeeper, Reason: "Shared books interest"},
	},
	"Photography": {
		{Text: "What is a photo you took that still feels like a secret?", Tone: domain.ToneSincere, Reason: "Shared photography interest"},
	},
	"Coffee": {
		{Text: "Neighborhood coffee, no agenda — Saturday or Sunday?", Tone: domain.ToneOpener, Reason: "Shared coffee interest"},
	},
	"Art": {
		{Text: "Last piece of art that made you stay longer than you planned?", Tone: domain.ToneSincere, Reason: "Shared art interest"},
	},
	"Movies": {
		{Text: "What film do you defend a little too hard?", Tone: domain.TonePlayful, Reason: "Shared movies interest"},
	},
	"Fitness": {
		{Text: "Are you a sunrise class person or a “convince me later” person?", Tone: domain.TonePlayful, Reason: "Shared fitness interest"},
	},
}

const maxSuggestions = 6

func fromShared(shared []string, other domain.Profile) []domain.ConversationSuggestion {
	out := []domain.ConversationSuggestion{}
	for _, interest := range shared {
		out = append(out, sharedTemplates[interest]...)
	}
	if len(out) == 0 {
		// No overlap, or overlap we have no template for. A plain human
		// question beats a forced reference to something neither person said.
		out = append(out, domain.ConversationSuggestion{
			Text:   "What should I know about you that never fits on a profile?",
			Tone:   domain.ToneOpener,
			Reason: fmt.Sprintf("A simple, human start with %s", other.FirstName),
		})
	}
	return out
}

func fromPrompts(other domain.Profile) []domain.ConversationSuggestion {
	if len(other.Prompts) == 0 {
		return nil
	}
	prompt := other.Prompts[0]
	return []domain.ConversationSuggestion{
		{
			Text:   fmt.Sprintf("Your answer to “%s” stayed with me. Tell me more?", prompt.Question),
			Tone:   domain.ToneSincere,
			Reason: "Responding to a prompt",
		},
		{
			Text:   fmt.Sprintf("I keep thinking about “%s”. Was that a recent Sunday?", prompt.Answer),
			Tone:   domain.ToneOpener,
			Reason: "Prompt-inspired opener",
		},
	}
}

// SuggestionsFor proposes openers for one pair. An empty tone means "any";
// filtering to a tone with no matches falls back to the first few rather than
// returning nothing, because an empty list reads as a broken screen.
func SuggestionsFor(current, other domain.Profile, tone domain.SuggestionTone) []domain.ConversationSuggestion {
	right := make(map[string]bool, len(other.Interests))
	for _, interest := range other.Interests {
		right[interest] = true
	}
	shared := []string{}
	for _, interest := range current.Interests {
		if right[interest] {
			shared = append(shared, interest)
		}
	}

	items := fromShared(shared, other)
	items = append(items, fromPrompts(other)...)
	// Both of these read badly when the field is blank ("being a ?"), so an
	// unfinished profile contributes fewer openers rather than broken ones.
	if other.Occupation != "" {
		items = append(items, domain.ConversationSuggestion{
			Text: fmt.Sprintf("What part of being a %s still surprises you?",
				strings.ToLower(other.Occupation)),
			Tone:   domain.ToneDeeper,
			Reason: "Occupation",
		})
	}
	if label, ok := intentShortLabel[other.RelationshipIntent]; ok {
		items = append(items, domain.ConversationSuggestion{
			Text: fmt.Sprintf(
				"You wrote that you are looking for %s. What does that look like in a regular week?",
				strings.ToLower(label)),
			Tone:   domain.ToneDeeper,
			Reason: "Dating intent",
		})
	}

	if tone == "" {
		return capped(items, maxSuggestions)
	}
	filtered := []domain.ConversationSuggestion{}
	for _, item := range items {
		if item.Tone == tone {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		return capped(items, 3)
	}
	return filtered
}

func capped(items []domain.ConversationSuggestion, limit int) []domain.ConversationSuggestion {
	if len(items) > limit {
		return items[:limit]
	}
	return items
}
