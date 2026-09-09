// Package safety is blocking, reporting, unmatching, and the queue a
// moderator works through.
//
// The rule this package exists to uphold: a protection the interface promises
// has to be real. A block that leaks anywhere is worse than no block at all,
// because the person believes they are safe and behaves accordingly. So a
// block is enforced in every read path rather than only where it was created,
// and it dissolves the connection it was aimed at rather than merely hiding it.
package safety

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/chatlog"
)

var (
	ErrSelfDirected = errors.New("you cannot do that to your own account")
	ErrNoSuchPerson = errors.New("no such person")
	ErrNoSuchMatch  = errors.New("no such match")
	ErrNoSuchReport = errors.New("no such report")
	// ErrTooMany is a rate limit. Reporting is limited too: a queue someone can
	// flood is a queue that hides real reports.
	ErrTooMany = errors.New("too many for now")
)

// Daily caps. Generous enough that no honest use meets them, low enough that
// scripted abuse does.
const (
	MaxReportsPerDay = 20
	MaxBlocksPerDay  = 100
)

type Service struct {
	pool  *pgxpool.Pool
	guard func(http.Handler) http.Handler
}

func New(pool *pgxpool.Pool, users *auth.Service) *Service {
	return &Service{pool: pool, guard: users.RequireAuth}
}

// ---------------------------------------------------------------------------
// Blocking
// ---------------------------------------------------------------------------

// Block cuts the connection in both directions and removes what it produced.
//
// It is one transaction over several facts: the block itself, the match it
// dissolves, the likes in each direction, and any notification naming the
// other person. Half of that committed would leave someone blocked but still
// matched — visible in an inbox, reachable — which is exactly the failure a
// block exists to prevent.
func (s *Service) Block(ctx context.Context, userID, targetID, reason string) error {
	if userID == targetID {
		return ErrSelfDirected
	}
	if err := s.underDailyLimit(ctx, "blocks", "user_id", userID, MaxBlocksPerDay); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := chatlog.LockPair(ctx, tx, userID, targetID); err != nil {
		return err
	}
	if err := chatlog.ClosePair(ctx, tx, userID, targetID, false); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		insert into blocks (user_id, blocked_id, reason) values ($1, $2, $3)
		on conflict (user_id, blocked_id) do update set reason = excluded.reason
	`, userID, targetID, strings.TrimSpace(reason)); err != nil {
		return translate(err)
	}

	if _, err := tx.Exec(ctx, `
		delete from matches
		where user_a = least($1::uuid, $2::uuid) and user_b = greatest($1::uuid, $2::uuid)
	`, userID, targetID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		delete from likes
		where (from_user_id = $1 and to_user_id = $2)
		   or (from_user_id = $2 and to_user_id = $1)
	`, userID, targetID); err != nil {
		return err
	}
	// A block should not leave "you matched with X" sitting in the list.
	if _, err := tx.Exec(ctx, `
		delete from notifications where user_id = $1 and profile_id = $2
	`, userID, targetID); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// Unblock lifts the block. It does not restore the match it dissolved —
// unblocking allows contact again, it does not undo what happened.
func (s *Service) Unblock(ctx context.Context, userID, targetID string) error {
	_, err := s.pool.Exec(ctx,
		`delete from blocks where user_id = $1 and blocked_id = $2`, userID, targetID)
	return translate(err)
}

// BlockedID is one entry in the blocked list: ids and times only. Rendering a
// name is the caller's job, and most callers should not need one.
type BlockedID struct {
	ProfileID string    `json:"profileId"`
	CreatedAt time.Time `json:"createdAt"`
}

