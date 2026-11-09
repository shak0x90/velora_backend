package settings

import (
	"errors"
	"net/http"
	"strings"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

// Routes registers GET and PATCH /me/settings.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.Handle("GET /me/settings", s.guard(http.HandlerFunc(s.handleGet)))
	mux.Handle("PATCH /me/settings", s.guard(http.HandlerFunc(s.handlePatch)))
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	current, err := s.store.Get(r.Context(), auth.UserIDFrom(r.Context()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, current)
}

func (s *Service) handlePatch(w http.ResponseWriter, r *http.Request) {
	var patch Patch
	if err := httpx.Decode(r, &patch); err != nil {
		httpx.Error(w, r, err)
		return
	}
	saved, err := s.store.Update(r.Context(), auth.UserIDFrom(r.Context()), func(current Settings) (Settings, error) {
		return current.Apply(patch)
	})
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			httpx.Error(w, r, httpx.BadRequest(strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")))
			return
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, saved)
}
