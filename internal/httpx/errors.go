// Package httpx holds the HTTP plumbing every handler shares: error shape,
// JSON helpers, and middleware. Domain packages must not import it.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Problem is an RFC 9457 problem-details document. Every error the API returns
// uses this shape, so the clients need exactly one error path. The web client's
// ApiClient already reads `detail`.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Instance carries the request ID so a user-visible error can be traced
	// back to a specific log line.
	Instance string `json:"instance,omitempty"`
}

func (p Problem) Error() string { return p.Title + ": " + p.Detail }

// Common problems. Construct with these rather than assembling ad hoc shapes,
// so `type` stays a stable, documentable URI per failure mode.
func BadRequest(detail string) Problem {
	return Problem{Type: "about:blank#bad-request", Title: "Bad request", Status: http.StatusBadRequest, Detail: detail}
}

func Unauthorized(detail string) Problem {
	return Problem{Type: "about:blank#unauthorized", Title: "Not signed in", Status: http.StatusUnauthorized, Detail: detail}
}

func Forbidden(detail string) Problem {
	return Problem{Type: "about:blank#forbidden", Title: "Not allowed", Status: http.StatusForbidden, Detail: detail}
}

func NotFound(detail string) Problem {
	return Problem{Type: "about:blank#not-found", Title: "Not found", Status: http.StatusNotFound, Detail: detail}
}

func TooManyRequests(detail string) Problem {
	return Problem{Type: "about:blank#rate-limited", Title: "Slow down", Status: http.StatusTooManyRequests, Detail: detail}
}

func Internal() Problem {
	return Problem{
		Type:   "about:blank#internal",
		Title:  "Something went wrong",
		Status: http.StatusInternalServerError,
		Detail: "The request could not be completed. Try again.",
	}
}

// JSON writes any value as a JSON response.
func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Headers are already sent, so there is nothing to do but record it.
		slog.Error("encoding response failed", "error", err)
	}
}

// Error writes a Problem. A non-Problem error is deliberately not exposed:
// it is logged in full and the caller sees a generic 500, so internals and
// database messages never leak to a client.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	problem, ok := err.(Problem)
	if !ok {
		slog.ErrorContext(r.Context(), "unhandled error", "error", err, "path", r.URL.Path)
		problem = Internal()
	}
	problem.Instance = RequestIDFrom(r.Context())

	if problem.Status >= 500 {
		slog.ErrorContext(r.Context(), problem.Title, "detail", problem.Detail, "path", r.URL.Path)
	}

	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(problem.Status)
	if encodeErr := json.NewEncoder(w).Encode(problem); encodeErr != nil {
		slog.Error("encoding problem failed", "error", encodeErr)
	}
}

// Decode reads a JSON body, rejecting unknown fields so a client typo is a
// loud 400 rather than a silently ignored value.
func Decode(r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return BadRequest("Could not read the request body: " + err.Error())
	}
	return nil
}