func (s *Service) Blocked(ctx context.Context, userID string) ([]BlockedID, error) {
	rows, err := s.pool.Query(ctx, `
		select blocked_id::text, created_at from blocks
		where user_id = $1 order by created_at desc
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("load blocks: %w", err)
	}
	defer rows.Close()

	out := []BlockedID{}
	for rows.Next() {
		var entry BlockedID
		if err := rows.Scan(&entry.ProfileID, &entry.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Unmatching
// ---------------------------------------------------------------------------

// Unmatch removes a match from both sides. Unlike a block it says nothing
// about the other person; it just ends the connection.
//
// The likes go with it, and a pass is recorded each way. Leaving either would
// let the pair re-match on the next tap, or put the person back in the other's
// feed tomorrow — neither is what someone who unmatched asked for.
func (s *Service) Unmatch(ctx context.Context, userID, matchID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var a, b string
	err = tx.QueryRow(ctx, `
		select user_a::text, user_b::text from matches
		where id = $1 and (user_a = $2 or user_b = $2)
	`, matchID, userID).Scan(&a, &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoSuchMatch
	}
	if err != nil {
		return translate(err)
	}
	if err = chatlog.LockPair(ctx, tx, a, b); err != nil {
		return err
	}
	if err = chatlog.ClosePair(ctx, tx, a, b, true); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `delete from matches where id=$1`, matchID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		delete from likes
		where (from_user_id = $1 and to_user_id = $2)
		   or (from_user_id = $2 and to_user_id = $1)
	`, a, b); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		insert into passes (user_id, target_id) values ($1, $2), ($2, $1)
		on conflict (user_id, target_id) do nothing
	`, a, b); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

type ReportInput struct {
	ProfileID string `json:"profileId"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail"`
	Context   string `json:"context"`
	// Block is the "and block them" checkbox. Reporting someone you want gone
	// should not require a second, separate action.
	Block bool `json:"block"`
}

var (
	validReasons = map[string]bool{
		"harassment": true, "spam": true, "fake_profile": true,
		"inappropriate_photos": true, "underage": true,
		"offline_behaviour": true, "other": true,
	}
	validContexts = map[string]bool{
		"profile": true, "photo": true, "message": true, "prompt": true,
	}
)

const maxDetailLen = 2000

// Report files a report, and optionally blocks in the same breath.
//
// Reporting the same person again updates the open report rather than adding
// another: twenty rows about one grievance bury nineteen other people's.
func (s *Service) Report(ctx context.Context, reporterID string, input ReportInput) error {
	if reporterID == input.ProfileID {
		return ErrSelfDirected
	}
	if !validReasons[input.Reason] {
		return fmt.Errorf("%q is not a reason we recognise", input.Reason)
	}
	where := input.Context
	if where == "" {
		where = "profile"
	}
	if !validContexts[where] {
		return fmt.Errorf("%q is not a place we recognise", input.Context)
	}
	detail := strings.TrimSpace(input.Detail)
	if len([]rune(detail)) > maxDetailLen {
		return fmt.Errorf("keep the detail under %d characters", maxDetailLen)
	}
	if err := s.underDailyLimit(ctx, "reports", "reporter_id", reporterID, MaxReportsPerDay); err != nil {
		return err
	}

	// The conflict target repeats the partial index's predicate, so an already
	// reviewed report does not block a fresh one about new behaviour.
	if _, err := s.pool.Exec(ctx, `
		insert into reports (id, reporter_id, subject_id, reason, detail, context)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (reporter_id, subject_id) where status in ('open', 'reviewing')
		do update set
			reason = excluded.reason,
			detail = excluded.detail,
			context = excluded.context,
			created_at = now()
	`, uuid.New(), reporterID, input.ProfileID, input.Reason, detail, where); err != nil {
		return translate(err)
	}

	if input.Block {
		return s.Block(ctx, reporterID, input.ProfileID, "reported: "+input.Reason)
	}
	return nil
}

// underDailyLimit counts a caller's rows from the last day. Table and column
// are chosen by the caller from literals, never from input.
func (s *Service) underDailyLimit(ctx context.Context, table, column, userID string, limit int) error {
	var count int
	if err := s.pool.QueryRow(ctx, fmt.Sprintf(
		`select count(*) from %s where %s = $1 and created_at > now() - interval '1 day'`,
		table, column), userID).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return ErrTooMany
	}
	return nil
}

