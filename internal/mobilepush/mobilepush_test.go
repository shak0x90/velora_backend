package mobilepush

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/settings"
)

// ---------------------------------------------------------------------------
// Device handlers
// ---------------------------------------------------------------------------

type call struct{ op, user, platform, token, version string }

type fakeDevices struct{ calls []call }

func (f *fakeDevices) Register(_ context.Context, userID string, d Device) (int, error) {
	f.calls = append(f.calls, call{"register", userID, d.Platform, d.Token, d.AppVersion})
	return 3, nil
}

func (f *fakeDevices) Ack(_ context.Context, id uuid.UUID) error {
	f.calls = append(f.calls, call{"ack", "", "", id.String(), ""})
	return nil
}

func (f *fakeDevices) Unregister(_ context.Context, userID, token string) error {
	f.calls = append(f.calls, call{"unregister", userID, "", token, ""})
	return nil
}

func serve(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, *fakeDevices) {
	t.Helper()
	store := &fakeDevices{}
	d := &Devices{store: store, guard: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), "user-1")))
		})
	}}
	mux := http.NewServeMux()
	d.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec, store
}

func TestRegisterDevice(t *testing.T) {
	rec, store := serve(t, http.MethodPost, "/devices", `{"platform":" Android ","token":"fcm:abc-123_x","appVersion":"1.2.0+14"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"missedPushes":3}` {
		t.Errorf("body = %s, want the missed-push count", got)
	}
	want := call{"register", "user-1", "android", "fcm:abc-123_x", "1.2.0+14"}
	if len(store.calls) != 1 || store.calls[0] != want {
		t.Fatalf("calls = %+v, want %+v", store.calls, want)
	}
}

func TestRegisterDeviceRejects(t *testing.T) {
	for name, body := range map[string]string{
		"web platform":   `{"platform":"web","token":"t"}`,
		"no platform":    `{"token":"t"}`,
		"no token":       `{"platform":"ios","token":"  "}`,
		"slash in token": `{"platform":"ios","token":"a/b"}`,
		"huge token":     `{"platform":"ios","token":"` + strings.Repeat("a", maxTokenLength+1) + `"}`,
		"long version":   `{"platform":"ios","token":"t","appVersion":"` + strings.Repeat("9", 65) + `"}`,
		"unknown field":  `{"platform":"ios","token":"t","model":"iPhone"}`,
		"not json":       `platform=ios`,
	} {
		t.Run(name, func(t *testing.T) {
			rec, store := serve(t, http.MethodPost, "/devices", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body)
			}
			if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/problem+json") {
				t.Errorf("not a problem document: %q", rec.Header().Get("Content-Type"))
			}
			if len(store.calls) != 0 {
				t.Error("an invalid device reached the store")
			}
		})
	}
}

