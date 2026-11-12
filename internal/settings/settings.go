// Package settings stores what a person wants to be notified about, and when
// not to be.
//
// The preferences live on the server, not only on the phone, because the
// server is the one deciding whether to send a push: a toggle that only the
// device knows about cannot stop a notification that is already on its way.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
	// Embedded zone database: tz names come from phones, and the box (or a
	// Windows dev machine) may not ship the one being asked for.
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/auth"
)

// Notify is one switch per kind of notification. Matches also covers likes:
// the settings screen offers "new matches and likes" as one choice.
type Notify struct {
	Matches   bool `json:"matches"`
	Messages  bool `json:"messages"`
	DailyEdit bool `json:"dailyEdit"`
	Weekly    bool `json:"weekly"`
	// BlindHour is the daily "Blind hour is open" reminder (9 PM local).
	BlindHour bool `json:"blindHour"`
}

// QuietHours is a nightly window, in local wall-clock time, when pushes wait
// instead of buzzing. Start and End are "HH:MM" and are both set or both
// null. TZ is an IANA zone name; it is also the zone the Daily Edit's day is
// counted in.
type QuietHours struct {
	Start *string `json:"start"`
	End   *string `json:"end"`
	TZ    string  `json:"tz"`
}

type Settings struct {
	Notify     Notify     `json:"notify"`
	QuietHours QuietHours `json:"quietHours"`
}

// Defaults is what a person who never opened the settings screen gets:
// everything on, no quiet hours, UTC.
func Defaults() Settings {
	return Settings{
		Notify:     Notify{Matches: true, Messages: true, DailyEdit: true, Weekly: true, BlindHour: true},
		QuietHours: QuietHours{TZ: "UTC"},
	}
}

// Location is the settings' zone, falling back to UTC for anything that does
// not load (a value written before validation existed, say).
func (s Settings) Location() *time.Location {
	if loc, err := loadZone(s.QuietHours.TZ); err == nil {
		return loc
	}
	return time.UTC
}

// QuietUntil reports whether now falls inside quiet hours and, if so, when
// they end. A window that starts and ends at the same minute is empty rather
// than all day: "22:00–22:00" is far more likely a mistake than a request to
// silence everything, and the switches exist for that.
func (s Settings) QuietUntil(now time.Time) (time.Time, bool) {
	q := s.QuietHours
	if q.Start == nil || q.End == nil {
		return time.Time{}, false
	}
	start, err1 := parseClock(*q.Start)
	end, err2 := parseClock(*q.End)
	if err1 != nil || err2 != nil || start == end {
		return time.Time{}, false
	}
	local := now.In(s.Location())
	minute := local.Hour()*60 + local.Minute()
	at := func(dayOffset, clock int) time.Time {
		return time.Date(local.Year(), local.Month(), local.Day()+dayOffset,
			clock/60, clock%60, 0, 0, local.Location())
	}
	if start < end { // same-day window, e.g. 13:00–15:00
		if minute >= start && minute < end {
			return at(0, end), true
		}
		return time.Time{}, false
	}
	// Overnight window, e.g. 22:00–07:00.
	if minute >= start {
		return at(1, end), true
	}
	if minute < end {
		return at(0, end), true
	}
	return time.Time{}, false
}

var clockPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func parseClock(v string) (int, error) {
	if !clockPattern.MatchString(v) {
		return 0, fmt.Errorf("%q is not HH:MM", v)
	}
	return int(v[0]-'0')*600 + int(v[1]-'0')*60 + int(v[3]-'0')*10 + int(v[4]-'0'), nil
}

// loadZone accepts IANA names only. "" and "Local" would both load, as UTC and
// as whatever the server's clock is set to, and neither is a place.
func loadZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" || len(name) > 64 {
		return nil, fmt.Errorf("%q is not a time zone", name)
	}
	return time.LoadLocation(name)
}

// ---------------------------------------------------------------------------
// Patching
// ---------------------------------------------------------------------------

// Patch is a partial update. Absent fields keep their value; quiet-hour start
// and end can also be sent as null to switch quiet hours off.
type Patch struct {
	Notify     *NotifyPatch `json:"notify"`
	QuietHours *QuietPatch  `json:"quietHours"`
}

type NotifyPatch struct {
	Matches   *bool `json:"matches"`
	Messages  *bool `json:"messages"`
	DailyEdit *bool `json:"dailyEdit"`
	Weekly    *bool `json:"weekly"`
	BlindHour *bool `json:"blindHour"`
}

type QuietPatch struct {
	Start nullable `json:"start"`
	End   nullable `json:"end"`
	TZ    *string  `json:"tz"`
}

// nullable tells "absent" from "null": UnmarshalJSON only runs for a key that
// is present, so Set stays false when the client left the field out.
type nullable struct {
	Set   bool
	Value *string
}

func (n *nullable) UnmarshalJSON(data []byte) error {
	n.Set = true
	if string(data) == "null" {
		n.Value = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return errors.New("quiet hours are \"HH:MM\" strings or null")
	}
	n.Value = &s
	return nil
}

// ErrInvalid wraps every validation failure, so the handler can answer 400
// with the sentence while anything else stays a 500.
var ErrInvalid = errors.New("invalid settings")

