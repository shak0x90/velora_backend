package discovery

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/compat"
	"github.com/shak0x90/velora_backend/internal/domain"
	"github.com/shak0x90/velora_backend/internal/settings"
)

// editHour is when a new Daily Edit becomes available, in the person's own
// time zone. The edit "day" runs from one 09:00 to the next, which is what
// makes the app's "next batch at 9:00 AM" true.
const editHour = 9

// dailyEditRetention is how many past edits are kept per person. Nothing
// reads them today; a short history is useful when someone reports "the app
// showed me X yesterday", and it bounds the table.
const dailyEditRetention = 14

// DailyEdit is the response of GET /discover/daily-edit.
type DailyEdit struct {
	// Picks are the batch's people not yet passed, liked or matched, in the
	// order they were ranked when the batch was made.
	Picks []domain.RankedProfile `json:"picks"`
	// Date is the edit day, YYYY-MM-DD in the person's zone.
	Date string `json:"date"`
	// NextBatchAt is when the next edit replaces this one.
	NextBatchAt time.Time `json:"nextBatchAt"`
	// Remaining counts the picks still waiting for a decision; zero means the
	// edit is complete.
	Remaining int `json:"remaining"`
	// Total is how many people the batch started with (normally 5).
	Total int `json:"total"`
}

// StoredPick is one frozen entry of a batch.
type StoredPick struct {
	ProfileID string
	RankScore float64
}

// DailyStore is the persistence the Daily Edit needs; tests replace it.
type DailyStore interface {
	// Location is the zone the person's edit day is counted in.
	Location(ctx context.Context, userID string) (*time.Location, error)
	// Batch returns the batch stored for (userID, date), calling generate and
	// storing its result if there is none yet. Two concurrent first requests
	// must end up with the same batch.
	Batch(ctx context.Context, userID string, date time.Time, generate func() ([]StoredPick, error)) ([]StoredPick, error)
	// Decided returns the subset of ids the person has passed, liked or
	// matched with.
	Decided(ctx context.Context, userID string, ids []string) (map[string]bool, error)
}

// editDay returns the edit day containing now in loc, and when it ends.
func editDay(now time.Time, loc *time.Location) (day time.Time, next time.Time) {
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), editHour, 0, 0, 0, loc)
	if local.Before(start) {
		start = time.Date(local.Year(), local.Month(), local.Day()-1, editHour, 0, 0, 0, loc)
	}
	day = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	next = time.Date(start.Year(), start.Month(), start.Day()+1, editHour, 0, 0, 0, loc)
	return day, next
}

// DailyEdit returns today's frozen batch, making it on the first request of
// the day. The batch is chosen once — the top of the default-filter feed at
// that moment — and then only shrinks as the person decides about each pick,
// or as someone in it becomes unavailable (hidden, blocked, suspended).
func (s *Service) DailyEdit(ctx context.Context, userID string) (DailyEdit, error) {
	if s.daily == nil {
		return DailyEdit{}, fmt.Errorf("daily edit store not configured")
	}
	me, err := s.viewer(ctx, userID)
	if err != nil {
		return DailyEdit{}, err
	}
	loc, err := s.daily.Location(ctx, userID)
	if err != nil {
		return DailyEdit{}, err
	}
	day, next := editDay(s.now(), loc)

	batch, err := s.daily.Batch(ctx, userID, day, func() ([]StoredPick, error) {
		candidates, err := s.profiles.Candidates(ctx, me, candidatePool)
		if err != nil {
			return nil, err
		}
		ranked := compat.DailyPicks(me, candidates, dailyPickCount)
		out := make([]StoredPick, len(ranked))
		for i, r := range ranked {
			out[i] = StoredPick{ProfileID: r.Profile.ID, RankScore: r.RankScore}
		}
		return out, nil
	})
	if err != nil {
		return DailyEdit{}, err
	}

	ids := make([]string, len(batch))
	for i, p := range batch {
		ids[i] = p.ProfileID
	}
	decided, err := s.daily.Decided(ctx, userID, ids)
	if err != nil {
		return DailyEdit{}, err
	}
	var open []string
	for _, id := range ids {
		if !decided[id] {
			open = append(open, id)
		}
	}
	// LoadMany applies the visibility rules, so someone who went hidden or
	// blocked the viewer since the batch was made silently drops out.
	people, err := s.profiles.LoadMany(ctx, me, open)
	if err != nil {
		return DailyEdit{}, err
	}
	byID := make(map[string]domain.Profile, len(people))
	for _, p := range people {
		byID[p.ID] = p
	}
	picks := []domain.RankedProfile{}
	for _, stored := range batch {
		p, ok := byID[stored.ProfileID]
		if !ok || decided[stored.ProfileID] {
			continue
		}
		picks = append(picks, domain.RankedProfile{
			Profile:       p,
			Compatibility: compat.Calculate(me, p),
			RankScore:     stored.RankScore,
		})
	}
	return DailyEdit{
		Picks:       picks,
		Date:        day.Format(time.DateOnly),
		NextBatchAt: next,
		Remaining:   len(picks),
		Total:       len(batch),
	}, nil
}