func TestUnregisterDeviceIsScopedAndIdempotent(t *testing.T) {
	rec, store := serve(t, http.MethodDelete, "/devices/fcm:abc-123", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	want := call{"unregister", "user-1", "", "fcm:abc-123", ""}
	if len(store.calls) != 1 || store.calls[0] != want {
		t.Fatalf("calls = %+v, want %+v", store.calls, want)
	}
}

func TestAckNeedsNoSignInAndChecksTheID(t *testing.T) {
	id := uuid.NewString()
	// serve's guard would add a user; /push/ack must not depend on it, so use
	// a mux without one.
	store := &fakeDevices{}
	d := &Devices{store: store, guard: func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	}}
	mux := http.NewServeMux()
	d.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/push/ack", strings.NewReader(`{"id":"`+id+`"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if len(store.calls) != 1 || store.calls[0].token != id {
		t.Fatalf("calls = %+v", store.calls)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/push/ack", strings.NewReader(`{"id":"not-a-uuid"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: status %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Routing and preferences
// ---------------------------------------------------------------------------

func TestAllowedFollowsPreferences(t *testing.T) {
	s := settings.Defaults()
	s.Notify.Matches = false
	s.Notify.Weekly = false
	for event, want := range map[string]bool{
		EventMessage: true, EventMatch: false, EventLike: false,
		EventDailyEdit: true, EventWeekly: false, "unknown": false,
	} {
		if got := Allowed(s, event); got != want {
			t.Errorf("Allowed(%s) = %v, want %v", event, got, want)
		}
	}
}

func TestRouterFallsBackToNoop(t *testing.T) {
	fcm := newFCM("p", http.DefaultClient, "http://unused")
	r := Router{Android: fcm}
	if s, ok := r.For(PlatformAndroid); !ok || s != Sender(fcm) {
		t.Error("android should use the configured sender")
	}
	if s, ok := r.For(PlatformIOS); ok {
		t.Error("ios has no sender configured")
	} else if err := s.Send(context.Background(), Message{}); err != nil {
		t.Errorf("noop sender failed: %v", err)
	}
	if (Router{}).Any() || !r.Any() {
		t.Error("Any() is wrong")
	}
}

func TestNewRouterConfig(t *testing.T) {
	if r, err := NewRouter(context.Background(), Config{}); err != nil || r.Any() {
		t.Fatalf("empty config must give a no-op router: %v", err)
	}
	if _, err := NewRouter(context.Background(), Config{APNsKeyID: "K", APNsTeamID: "T"}); err == nil {
		t.Error("half-configured APNs must fail loudly")
	}
	if _, err := NewRouter(context.Background(), Config{FCMProjectID: "p"}); err == nil {
		t.Error("a project with no credentials must fail loudly")
	}

	pemKey := testP8(t)
	old := readFile
	readFile = func(string) ([]byte, error) { return pemKey, nil }
	t.Cleanup(func() { readFile = old })
	r, err := NewRouter(context.Background(), Config{APNsKeyFile: "k.p8", APNsKeyID: "K", APNsTeamID: "T", APNsTopic: "com.thapaw.app"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.For(PlatformIOS); !ok {
		t.Error("APNs config should enable iOS")
	}
	if _, ok := r.For(PlatformAndroid); ok {
		t.Error("APNs alone must not claim Android")
	}
}

func TestEnqueueWithoutSendersIsANoop(t *testing.T) {
	// No router, no pool, no transaction: it must return before touching any.
	q := &Queue{}
	if err := q.Enqueue(context.Background(), nil, "u", Note{Event: EventMessage}); err != nil {
		t.Fatal(err)
	}
	if err := q.SocialHook(context.Background(), nil, "u", "matchmaker", "t", "b", ""); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// FCM
// ---------------------------------------------------------------------------

func TestFCMSend(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"name":"projects/p/messages/1"}`)
	}))
	defer srv.Close()

	f := newFCM("thapaw-prod", srv.Client(), srv.URL)
	err := f.Send(context.Background(), Message{Token: "tok", Title: "You have a new match", Body: "You and Ana liked each other.",
		Data: map[string]string{"type": "match", "profileId": "p1"}, Collapse: "match"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/projects/thapaw-prod/messages:send" {
		t.Errorf("path %s", path)
	}
	msg := got["message"].(map[string]any)
	if msg["token"] != "tok" || msg["notification"].(map[string]any)["title"] != "You have a new match" {
		t.Errorf("message %v", msg)
	}
	if msg["data"].(map[string]any)["profileId"] != "p1" {
		t.Errorf("data %v", msg["data"])
	}
	if msg["android"].(map[string]any)["collapse_key"] != "match" {
		t.Errorf("android %v", msg["android"])
	}
}

func TestFCMErrors(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
		want   error // nil means "a retryable error"
	}{
		"404":                {404, `{"error":{"status":"NOT_FOUND"}}`, ErrUnregistered},
		"unregistered":       {400, `{"error":{"status":"INVALID_ARGUMENT","details":[{"errorCode":"UNREGISTERED"}]}}`, ErrUnregistered},
		"sender mismatch":    {403, `{"error":{"status":"PERMISSION_DENIED","details":[{"errorCode":"SENDER_ID_MISMATCH"}]}}`, ErrUnregistered},
		"bad payload":        {400, `{"error":{"status":"INVALID_ARGUMENT"}}`, ErrPermanent},
		"quota":              {429, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, nil},
		"server":             {503, `oops`, nil},
		"auth (retry later)": {401, `{"error":{"status":"UNAUTHENTICATED"}}`, nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			err := newFCM("p", srv.Client(), srv.URL).Send(context.Background(), Message{Token: "t"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if tc.want == nil && (errors.Is(err, ErrUnregistered) || errors.Is(err, ErrPermanent)) {
				t.Fatalf("got %v, want a retryable error", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// APNs
// ---------------------------------------------------------------------------

func testP8(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func testAPNs(t *testing.T, handler http.HandlerFunc) *APNs {
	t.Helper()
	a, err := NewAPNs(testP8(t), "KEY123", "TEAM456", "com.thapaw.app", false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	a.base, a.client = srv.URL, srv.Client()
	return a
}

func TestAPNsSend(t *testing.T) {
	var req *http.Request
	var payload map[string]any
	a := testAPNs(t, func(w http.ResponseWriter, r *http.Request) {
		req = r
		_ = json.NewDecoder(r.Body).Decode(&payload)
	})
	err := a.Send(context.Background(), Message{Token: "abcdef0123", Title: "New message", Body: "You have a new message on Thapaw.",
		Data: map[string]string{"type": "message", "conversationId": "c1"}, Collapse: "chat-c1"})
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Path != "/3/device/abcdef0123" {
		t.Errorf("path %s", req.URL.Path)
	}
	for header, want := range map[string]string{
		"apns-topic": "com.thapaw.app", "apns-push-type": "alert", "apns-collapse-id": "chat-c1",
	} {
		if got := req.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if !strings.HasPrefix(req.Header.Get("authorization"), "bearer ey") {
		t.Errorf("authorization %q", req.Header.Get("authorization"))
	}
	alert := payload["aps"].(map[string]any)["alert"].(map[string]any)
	if alert["title"] != "New message" || payload["conversationId"] != "c1" {
		t.Errorf("payload %v", payload)
	}
}

func TestAPNsErrors(t *testing.T) {
	tests := map[string]struct {
		status int
		reason string
		want   error
	}{
		"gone":          {410, "Unregistered", ErrUnregistered},
		"bad token":     {400, "BadDeviceToken", ErrUnregistered},
		"wrong topic":   {400, "DeviceTokenNotForTopic", ErrUnregistered},
		"bad payload":   {400, "PayloadEmpty", ErrPermanent},
		"server":        {503, "ServiceUnavailable", nil},
		"too many reqs": {429, "TooManyRequests", nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			a := testAPNs(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"reason": tc.reason})
			})
			err := a.Send(context.Background(), Message{Token: "t"})
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if tc.want == nil && (errors.Is(err, ErrUnregistered) || errors.Is(err, ErrPermanent)) {
				t.Fatalf("got %v, want a retryable error", err)
			}
		})
	}
}

// An expired provider token is refreshed once and the push retried.
func TestAPNsRefreshesExpiredProviderToken(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	a := testAPNs(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Header.Get("authorization"))
		if len(seen) == 1 {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"reason":"ExpiredProviderToken"}`)
		}
	})
	// Make the cached token old enough to differ from a fresh one.
	start := time.Now().Add(-10 * time.Minute)
	a.now = func() time.Time { return start }
	if _, err := a.bearer(false); err != nil {
		t.Fatal(err)
	}
	a.now = time.Now
	if err := a.Send(context.Background(), Message{Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] == seen[1] {
		t.Fatalf("want a retry with a new token, saw %d requests", len(seen))
	}
}

func TestNewAPNsRejectsBadKeys(t *testing.T) {
	if _, err := NewAPNs([]byte("not pem"), "k", "t", "topic", true); err == nil {
		t.Error("accepted a non-PEM key")
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")})
	if _, err := NewAPNs(block, "k", "t", "topic", true); err == nil {
		t.Error("accepted a garbage key")
	}
}
