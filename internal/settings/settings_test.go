package settings

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shak0x90/velora_backend/internal/auth"
)

func str(s string) *string { return &s }

func TestQuietUntil(t *testing.T) {
	dhaka, _ := time.LoadLocation("Asia/Dhaka") // UTC+6, no DST
	overnight := Settings{QuietHours: QuietHours{Start: str("22:00"), End: str("07:00"), TZ: "Asia/Dhaka"}}
	daytime := Settings{QuietHours: QuietHours{Start: str("13:00"), End: str("15:30"), TZ: "UTC"}}
	at := func(loc *time.Location, day, h, m int) time.Time {
		return time.Date(2026, 11, day, h, m, 0, 0, loc)
	}
	tests := map[string]struct {
		s     Settings
		now   time.Time
		quiet bool
		until time.Time
	}{
		"overnight, late evening":    {overnight, at(dhaka, 9, 23, 10), true, at(dhaka, 10, 7, 0)},
		"overnight, early morning":   {overnight, at(dhaka, 10, 6, 59), true, at(dhaka, 10, 7, 0)},
		"overnight, at end":          {overnight, at(dhaka, 10, 7, 0), false, time.Time{}},
		"overnight, afternoon":       {overnight, at(dhaka, 10, 15, 0), false, time.Time{}},
		"overnight, read from UTC":   {overnight, at(time.UTC, 9, 16, 30), true, at(dhaka, 10, 7, 0)}, // 22:30 Dhaka
		"daytime, inside":            {daytime, at(time.UTC, 9, 14, 0), true, at(time.UTC, 9, 15, 30)},
		"daytime, before":            {daytime, at(time.UTC, 9, 12, 59), false, time.Time{}},
		"no quiet hours":             {Defaults(), at(time.UTC, 9, 3, 0), false, time.Time{}},
		"equal start and end is off": {Settings{QuietHours: QuietHours{Start: str("22:00"), End: str("22:00"), TZ: "UTC"}}, at(time.UTC, 9, 22, 0), false, time.Time{}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			until, quiet := tc.s.QuietUntil(tc.now)
			if quiet != tc.quiet {
				t.Fatalf("quiet = %v, want %v", quiet, tc.quiet)
			}
			if quiet && !until.Equal(tc.until) {
				t.Fatalf("until = %v, want %v", until, tc.until)
			}
		})
	}
}

