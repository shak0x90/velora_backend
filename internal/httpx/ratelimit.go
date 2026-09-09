package httpx

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rate limiting, per client address.
//
// The endpoint this exists for is registration. It is public by necessity —
// nobody could sign up otherwise — and every call writes a row. Left
// unlimited, one script fills the database, and on a small box that takes the
// whole service down along with everything else sharing it.
//
// A fixed window rather than a token bucket: the thing being guarded against
// is thousands of requests a minute, and any of the classic algorithms stops
// that. The window's edge lets a determined caller send double the limit
// across a boundary, which is a rounding error against what it prevents.
//
// In-memory, so counts are per process and reset on deploy. That is the right
// trade until there is more than one instance; a shared counter needs Redis,
// and adding a dependency to a service that does not otherwise need one is a
// worse outcome than a limit that resets when the binary restarts.

// Limiter counts requests per key inside a fixed window.
type Limiter struct {
	mu     sync.Mutex
	counts map[string]*counter
	limit  int
	window time.Duration

	// maxKeys bounds the map. Without it, a caller cycling through addresses
	// turns this defence into the memory exhaustion it was meant to prevent.
	maxKeys int
	swept   time.Time
}

type counter struct {
	hits    int
	resetAt time.Time
}

// NewLimiter allows `limit` requests per key per `window`.
func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{
		counts:  make(map[string]*counter),
		limit:   limit,
		window:  window,
		maxKeys: 50_000,
		swept:   time.Now(),
	}
}

// Allow records a hit and reports whether it is within the limit, along with
// how long until the window resets.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	entry, seen := l.counts[key]
	if !seen || now.After(entry.resetAt) {
		l.counts[key] = &counter{hits: 1, resetAt: now.Add(l.window)}
		return true, 0
	}

	entry.hits++
	if entry.hits > l.limit {
		return false, time.Until(entry.resetAt)
	}
	return true, 0
}

// sweep drops expired entries. Called under the lock, at most once a minute,
// and unconditionally once the map passes its ceiling.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < time.Minute && len(l.counts) < l.maxKeys {
		return
	}
	l.swept = now
	for key, entry := range l.counts {
		if now.After(entry.resetAt) {
			delete(l.counts, key)
		}
	}
	// Still over the ceiling with nothing expired: every entry is live, which
	// means an attack rather than traffic. Start again and let honest callers
	// re-establish their counts, rather than growing without bound.
	if len(l.counts) >= l.maxKeys {
		l.counts = make(map[string]*counter)
	}
}

// Middleware applies the limit to everything it wraps.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, retryAfter := l.Allow(ClientIP(r))
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
			Error(w, r, TooManyRequests(fmt.Sprintf(
				"Too many requests. Try again in %s.", humanise(retryAfter))))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// LimitPaths applies a limit only to requests whose path carries one of the
// given prefixes, so one middleware can guard the expensive routes and leave
// the rest alone.
func LimitPaths(l *Limiter, prefixes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		limited := l.Middleware(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, prefix := range prefixes {
				if strings.HasPrefix(r.URL.Path, prefix) {
					limited.ServeHTTP(w, r)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP is who to count against.
//
// X-Real-IP is set by nginx and trusted because the API port is firewalled —
// nothing reaches this process without passing through the proxy. Were that
// ever untrue the header becomes attacker-controlled, and this would have to
// go back to RemoteAddr alone.
func ClientIP(r *http.Request) string {
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	// The left-most X-Forwarded-For entry is the original client, when a proxy
	// we trust wrote it.
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		if trimmed := strings.TrimSpace(first); trimmed != "" {
			return trimmed
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func humanise(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds())+1)
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes())+1)
	default:
		return fmt.Sprintf("%d hours", int(d.Hours())+1)
	}
}
