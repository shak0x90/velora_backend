package safety

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

func (s *Service) Routes(mux *http.ServeMux) {
	mux.Handle("GET /blocks", s.guard(http.HandlerFunc(s.handleBlocked)))
	mux.Handle("POST /blocks/{id}", s.guard(http.HandlerFunc(s.handleBlock)))
	mux.Handle("DELETE /blocks/{id}", s.guard(http.HandlerFunc(s.handleUnblock)))

	mux.Handle("POST /reports", s.guard(http.HandlerFunc(s.handleReport)))

	// Unmatching lives here rather than with matches: it is the same gesture as
	// blocking, one notch softer, and people reach for them in the same moment.
	mux.Handle("DELETE /matches/{id}", s.guard(http.HandlerFunc(s.handleUnmatch)))

	mux.Handle("GET /admin/reports", s.RequireModerator(http.HandlerFunc(s.handleQueue)))
	mux.Handle("POST /admin/reports/{id}", s.RequireModerator(http.HandlerFunc(s.handleResolve)))
}

type blockRequest struct {
	Reason string `json:"reason"`
}

func (s *Service) handleBlock(w http.ResponseWriter, r *http.Request) {
	var req blockRequest
	// A block with no body is still a block. Never fail this on a detail.
	_ = httpx.Decode(r, &req)

	if err := s.Block(r.Context(), auth.UserIDFrom(r.Context()),
		r.PathValue("id"), req.Reason); err != nil {
		httpx.Error(w, r, translateHTTP(err))
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

func (s *Service) handleUnblock(w http.ResponseWriter, r *http.Request) {
	if err := s.Unblock(r.Context(), auth.UserIDFrom(r.Context()),
		r.PathValue("id")); err != nil {
		httpx.Error(w, r, translateHTTP(err))
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

func (s *Service) handleBlocked(w http.ResponseWriter, r *http.Request) {
	blocked, err := s.Blocked(r.Context(), auth.UserIDFrom(r.Context()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, blocked)
}

func (s *Service) handleReport(w http.ResponseWriter, r *http.Request) {
	var input ReportInput
	if err := httpx.Decode(r, &input); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if strings.TrimSpace(input.ProfileID) == "" {
		httpx.Error(w, r, httpx.BadRequest("profileId is required."))
		return
	}

	if err := s.Report(r.Context(), auth.UserIDFrom(r.Context()), input); err != nil {
		httpx.Error(w, r, translateHTTP(err))
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

func (s *Service) handleUnmatch(w http.ResponseWriter, r *http.Request) {
	if err := s.Unmatch(r.Context(), auth.UserIDFrom(r.Context()),
		r.PathValue("id")); err != nil {
		httpx.Error(w, r, translateHTTP(err))
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

func (s *Service) handleQueue(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	reports, err := s.Queue(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		httpx.Error(w, r, translateHTTP(err))
		return
	}
	httpx.JSON(w, http.StatusOK, reports)
}

func (s *Service) handleResolve(w http.ResponseWriter, r *http.Request) {
	var decision Resolution
	if err := httpx.Decode(r, &decision); err != nil {
		httpx.Error(w, r, err)
		return
	}

	if err := s.Resolve(r.Context(), auth.UserIDFrom(r.Context()),
		r.PathValue("id"), decision); err != nil {
		httpx.Error(w, r, translateHTTP(err))
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

// translateHTTP maps this package's errors onto problem documents. Anything
// unrecognised falls through to a generic 500 rather than leaking internals.
func translateHTTP(err error) error {
	switch {
	case errors.Is(err, ErrSelfDirected):
		return httpx.BadRequest("That is your own account.")
	case errors.Is(err, ErrNoSuchPerson):
		return httpx.NotFound("That profile is no longer available.")
	case errors.Is(err, ErrNoSuchMatch):
		return httpx.NotFound("That match is not yours, or is already gone.")
	case errors.Is(err, ErrNoSuchReport):
		return httpx.NotFound("That report no longer exists.")
	case errors.Is(err, ErrTooMany):
		return httpx.TooManyRequests(
			"That is a lot of reports for one day. If something urgent is happening, contact support.")
	// The validation failures are plain errors from one small set of messages,
	// all safe to show and all the caller's to fix.
	case err != nil && strings.Contains(err.Error(), "we recognise"),
		err != nil && strings.Contains(err.Error(), "keep the detail under"):
		return httpx.BadRequest(capitalise(err.Error()) + ".")
	default:
		return err
	}
}

func capitalise(message string) string {
	if message == "" {
		return message
	}
	return strings.ToUpper(message[:1]) + message[1:]
}