func TestApplyMergesPartialPatch(t *testing.T) {
	var p Patch
	if err := json.Unmarshal([]byte(`{"notify":{"messages":false},"quietHours":{"start":"23:00","end":"06:30","tz":"Europe/London"}}`), &p); err != nil {
		t.Fatal(err)
	}
	got, err := Defaults().Apply(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Notify.Messages || !got.Notify.Matches || !got.Notify.DailyEdit || !got.Notify.Weekly {
		t.Errorf("notify = %+v, want only messages off", got.Notify)
	}
	if *got.QuietHours.Start != "23:00" || *got.QuietHours.End != "06:30" || got.QuietHours.TZ != "Europe/London" {
		t.Errorf("quiet hours = %+v", got.QuietHours)
	}
}

func TestApplyNullClearsQuietHoursAndAbsentKeeps(t *testing.T) {
	stored := Settings{Notify: Defaults().Notify, QuietHours: QuietHours{Start: str("22:00"), End: str("07:00"), TZ: "Asia/Dhaka"}}

	var keep Patch
	_ = json.Unmarshal([]byte(`{"quietHours":{"tz":"UTC"}}`), &keep)
	got, err := stored.Apply(keep)
	if err != nil || got.QuietHours.Start == nil || got.QuietHours.TZ != "UTC" {
		t.Fatalf("absent start/end should keep them: %+v, %v", got.QuietHours, err)
	}

	var clear Patch
	_ = json.Unmarshal([]byte(`{"quietHours":{"start":null,"end":null}}`), &clear)
	got, err = stored.Apply(clear)
	if err != nil || got.QuietHours.Start != nil || got.QuietHours.End != nil {
		t.Fatalf("null should clear quiet hours: %+v, %v", got.QuietHours, err)
	}
	if got.QuietHours.TZ != "Asia/Dhaka" {
		t.Errorf("clearing quiet hours changed the zone to %q", got.QuietHours.TZ)
	}
}

func TestApplyRejects(t *testing.T) {
	for name, body := range map[string]string{
		"only start":   `{"quietHours":{"start":"22:00"}}`,
		"bad clock":    `{"quietHours":{"start":"10pm","end":"07:00"}}`,
		"24:00":        `{"quietHours":{"start":"24:00","end":"07:00"}}`,
		"unknown zone": `{"quietHours":{"tz":"Mars/Olympus"}}`,
		"server local": `{"quietHours":{"tz":"Local"}}`,
		"empty zone":   `{"quietHours":{"tz":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var p Patch
			if err := json.Unmarshal([]byte(body), &p); err != nil {
				t.Fatal(err)
			}
			if _, err := Defaults().Apply(p); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

type memStore struct {
	saved   map[string]Settings
	updates int
}

func (m *memStore) Get(_ context.Context, userID string) (Settings, error) {
	if s, ok := m.saved[userID]; ok {
		return s, nil
	}
	return Defaults(), nil
}

func (m *memStore) Update(ctx context.Context, userID string, change func(Settings) (Settings, error)) (Settings, error) {
	current, _ := m.Get(ctx, userID)
	next, err := change(current)
	if err != nil {
		return Settings{}, err
	}
	m.updates++
	m.saved[userID] = next
	return next, nil
}

func newTestService() (*Service, *memStore) {
	store := &memStore{saved: map[string]Settings{}}
	guard := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUserID(r.Context(), "user-1")))
		})
	}
	return &Service{store: store, guard: guard}, store
}

func do(t *testing.T, s *Service, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, "/me/settings", strings.NewReader(body)))
	return rec
}

func TestGetSettingsDefaults(t *testing.T) {
	s, _ := newTestService()
	rec := do(t, s, http.MethodGet, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	notify := got["notify"].(map[string]any)
	quiet := got["quietHours"].(map[string]any)
	if notify["matches"] != true || notify["messages"] != true || notify["dailyEdit"] != true || notify["weekly"] != true {
		t.Errorf("notify = %v, want everything on", notify)
	}
	if quiet["start"] != nil || quiet["end"] != nil || quiet["tz"] != "UTC" {
		t.Errorf("quietHours = %v, want null/null/UTC", quiet)
	}
}

func TestPatchSettingsSavesAndEchoes(t *testing.T) {
	s, store := newTestService()
	rec := do(t, s, http.MethodPatch, `{"notify":{"weekly":false},"quietHours":{"start":"22:00","end":"07:00","tz":"Asia/Dhaka"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	saved := store.saved["user-1"]
	if saved.Notify.Weekly || !saved.Notify.Messages || saved.QuietHours.TZ != "Asia/Dhaka" {
		t.Errorf("saved %+v", saved)
	}
	var echoed Settings
	_ = json.Unmarshal(rec.Body.Bytes(), &echoed)
	if echoed.Notify != saved.Notify || *echoed.QuietHours.Start != "22:00" {
		t.Errorf("response %+v does not match saved %+v", echoed, saved)
	}

	// A second, partial patch keeps the first one's values.
	rec = do(t, s, http.MethodPatch, `{"notify":{"messages":false}}`)
	if rec.Code != http.StatusOK || store.saved["user-1"].Notify.Weekly || store.saved["user-1"].QuietHours.Start == nil {
		t.Fatalf("partial patch lost earlier values: %d %+v", rec.Code, store.saved["user-1"])
	}
}

func TestPatchSettingsRejectsWithoutSaving(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":      `{"notify":{"marketing":true}}`,
		"unknown top level":  `{"theme":"dark"}`,
		"half quiet hours":   `{"quietHours":{"start":"22:00"}}`,
		"bad zone":           `{"quietHours":{"tz":"Nowhere/Special"}}`,
		"wrong type":         `{"notify":{"matches":"yes"}}`,
		"quiet hours number": `{"quietHours":{"start":2200,"end":700}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, store := newTestService()
			rec := do(t, s, http.MethodPatch, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Errorf("content type %q", ct)
			}
			if store.updates != 0 {
				t.Error("a rejected patch was saved")
			}
		})
	}
}