// Apply merges p onto s and validates the result as a whole: quiet hours are
// checked after the merge, so sending only a new start is fine when an end is
// already stored.
func (s Settings) Apply(p Patch) (Settings, error) {
	out := s
	if n := p.Notify; n != nil {
		setBool(&out.Notify.Matches, n.Matches)
		setBool(&out.Notify.Messages, n.Messages)
		setBool(&out.Notify.DailyEdit, n.DailyEdit)
		setBool(&out.Notify.Weekly, n.Weekly)
		setBool(&out.Notify.BlindHour, n.BlindHour)
	}
	if q := p.QuietHours; q != nil {
		if q.Start.Set {
			out.QuietHours.Start = q.Start.Value
		}
		if q.End.Set {
			out.QuietHours.End = q.End.Value
		}
		if q.TZ != nil {
			out.QuietHours.TZ = *q.TZ
		}
	}
	return out, out.validate()
}

func (s Settings) validate() error {
	q := s.QuietHours
	if (q.Start == nil) != (q.End == nil) {
		return fmt.Errorf("%w: set both quietHours.start and quietHours.end, or neither", ErrInvalid)
	}
	for _, v := range []*string{q.Start, q.End} {
		if v != nil {
			if _, err := parseClock(*v); err != nil {
				return fmt.Errorf("%w: quiet hours must be 24-hour \"HH:MM\", like \"22:30\"", ErrInvalid)
			}
		}
	}
	if _, err := loadZone(q.TZ); err != nil {
		return fmt.Errorf("%w: quietHours.tz must be an IANA time zone, like \"Asia/Dhaka\"", ErrInvalid)
	}
	return nil
}

func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

// ---------------------------------------------------------------------------
// Storage
// ---------------------------------------------------------------------------

// Querier is satisfied by both the pool and a transaction, so the push
// enqueue path can read preferences inside the transaction that sends the
// message.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Load reads one person's settings, or the defaults if they never saved any.
func Load(ctx context.Context, q Querier, userID string) (Settings, error) {
	s := Defaults()
	err := q.QueryRow(ctx, `
		select notify_matches, notify_messages, notify_daily_edit, notify_weekly, notify_blind_hour,
		       quiet_start, quiet_end, tz
		from user_settings where user_id = $1
	`, userID).Scan(&s.Notify.Matches, &s.Notify.Messages, &s.Notify.DailyEdit, &s.Notify.Weekly, &s.Notify.BlindHour,
		&s.QuietHours.Start, &s.QuietHours.End, &s.QuietHours.TZ)
	if errors.Is(err, pgx.ErrNoRows) {
		return Defaults(), nil
	}
	return s, err
}

// Store is the persistence the handlers need; tests replace it.
type Store interface {
	Get(ctx context.Context, userID string) (Settings, error)
	// Update applies change to the stored settings atomically and saves the
	// result, unless change fails, in which case nothing is written.
	Update(ctx context.Context, userID string, change func(Settings) (Settings, error)) (Settings, error)
}

type pgStore struct{ pool *pgxpool.Pool }

func (p pgStore) Get(ctx context.Context, userID string) (Settings, error) {
	return Load(ctx, p.pool, userID)
}

// Update locks the row for the read-modify-write, so two toggles flipped in
// quick succession cannot each overwrite the other with a stale copy. The
// row is created first (with the column defaults, which equal Defaults()) so
// there is always something to lock.
func (p pgStore) Update(ctx context.Context, userID string, change func(Settings) (Settings, error)) (Settings, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Settings{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err = tx.Exec(ctx, `insert into user_settings (user_id) values ($1) on conflict do nothing`, userID); err != nil {
		return Settings{}, err
	}
	current := Defaults()
	if err = tx.QueryRow(ctx, `
		select notify_matches, notify_messages, notify_daily_edit, notify_weekly, notify_blind_hour,
		       quiet_start, quiet_end, tz
		from user_settings where user_id = $1 for update
	`, userID).Scan(&current.Notify.Matches, &current.Notify.Messages, &current.Notify.DailyEdit,
		&current.Notify.Weekly, &current.Notify.BlindHour,
		&current.QuietHours.Start, &current.QuietHours.End, &current.QuietHours.TZ); err != nil {
		return Settings{}, err
	}
	next, err := change(current)
	if err != nil {
		return Settings{}, err
	}
	if _, err = tx.Exec(ctx, `
		update user_settings set
			notify_matches = $2, notify_messages = $3, notify_daily_edit = $4, notify_weekly = $5,
			notify_blind_hour = $9, quiet_start = $6, quiet_end = $7, tz = $8, updated_at = now()
		where user_id = $1
	`, userID, next.Notify.Matches, next.Notify.Messages, next.Notify.DailyEdit, next.Notify.Weekly,
		next.QuietHours.Start, next.QuietHours.End, next.QuietHours.TZ, next.Notify.BlindHour); err != nil {
		return Settings{}, err
	}
	return next, tx.Commit(ctx)
}

type Service struct {
	store Store
	guard func(http.Handler) http.Handler
}

func New(pool *pgxpool.Pool, users *auth.Service) *Service {
	return &Service{store: pgStore{pool: pool}, guard: users.RequireAuth}
}