// ---------------------------------------------------------------------------
// Moderation
// ---------------------------------------------------------------------------

// Report is a queue entry. It carries both accounts' ids so a moderator can
// pull either profile; nothing here decides what they should see.
type Report struct {
	ID         string     `json:"id"`
	ReporterID string     `json:"reporterId"`
	SubjectID  string     `json:"subjectId"`
	Reason     string     `json:"reason"`
	Detail     string     `json:"detail"`
	Context    string     `json:"context"`
	Status     string     `json:"status"`
	Resolution string     `json:"resolution"`
	ReviewedAt *time.Time `json:"reviewedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	// PriorReports is how many times this subject has been reported by anyone.
	// One report is a disagreement; the fifth is a pattern, and a queue that
	// hides that makes every decision worse.
	PriorReports int `json:"priorReports"`
}

// Queue lists reports for review, oldest first so nothing waits forever.
func (s *Service) Queue(ctx context.Context, status string, limit int) ([]Report, error) {
	if status == "" {
		status = "open"
	}
	if !validStatuses[status] {
		return nil, fmt.Errorf("%q is not a status we recognise", status)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := s.pool.Query(ctx, `
		select r.id::text, r.reporter_id::text, r.subject_id::text,
		       r.reason, r.detail, r.context, r.status, r.resolution,
		       r.reviewed_at, r.created_at,
		       (select count(*) from reports p where p.subject_id = r.subject_id)
		from reports r
		where r.status = $1
		order by r.created_at
		limit $2
	`, status, limit)
	if err != nil {
		return nil, fmt.Errorf("load report queue: %w", err)
	}
	defer rows.Close()

	out := []Report{}
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.ID, &r.ReporterID, &r.SubjectID,
			&r.Reason, &r.Detail, &r.Context, &r.Status, &r.Resolution,
			&r.ReviewedAt, &r.CreatedAt, &r.PriorReports); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Resolution is a moderator's decision on one report.
type Resolution struct {
	Status     string `json:"status"`
	Resolution string `json:"resolution"`
	// Suspend acts on the account as well as the report. Explicit rather than
	// inferred from the status, so nobody is suspended by a dropdown.
	Suspend bool `json:"suspend"`
}

var validStatuses = map[string]bool{
	"open": true, "reviewing": true, "actioned": true, "dismissed": true,
}

func (s *Service) Resolve(ctx context.Context, moderatorID, reportID string, decision Resolution) error {
	if !validStatuses[decision.Status] {
		return fmt.Errorf("%q is not a status we recognise", decision.Status)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var subjectID string
	err = tx.QueryRow(ctx, `
		update reports set
			status = $1, resolution = $2,
			reviewed_by = $3, reviewed_at = now()
		where id = $4
		returning subject_id::text
	`, decision.Status, strings.TrimSpace(decision.Resolution),
		moderatorID, reportID).Scan(&subjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoSuchReport
	}
	if err != nil {
		return translate(err)
	}

	if decision.Suspend {
		if _, err := tx.Exec(ctx,
			`update users set status = 'suspended' where id = $1`, subjectID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RequireModerator gates the queue. It reads the flag per request rather than
// trusting a claim in the token: revoking moderation should take effect now,
// not whenever the holder's access token happens to expire.
func (s *Service) RequireModerator(next http.Handler) http.Handler {
	return s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var isModerator bool
		err := s.pool.QueryRow(r.Context(),
			`select is_moderator from users where id = $1 and status = 'active'`,
			auth.UserIDFrom(r.Context())).Scan(&isModerator)
		if err != nil || !isModerator {
			// Deliberately a 404: a 403 confirms to whoever is probing that
			// there is a moderation surface here worth attacking.
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func translate(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if strings.Contains(message, "violates foreign key constraint") ||
		strings.Contains(message, "invalid input syntax for type uuid") {
		return ErrNoSuchPerson
	}
	return err
}
