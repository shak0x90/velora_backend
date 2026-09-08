package profile

import (
	"errors"
	"net/http"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/compat"
	"github.com/shak0x90/velora_backend/internal/domain"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

// Routes registers the /me surface.
//
// GET /me lives here rather than in auth because the response is now mostly
// profile: auth contributes one object to it, and this package owns the four
// tables the rest of it comes from.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.Handle("GET /me", s.guard(http.HandlerFunc(s.handleMe)))
	mux.Handle("PATCH /me", s.guard(http.HandlerFunc(s.handleUpdate)))
	mux.Handle("POST /me/onboarding", s.guard(http.HandlerFunc(s.handleOnboard)))
}

// meResponse is what a client boots from. Profile and completion are null
// until onboarding writes a row — that null is what tells the client to show
// the wizard instead of the feed.
type meResponse struct {
	User       auth.AuthUser            `json:"user"`
	Profile    *domain.Profile          `json:"profile"`
	Completion *compat.CompletionAdvice `json:"completion"`
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFrom(r.Context())

	user, err := s.users.CurrentUser(r.Context(), userID)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidToken) {
			httpx.Error(w, r, httpx.Unauthorized("Your session expired. Sign in again."))
			return
		}
		httpx.Error(w, r, err)
		return
	}

	body := meResponse{User: user}
	profile, err := s.Load(r.Context(), userID)
	switch {
	case errors.Is(err, ErrNoProfile):
		// Not an error: a signed-in account partway through onboarding.
	case err != nil:
		httpx.Error(w, r, err)
		return
	default:
		advice := compat.EvaluateCompletion(profile)
		body.Profile = &profile
		body.Completion = &advice
		s.Touch(r.Context(), userID)
	}

	httpx.JSON(w, http.StatusOK, body)
}

// handleUpdate applies a partial change and returns the whole profile, so the
// client replaces its copy rather than merging and drifting away from the
// server's normalisation — trimmed strings, deduped tags, minted prompt ids.
func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFrom(r.Context())

	var patch Patch
	if err := httpx.Decode(r, &patch); err != nil {
		httpx.Error(w, r, err)
		return
	}

	profile, err := s.Update(r.Context(), userID, patch)
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusOK, profile)
}

// handleOnboard is the onboarding write: the first profile row for this user.
// It takes the same patch shape as PATCH /me, so a wizard can send everything
// it collected in one call, but firstName, dateOfBirth and gender are required
// because the table cannot hold a row without them.
func (s *Service) handleOnboard(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFrom(r.Context())

	var patch Patch
	if err := httpx.Decode(r, &patch); err != nil {
		httpx.Error(w, r, err)
		return
	}

	profile, err := s.Create(r.Context(), userID, patch)
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}

	user, err := s.users.CurrentUser(r.Context(), userID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	advice := compat.EvaluateCompletion(profile)
	httpx.JSON(w, http.StatusCreated, meResponse{
		User: user, Profile: &profile, Completion: &advice,
	})
}

// translate maps this package's errors onto problem documents. Anything
// unrecognised falls through to a generic 500 rather than leaking internals.
func translate(err error) error {
	var invalid ValidationError
	switch {
	case errors.As(err, &invalid):
		return httpx.BadRequest(invalid.Detail)
	case errors.Is(err, ErrNoProfile):
		return httpx.Problem{
			Type:   "about:blank#no-profile",
			Title:  "Profile not set up",
			Status: http.StatusConflict,
			Detail: "Finish setting up your profile before editing it.",
		}
	default:
		return err
	}
}
