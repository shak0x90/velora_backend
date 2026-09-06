package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/shak0x90/velora_backend/internal/httpx"
)

type ctxKey string

const userIDKey ctxKey = "velora_user_id"

// UserIDFrom returns the authenticated user id, or "" if the request did not
// pass through RequireAuth.
func UserIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(userIDKey).(string)
	return id
}

// Routes registers the auth endpoints on the given mux.
func (s *Service) Routes(mux *http.ServeMux) {
	// Email + password. Here so accounts can be created before the Google
	// OAuth clients exist, since those need a real domain.
	mux.HandleFunc("POST /auth/register", s.handleRegister)
	mux.HandleFunc("POST /auth/login", s.handleLogin)

	mux.HandleFunc("POST /auth/social", s.handleSocial)
	mux.HandleFunc("POST /auth/refresh", s.handleRefresh)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.Handle("GET /me", s.RequireAuth(http.HandlerFunc(s.handleMe)))
}

type socialRequest struct {
	Provider string `json:"provider"`
	IDToken  string `json:"idToken"`
}

// handleSocial is both sign-up and sign-in. The client sends the ID token it
// got from Google; we never see a password and never handle an OAuth secret,
// because this verifies an already-issued token rather than exchanging a code.
func (s *Service) handleSocial(w http.ResponseWriter, r *http.Request) {
	var req socialRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.Provider != "google" {
		httpx.Error(w, r, httpx.BadRequest("Only 'google' is supported right now."))
		return
	}
	if strings.TrimSpace(req.IDToken) == "" {
		httpx.Error(w, r, httpx.BadRequest("idToken is required."))
		return
	}

	session, err := s.SignInWithGoogle(r.Context(), req.IDToken)
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusOK, session)
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

func (s *Service) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.RefreshToken == "" {
		httpx.Error(w, r, httpx.BadRequest("refreshToken is required."))
		return
	}

	session, err := s.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	httpx.JSON(w, http.StatusOK, session)
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	// A logout with no body is still a successful logout — never make signing
	// out fail.
	_ = httpx.Decode(r, &req)
	if req.RefreshToken != "" {
		if err := s.SignOut(r.Context(), req.RefreshToken); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	user, err := s.CurrentUser(r.Context(), UserIDFrom(r.Context()))
	if err != nil {
		httpx.Error(w, r, translate(err))
		return
	}
	// Profile is null until onboarding writes one — the client uses that to
	// decide between the feed and the onboarding flow.
	httpx.JSON(w, http.StatusOK, map[string]any{
		"user":    user,
		"profile": nil,
	})
}

// RequireAuth rejects anything without a valid bearer token.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			httpx.Error(w, r, httpx.Unauthorized("Sign in to continue."))
			return
		}
		userID, err := s.ParseAccessToken(strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			httpx.Error(w, r, httpx.Unauthorized("Your session expired. Sign in again."))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userIDKey, userID)))
	})
}

// translate maps domain errors onto problem documents. Anything unrecognised
// falls through to a generic 500 rather than leaking internals.
func translate(err error) error {
	switch {
	case errors.Is(err, ErrInvalidToken):
		return httpx.Unauthorized("That sign-in could not be verified. Try again.")
	case errors.Is(err, ErrTokenReuse):
		return httpx.Unauthorized("Your session was ended for security. Sign in again.")
	case errors.Is(err, ErrEmailNotVerified):
		return httpx.BadRequest("Your Google account needs a verified email address.")
	case errors.Is(err, ErrUserSuspended):
		return httpx.Forbidden("This account is not available.")
	default:
		return err
	}
}
