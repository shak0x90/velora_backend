package safety

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shak0x90/velora_backend/internal/httpx"
)

// The errors Block and Report return for ordinary mistakes must reach the
// client as 4xx problems, whichever handler made the call. Chat's in-thread
// block and report go through this mapper too, so a regression here shows up
// as a generic 500 in the conversation screen.
func TestTranslateHTTPMapsExpectedErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"self directed", ErrSelfDirected, http.StatusBadRequest},
		{"unknown profile", ErrNoSuchPerson, http.StatusNotFound},
		{"wrapped unknown profile", fmt.Errorf("block: %w", ErrNoSuchPerson), http.StatusNotFound},
		{"no such match", ErrNoSuchMatch, http.StatusNotFound},
		{"no such report", ErrNoSuchReport, http.StatusNotFound},
		{"daily limit", ErrTooMany, http.StatusTooManyRequests},
		{"bad reason", fmt.Errorf("%q is not a reason we recognise", "x"), http.StatusBadRequest},
		{"long detail", fmt.Errorf("keep the detail under %d characters", 2000), http.StatusBadRequest},
		{"unexpected", errors.New("connection reset"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			httpx.Error(rec, httptest.NewRequest(http.MethodPost, "/", nil), TranslateHTTP(tc.err))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestTranslateHTTPCapitalisesValidationMessages(t *testing.T) {
	got := TranslateHTTP(errors.New("keep the detail under 10 characters"))
	var problem httpx.Problem
	if !errors.As(got, &problem) {
		t.Fatalf("got %T, want httpx.Problem", got)
	}
	if problem.Detail != "Keep the detail under 10 characters." {
		t.Fatalf("detail = %q", problem.Detail)
	}
}