// ---------------------------------------------------------------------------
// Postgres store
// ---------------------------------------------------------------------------

type pgDaily struct{ pool *pgxpool.Pool }

// NewDailyStore is the production DailyStore.
func NewDailyStore(pool *pgxpool.Pool) DailyStore { return pgDaily{pool: pool} }

func (p pgDaily) Location(ctx context.Context, userID string) (*time.Location, error) {
	prefs, err := settings.Load(ctx, p.pool, userID)
	if err != nil {
		return nil, err
	}
	return prefs.Location(), nil
}

// Batch serialises on the person, so two tabs opening the app at once cannot
// each store a different five. An empty result is not stored: someone with no
// candidates this morning should see people who join this afternoon.
func (p pgDaily) Batch(ctx context.Context, userID string, date time.Time, generate func() ([]StoredPick, error)) ([]StoredPick, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err = tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('daily-edit:' || $1)::bigint)`, userID); err != nil {
		return nil, err
	}
	stored, err := loadBatch(ctx, tx, userID, date)
	if err != nil || len(stored) > 0 {
		return stored, err
	}
	fresh, err := generate()
	if err != nil {
		return nil, err
	}
	for i, pick := range fresh {
		if _, err = tx.Exec(ctx, `
			insert into daily_edits (user_id, edit_date, profile_id, position, rank_score)
			values ($1, $2, $3, $4, $5)
			on conflict do nothing
		`, userID, date, pick.ProfileID, i, pick.RankScore); err != nil {
			return nil, err
		}
	}
	if _, err = tx.Exec(ctx, `delete from daily_edits where user_id = $1 and edit_date < $2::date - $3::int`,
		userID, date, dailyEditRetention); err != nil {
		return nil, err
	}
	return fresh, tx.Commit(ctx)
}

func loadBatch(ctx context.Context, tx pgx.Tx, userID string, date time.Time) ([]StoredPick, error) {
	rows, err := tx.Query(ctx, `
		select profile_id::text, rank_score from daily_edits
		where user_id = $1 and edit_date = $2
		order by position
	`, userID, date)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (StoredPick, error) {
		var p StoredPick
		err := row.Scan(&p.ProfileID, &p.RankScore)
		return p, err
	})
}

// Decided mirrors the feed's own exclusions (profile.candidateWhere): a pass,
// a like still pending, or a match — a like that matched is deleted, so the
// match is what remembers it.
func (p pgDaily) Decided(ctx context.Context, userID string, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := p.pool.Query(ctx, `
		select c.pid::text from unnest($2::uuid[]) as c(pid)
		where exists (select 1 from passes where user_id = $1 and target_id = c.pid)
		   or exists (select 1 from likes where from_user_id = $1 and to_user_id = c.pid)
		   or exists (select 1 from matches
		              where user_a = least($1::uuid, c.pid) and user_b = greatest($1::uuid, c.pid))
	`, userID, ids)
	if err != nil {
		return nil, err
	}
	decided, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, id := range decided {
		out[id] = true
	}
	return out, nil
}
