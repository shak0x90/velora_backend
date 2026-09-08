package compat_test

import (
	"strings"
	"testing"

	"github.com/shak0x90/velora_backend/internal/compat"
	"github.com/shak0x90/velora_backend/internal/domain"
)

func person(name string, interests []string) domain.Profile {
	return domain.Profile{
		FirstName:          name,
		Interests:          interests,
		Occupation:         "Ceramicist",
		RelationshipIntent: domain.IntentLongTerm,
	}
}

func TestSuggestionsUseSharedInterests(t *testing.T) {
	me := person("Sam", []string{"Books", "Hiking", "Chess"})
	them := person("Rae", []string{"Books", "Coffee"})

	got := compat.SuggestionsFor(me, them, "")
	if len(got) == 0 {
		t.Fatal("expected suggestions")
	}
	// Books is shared; Coffee is theirs alone and must not be drawn on.
	joined := renderAll(got)
	if !strings.Contains(joined, "book") {
		t.Errorf("shared interest was not used: %v", joined)
	}
	if strings.Contains(joined, "neighborhood coffee") {
		t.Errorf("an interest only they hold was treated as shared: %v", joined)
	}
}

func TestSuggestionsFallBackWhenNothingIsShared(t *testing.T) {
	me := person("Sam", []string{"Chess"})
	them := person("Rae", []string{"Bouldering"})

	got := compat.SuggestionsFor(me, them, "")
	if len(got) == 0 {
		t.Fatal("expected a fallback opener rather than nothing")
	}
	if !strings.Contains(got[0].Reason, "Rae") {
		t.Errorf("the fallback should name the other person, got %q", got[0].Reason)
	}
}

// An empty profile is what produces broken copy: "being a ?" reads as a bug to
// whoever is about to send it.
func TestSuggestionsSkipBlankFields(t *testing.T) {
	me := person("Sam", nil)
	them := domain.Profile{FirstName: "Rae"}

	for _, item := range compat.SuggestionsFor(me, them, "") {
		if strings.Contains(item.Text, "being a  ") || strings.Contains(item.Text, "looking for .") {
			t.Errorf("a blank field leaked into copy: %q", item.Text)
		}
	}
}

func TestSuggestionsFilterByTone(t *testing.T) {
	me := person("Sam", []string{"Books"})
	them := person("Rae", []string{"Books"})

	for _, item := range compat.SuggestionsFor(me, them, domain.ToneDeeper) {
		if item.Tone != domain.ToneDeeper {
			t.Errorf("asked for deeper, got %q", item.Tone)
		}
	}

	// A tone nothing matches must still return something: an empty list reads
	// as a broken screen rather than as "none of that flavour".
	if got := compat.SuggestionsFor(me, them, domain.SuggestionTone("wry")); len(got) == 0 {
		t.Error("an unmatched tone should fall back, not return nothing")
	}
}

func TestSuggestionsAreCapped(t *testing.T) {
	shared := []string{"Books", "Travel", "Food", "Art", "Movies", "Coffee", "Hiking"}
	me := person("Sam", shared)
	them := person("Rae", shared)

	if got := compat.SuggestionsFor(me, them, ""); len(got) > 6 {
		t.Errorf("expected at most 6 suggestions, got %d", len(got))
	}
}

func renderAll(items []domain.ConversationSuggestion) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, strings.ToLower(item.Text))
	}
	return strings.Join(parts, " | ")
}
