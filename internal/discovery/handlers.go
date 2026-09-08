package discovery

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/domain"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

func (s *Service) Routes(mux *http.ServeMux) {
	mux.Handle("GET /discover", s.guard(http.HandlerFunc(s.handleFeed)))
	mux.Handle("GET /discover/daily-picks", s.guard(http.HandlerFunc(s.handleDailyPicks)))
	mux.Handle("GET /search", s.guard(http.HandlerFunc(s.handleSearch)))

	// The collection form resolves ids the likes and matches screens already
	// hold; the item form is a profile someone navigated to.
	mux.Handle("GET /profiles", s.guard(http.HandlerFunc(s.handlePeople)))
	mux.Handle("GET /profiles/{id}", s.guard(http.HandlerFunc(s.handleDetail)))
	mux.Handle("GET /profiles/{id}/suggestions", s.guard(http.HandlerFunc(s.handleSuggestions)))
}

// maxIDsPerRequest bounds a batch lookup: well past any screen's needs, and
// low enough that the URL and the query stay sane.
const maxIDsPerRequest = 100

func (s *Service) handleFeed(w http.ResponseWriter, r *http.Request) {
	filters, err := parseFilters(r.URL.Query().Get("filters"))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	ranked, err := s.Feed(r.Context(), auth.UserIDFrom(r.Context()), filters)
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusOK, ranked)
}

func (s *Service) handleDailyPicks(w http.ResponseWriter, r *http.Request) {
	picks, err := s.DailyPicks(r.Context(), auth.UserIDFrom(r.Context()))
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusOK, picks)
}

func (s *Service) handleSearch(w http.ResponseWriter, r *http.Request) {
	results, err := s.Search(r.Context(), auth.UserIDFrom(r.Context()), r.URL.Query().Get("q"))
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusOK, results)
}

func (s *Service) handleDetail(w http.ResponseWriter, r *http.Request) {
	detail, err := s.Detail(r.Context(), auth.UserIDFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusOK, detail)
}

func (s *Service) handlePeople(w http.ResponseWriter, r *http.Request) {
	ids := splitIDs(r.URL.Query().Get("ids"))
	if len(ids) == 0 {
		httpx.JSON(w, http.StatusOK, []domain.Profile{})
		return
	}
	if len(ids) > maxIDsPerRequest {
		httpx.Error(w, r, httpx.BadRequest("Ask for at most 100 profiles at a time."))
		return
	}

	people, err := s.People(r.Context(), auth.UserIDFrom(r.Context()), ids)
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusOK, people)
}

func (s *Service) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	tone := domain.SuggestionTone(r.URL.Query().Get("tone"))
	suggestions, err := s.Suggestions(
		r.Context(), auth.UserIDFrom(r.Context()), r.PathValue("id"), tone)
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusOK, suggestions)
}

// parseFilters reads the JSON blob the clients put in the query string.
//
// An absent parameter means the defaults, not an empty filter set: a zero
// DiscoverFilters has MaxAge 0 and would match nobody. Starting from the
// defaults also means a client sending only some fields still gets sane
// bounds for the rest.
func parseFilters(raw string) (domain.DiscoverFilters, error) {
	filters := domain.DefaultFilters()
	if strings.TrimSpace(raw) == "" {
		return filters, nil
	}
	if err := json.Unmarshal([]byte(raw), &filters); err != nil {
		return filters, httpx.BadRequest("Could not read the filters: " + err.Error())
	}
	return filters, nil
}

func splitIDs(raw string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// translate maps the errors this package surfaces onto problem documents.
func translate(err error) error {
	switch {
	case errors.Is(err, ErrNoProfile):
		return httpx.Problem{
			Type:   "about:blank#no-profile",
			Title:  "Profile not set up",
			Status: http.StatusConflict,
			Detail: "Finish setting up your profile to see people.",
		}
	// A malformed uuid in a path is a stale link, not a server fault.
	case err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid"):
		return httpx.NotFound("That profile is no longer available.")
	default:
		return err
	}
}
