package social

import (
	"errors"
	"net/http"
	"strings"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

func (s *Service) Routes(mux *http.ServeMux) {
	mux.Handle("POST /passes/{id}", s.guard(http.HandlerFunc(s.handlePass)))

	mux.Handle("POST /likes", s.guard(http.HandlerFunc(s.handleLike)))
	mux.Handle("GET /likes/incoming", s.guard(http.HandlerFunc(s.handleIncoming)))
	mux.Handle("GET /likes/outgoing", s.guard(http.HandlerFunc(s.handleOutgoing)))
	mux.Handle("DELETE /likes/{id}", s.guard(http.HandlerFunc(s.handleRemoveLike)))

	mux.Handle("GET /matches", s.guard(http.HandlerFunc(s.handleMatches)))

	mux.Handle("GET /notifications", s.guard(http.HandlerFunc(s.handleNotifications)))
	mux.Handle("POST /notifications/{id}/read", s.guard(http.HandlerFunc(s.handleMarkRead)))
}

func (s *Service) handlePass(w http.ResponseWriter, r *http.Request) {
	if err := s.Pass(r.Context(), auth.UserIDFrom(r.Context()), r.PathValue("id")); err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

func (s *Service) handleLike(w http.ResponseWriter, r *http.Request) {
	var input LikeInput
	if err := httpx.Decode(r, &input); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if strings.TrimSpace(input.ProfileID) == "" {
		httpx.Error(w, r, httpx.BadRequest("profileId is required."))
		return
	}

	result, err := s.Like(r.Context(), auth.UserIDFrom(r.Context()), input)
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusOK, result)
}

func (s *Service) handleIncoming(w http.ResponseWriter, r *http.Request) {
	s.writeLikes(w, r, true)
}

func (s *Service) handleOutgoing(w http.ResponseWriter, r *http.Request) {
	s.writeLikes(w, r, false)
}

func (s *Service) writeLikes(w http.ResponseWriter, r *http.Request, incoming bool) {
	likes, err := s.Likes(r.Context(), auth.UserIDFrom(r.Context()), incoming)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, likes)
}

func (s *Service) handleRemoveLike(w http.ResponseWriter, r *http.Request) {
	if err := s.RemoveLike(r.Context(), auth.UserIDFrom(r.Context()), r.PathValue("id")); err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

func (s *Service) handleMatches(w http.ResponseWriter, r *http.Request) {
	matches, err := s.Matches(r.Context(), auth.UserIDFrom(r.Context()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, matches)
}

func (s *Service) handleNotifications(w http.ResponseWriter, r *http.Request) {
	notifications, err := s.Notifications(r.Context(), auth.UserIDFrom(r.Context()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, notifications)
}

func (s *Service) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	if err := s.MarkRead(r.Context(), auth.UserIDFrom(r.Context()), r.PathValue("id")); err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

// translate maps this package's errors onto problem documents. Anything
// unrecognised falls through to a generic 500 rather than leaking internals.
func translate(err error) error {
	switch {
	case errors.Is(err, ErrSelfDirected):
		return httpx.BadRequest("That is your own profile.")
	case errors.Is(err, ErrNoSuchPerson):
		return httpx.NotFound("That profile is no longer available.")
	case errors.Is(err, ErrNoSuchLike):
		return httpx.NotFound("That like is not yours, or is already gone.")
	case err != nil && strings.Contains(err.Error(), "is not a kind of like"):
		return httpx.BadRequest("That is not a kind of like we recognise.")
	default:
		return err
	}
}
