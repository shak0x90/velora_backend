package account

import (
	"errors"
	"net/http"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

// Routes registers DELETE /me. GET and PATCH /me belong to the profile
// package; deletion lives here because it reaches far past the profile.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.Handle("DELETE /me", s.guard(http.HandlerFunc(s.handleDelete)))
}

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Delete(r.Context(), auth.UserIDFrom(r.Context())); err != nil {
		if errors.Is(err, ErrNotActive) {
			httpx.Error(w, r, httpx.Forbidden("This account is no longer available."))
			return
		}
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}
