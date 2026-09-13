package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The limiter had no tests at all, and the integration suite cannot supply
// them: apitest runs on the server, and isLocal deliberately exempts loopback
// callers, so every check it makes goes straight past this code. All of it is
// pure, which is exactly why leaving it unproven was the wrong trade.

func TestAllowsUpToTheLimitThenRefuses(t *testing.T) {
	l := NewLimiter(3, time.Minute)

	for i := 1; i <= 3; i++ {
		if ok, _ := l.Allow("client"); !ok {
			t.Fatalf("request %d of 3 was refused inside the limit", i)
		}
	}

	ok, retryAfter := l.Allow("client")
	if ok {
		t.Fatal("the fourth request was allowed past a limit of three")
	}
	if retryAfter <= 0 || retryAfter > time.Minute {
		t.Errorf("retry-after should fall inside the window, got %s", retryAfter)
	}
}

func TestBudgetsAreKeyedSeparately(t *testing.T) {
	l := NewLimiter(1, time.Minute)

	if ok, _ := l.Allow("first"); !ok {
		t.Fatal("the first caller was refused immediately")
	}
	if ok, _ := l.Allow("second"); !ok {
		t.Fatal("one caller's traffic consumed another's budget")
	}
	if ok, _ := l.Allow("first"); ok {
		t.Error("the first caller was allowed a second request")
	}
}

func TestTheWindowReopens(t *testing.T) {
	l := NewLimiter(1, 40*time.Millisecond)

	if ok, _ := l.Allow("client"); !ok {
		t.Fatal("refused on the first request")
	}
	if ok, _ := l.Allow("client"); ok {
		t.Fatal("allowed a second request inside the window")
	}

	time.Sleep(60 * time.Millisecond)
	if ok, _ := l.Allow("client"); !ok {
		t.Error("still refused after the window had passed")
	}
}

// A limiter that grows a row per address is the memory exhaustion it exists to
// prevent, so the map has a ceiling and a sweep.
func TestSweepDropsExpiredEntries(t *testing.T) {
	l := NewLimiter(5, 20*time.Millisecond)
	l.Allow("stale-one")
	l.Allow("stale-two")

	time.Sleep(40 * time.Millisecond)

	// sweep only runs once a minute unless the map is over its ceiling, so
	// reach past that here rather than waiting.
	l.mu.Lock()
	l.swept = time.Now().Add(-2 * time.Minute)
	l.sweep(time.Now())
	remaining := len(l.counts)
	l.mu.Unlock()

	if remaining != 0 {
		t.Errorf("expired entries survived the sweep: %d left", remaining)
	}
}

func TestSweepResetsWhenEveryEntryIsLive(t *testing.T) {
	l := NewLimiter(5, time.Hour)
	l.maxKeys = 3

	// A long window means nothing expires, so the ceiling is the only thing
	// between an address-cycling caller and unbounded growth.
	for _, key := range []string{"a", "b", "c", "d"} {
		l.Allow(key)
	}

	l.mu.Lock()
	size := len(l.counts)
	l.mu.Unlock()

	if size >= 4 {
		t.Errorf("the map grew past its ceiling: %d entries", size)
	}
}

func TestClientIPPrefersTheProxyHeaders(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		realIP     string
		forwarded  string
		want       string
	}{
		{"real ip wins", "10.0.0.1:5555", "203.0.113.7", "198.51.100.9", "203.0.113.7"},
		{"forwarded when no real ip", "10.0.0.1:5555", "", "198.51.100.9, 10.0.0.1", "198.51.100.9"},
		{"remote addr as the floor", "203.0.113.7:5555", "", "", "203.0.113.7"},
		{"blank headers are ignored", "203.0.113.7:5555", "   ", "", "203.0.113.7"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = c.remoteAddr
			if c.realIP != "" {
				r.Header.Set("X-Real-IP", c.realIP)
			}
			if c.forwarded != "" {
				r.Header.Set("X-Forwarded-For", c.forwarded)
			}
			if got := ClientIP(r); got != c.want {
				t.Errorf("ClientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// The exemption is what keeps the test suite usable on the box, so its shape
// matters: a request carrying proxy headers is never local, however it arrived.
func TestIsLocalOnlyForUnproxiedLoopback(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		header     string
		value      string
		want       bool
	}{
		{"loopback with no headers", "127.0.0.1:5555", "", "", true},
		{"ipv6 loopback", "[::1]:5555", "", "", true},
		{"loopback but proxied", "127.0.0.1:5555", "X-Real-IP", "203.0.113.7", false},
		{"loopback but forwarded", "127.0.0.1:5555", "X-Forwarded-For", "203.0.113.7", false},
		{"public address", "203.0.113.7:5555", "", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = c.remoteAddr
			if c.header != "" {
				r.Header.Set(c.header, c.value)
			}
			if got := isLocal(r); got != c.want {
				t.Errorf("isLocal = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMiddlewareRefusesWithRetryAfter(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	served := 0
	handler := l.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served++
	}))

	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/auth/register", nil)
		r.RemoteAddr = "203.0.113.7:5555"
		r.Header.Set("X-Real-IP", "203.0.113.7")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	if got := call().Code; got != http.StatusOK {
		t.Fatalf("first request answered %d, want 200", got)
	}

	second := call()
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request answered %d, want 429", second.Code)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Error("a 429 without Retry-After leaves the caller guessing")
	}
	if served != 1 {
		t.Errorf("the refused request still reached the handler (served %d)", served)
	}
}

func TestMiddlewareExemptsLocalCallers(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	served := 0
	handler := l.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served++
	}))

	for i := 0; i < 5; i++ {
		r := httptest.NewRequest(http.MethodPost, "/auth/register", nil)
		r.RemoteAddr = "127.0.0.1:5555"
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}

	if served != 5 {
		t.Errorf("loopback calls were limited: %d of 5 served", served)
	}
}

func TestLimitPathsOnlyGuardsItsPrefixes(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	served := 0
	handler := LimitPaths(l, "/auth/")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served++
	}))

	send := func(path string) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "203.0.113.7:5555"
		r.Header.Set("X-Real-IP", "203.0.113.7")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}

	if code := send("/auth/login"); code != http.StatusOK {
		t.Fatalf("first guarded request answered %d", code)
	}
	if code := send("/auth/login"); code != http.StatusTooManyRequests {
		t.Errorf("second guarded request answered %d, want 429", code)
	}
	for i := 0; i < 3; i++ {
		if code := send("/discover"); code != http.StatusOK {
			t.Fatalf("an unguarded path was limited: answered %d", code)
		}
	}
	if served != 4 {
		t.Errorf("handler served %d times, want 4", served)
	}
}
