// Package social owns the decisions people make about each other: passing,
// liking, the matches those produce, and the notifications that announce them.
//
// It deliberately deals in ids rather than profiles. A like is a fact about
// two user ids; rendering it needs a profile, but fetching one is discovery's
// job, so this package stays a thin layer over four tables and never grows a
// second copy of the profile read.
package social

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
	"github.com/shak0x90/velora_backend/internal/domain"
)

var (
	// ErrSelfDirected is liking or passing yourself. The schema rejects it too;
	// catching it here turns a constraint violation into a sentence.
	ErrSelfDirected = errors.New("you cannot do that to your own profile")
	ErrNoSuchPerson = errors.New("no such person")
	ErrNoSuchLike   = errors.New("no such like")
	// ErrBlocked is a like aimed at someone either side has blocked. The
	// message never says which direction: telling someone they were blocked is
	// exactly the information a block is meant to withhold.
	ErrBlocked = errors.New("not available")
	ErrTooMany = errors.New("daily limit reached")
)

// MaxLikesPerDayFree is the spam limit. A person swiping attentively will not
// reach it in a day; a script reaches it in seconds. Premium lifts it, which
// is also why the cap cannot be the only thing standing between the app and
// abuse — it is a speed bump, not a wall.
const MaxLikesPerDayFree = 60

// newMatchWindow is how long a match counts as new. The clients badge fresh
// matches; once messaging lands this becomes "no messages yet" instead, which
// is the question the badge is really asking.
const newMatchWindow = 24 * time.Hour

type Service struct {
	pool  *pgxpool.Pool
	guard func(http.Handler) http.Handler
}

func New(pool *pgxpool.Pool, users *auth.Service) *Service {
	return &Service{pool: pool, guard: users.RequireAuth}
}

// ---------------------------------------------------------------------------
// Passing
// ---------------------------------------------------------------------------

// Pass hides someone from the viewer's feed for good. Passing twice is the
// same fact as passing once, so a repeat is a no-op rather than an error — a
// double tap on a slow connection must not fail.
func (s *Service) Pass(ctx context.Context, userID, targetID string) error {
	if userID == targetID {
		return ErrSelfDirected
	}
	_, err := s.pool.Exec(ctx, `
		insert into passes (user_id, target_id) values ($1, $2)
		on conflict (user_id, target_id) do nothing
	`, userID, targetID)
	return translateFK(err)
}

// ---------------------------------------------------------------------------
// Liking
// ---------------------------------------------------------------------------

// LikeInput is what a client sends. Kind and Label describe what was liked, so
// the recipient sees "Liked your photo" rather than a bare name.
type LikeInput struct {
	ProfileID string          `json:"profileId"`
	Kind      domain.LikeKind `json:"kind"`
	Label     string          `json:"label"`
	Priority  bool            `json:"priority"`
}

// LikeResult mirrors the clients' shape: a like either matches or it does not.
type LikeResult struct {
	Matched bool                `json:"matched"`
	Match   *domain.MatchRecord `json:"match,omitempty"`
}

var validKinds = map[domain.LikeKind]bool{
	domain.LikeProfile: true, domain.LikePhoto: true,
	domain.LikePrompt: true, domain.LikeInterest: true,
}

// Like records interest and, when it is mutual, creates the match.
//
// The whole thing is one transaction because a match is several facts that
// must agree: the pair row, the removal of the likes it consumed, and the
// notifications. Committing half of that would leave someone matched but still
// listed as a pending like.
func (s *Service) Like(ctx context.Context, userID string, input LikeInput) (LikeResult, error) {
	if userID == input.ProfileID {
		return LikeResult{}, ErrSelfDirected
	}
	kind := input.Kind
	if kind == "" {
		kind = domain.LikeProfile
	}
	if !validKinds[kind] {
		return LikeResult{}, fmt.Errorf("%q is not a kind of like", kind)
	}
	label := strings.TrimSpace(input.Label)
	if label == "" {
		label = "Liked your profile"
	}

	if err := s.underLikeLimit(ctx, userID); err != nil {
		return LikeResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LikeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialise on the pair before touching either row.
	//
	// Two people liking each other at the same instant would otherwise each
	// insert their like and then look for the other's, which is still
	// uncommitted and therefore invisible at read committed. Both would commit
	// happily and neither would match — the worst possible failure here, since
	// the app would have silently thrown away a mutual like. The key is built
	// from the ordered pair so both sides queue on the same lock.
	lockLow, lockHigh := userID, input.ProfileID
	if lockLow > lockHigh {
		lockLow, lockHigh = lockHigh, lockLow
	}
	if _, err := tx.Exec(ctx,
		`select pg_advisory_xact_lock(hashtext($1 || $2)::bigint)`,
		lockLow, lockHigh); err != nil {
		return LikeResult{}, err
	}

	// Checked inside the transaction, after the lock: a block landing between
	// the check and the insert would otherwise slip a like past it.
	var blocked bool
	if err := tx.QueryRow(ctx, `
		select exists (
			select 1 from blocks
			where (user_id = $1 and blocked_id = $2)
			   or (user_id = $2 and blocked_id = $1)
		)
	`, userID, input.ProfileID).Scan(&blocked); err != nil {
		return LikeResult{}, translateFK(err)
	}
	if blocked {
		return LikeResult{}, ErrBlocked
	}

	// A repeat like updates the existing row rather than adding one, so the
	// recipient's list cannot fill with the same person twice.
	if _, err := tx.Exec(ctx, `
		insert into likes (id, from_user_id, to_user_id, kind, label, priority)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (from_user_id, to_user_id) do update set
			kind = excluded.kind, label = excluded.label,
			priority = excluded.priority, created_at = now()
	`, uuid.New(), userID, input.ProfileID, kind, label, input.Priority); err != nil {
		return LikeResult{}, translateFK(err)
	}

	var reciprocal bool
	if err := tx.QueryRow(ctx, `
		select exists (select 1 from likes where from_user_id = $1 and to_user_id = $2)
	`, input.ProfileID, userID).Scan(&reciprocal); err != nil {
		return LikeResult{}, err
	}

	if !reciprocal {
		// Tell them someone liked them, but never who: revealing that is what
		// the likes screen is for, and naming the person here would give away
		// the thing the screen exists to show.
		if err := notify(ctx, tx, input.ProfileID, domain.NotifyLike,
			"Someone liked you", "Open your likes to see who is waiting.", ""); err != nil {
			return LikeResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return LikeResult{}, err
		}
		return LikeResult{Matched: false}, nil
	}

	match, err := createMatch(ctx, tx, userID, input.ProfileID)
	if err != nil {
		return LikeResult{}, err
	}

	myName, err := firstName(ctx, tx, userID)
	if err != nil {
		return LikeResult{}, err
	}
	theirName, err := firstName(ctx, tx, input.ProfileID)
	if err != nil {
		return LikeResult{}, err
	}
	if err := notify(ctx, tx, userID, domain.NotifyMatch, "You have a new match",
		fmt.Sprintf("You and %s liked each other.", or(theirName, "someone")),
		input.ProfileID); err != nil {
		return LikeResult{}, err
	}
	if err := notify(ctx, tx, input.ProfileID, domain.NotifyMatch, "You have a new match",
		fmt.Sprintf("You and %s liked each other.", or(myName, "someone")),
		userID); err != nil {
		return LikeResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return LikeResult{}, err
	}
	return LikeResult{Matched: true, Match: &match}, nil
}

// createMatch writes the pair row and consumes the two likes that produced it.
// The likes are deleted because they have been answered — leaving them would
// show a pending like from someone you are already matched with.
func createMatch(ctx context.Context, tx pgx.Tx, viewer, other string) (domain.MatchRecord, error) {
	// The table requires user_a < user_b, so a pair can only be stored one way.
	low, high := viewer, other
	if low > high {
		low, high = high, low
	}

	var id string
	var createdAt time.Time
	// The no-op update is what makes `returning` fire on a conflict: a plain
	// `do nothing` returns no row, and two people tapping at once would leave
	// one of them with no match to show.
	if err := tx.QueryRow(ctx, `
		insert into matches (id, user_a, user_b) values ($1, $2, $3)
		on conflict (user_a, user_b) do update set user_a = excluded.user_a
		returning id::text, created_at
	`, uuid.New(), low, high).Scan(&id, &createdAt); err != nil {
		return domain.MatchRecord{}, fmt.Errorf("create match: %w", err)
	}
	if _, err := chatlog.EnsureMatch(ctx, tx, viewer, other); err != nil {
		return domain.MatchRecord{}, err
	}

	if _, err := tx.Exec(ctx, `
		delete from likes
		where (from_user_id = $1 and to_user_id = $2)
		   or (from_user_id = $2 and to_user_id = $1)
	`, viewer, other); err != nil {
		return domain.MatchRecord{}, err
	}

	return domain.MatchRecord{
		ID: id, ProfileID: other, CreatedAt: createdAt, IsNew: true,
	}, nil
}

// underLikeLimit caps how many people a free account can like in a day.
//
// Only new likes count. Re-liking someone you already liked updates a row
// rather than adding one, and should not spend today's allowance.
func (s *Service) underLikeLimit(ctx context.Context, userID string) error {
	var premium bool
	var today int
	if err := s.pool.QueryRow(ctx, `
		select u.is_premium,
		       (select count(*) from likes
		        where from_user_id = $1 and created_at > now() - interval '1 day')
		from users u where u.id = $1
	`, userID).Scan(&premium, &today); err != nil {
		return translateFK(err)
	}
	if !premium && today >= MaxLikesPerDayFree {
		return ErrTooMany
	}
	return nil
}

// Likes lists one direction. Priority likes sort first: that is what someone
// paid for, and burying them under newer ordinary likes would defeat it.
func (s *Service) Likes(ctx context.Context, userID string, incoming bool) ([]domain.ProfileLike, error) {
	column := "to_user_id"
	if !incoming {
		column = "from_user_id"
	}
	// The column name is chosen here from two literals, never from input.
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		select id::text, from_user_id::text, to_user_id::text, kind, label, priority, created_at
		from likes where %s = $1
		order by priority desc, created_at desc
	`, column), userID)
	if err != nil {
		return nil, fmt.Errorf("load likes: %w", err)
	}
	defer rows.Close()

	out := []domain.ProfileLike{}
	for rows.Next() {
		var like domain.ProfileLike
		if err := rows.Scan(&like.ID, &like.FromProfileID, &like.ToProfileID,
			&like.Kind, &like.Label, &like.Priority, &like.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, like)
	}
	return out, rows.Err()
}

// RemoveLike dismisses a like from either side: the sender withdrawing it or
// the recipient clearing it. Scoping the delete to both columns is the
// authorization check — a third party's id matches neither.
func (s *Service) RemoveLike(ctx context.Context, userID, likeID string) error {
	tag, err := s.pool.Exec(ctx, `
		delete from likes
		where id = $1 and (from_user_id = $2 or to_user_id = $2)
	`, likeID, userID)
	if err != nil {
		return translateFK(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoSuchLike
	}
	return nil
}

// ---------------------------------------------------------------------------
// Matches
// ---------------------------------------------------------------------------

// Matches lists the viewer's matches, each told from their side.
//
// The message preview and unread count stay empty until the messaging phase.
// They are real fields of the clients' MatchRecord, so they are present and
// zero rather than absent, and the screens degrade to "matched, no chat yet"
// instead of failing to render.
func (s *Service) Matches(ctx context.Context, userID string) ([]domain.MatchRecord, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text,
		       case when user_a = $1 then user_b::text else user_a::text end as other_id,
		       created_at
		from matches
		where user_a = $1 or user_b = $1
		order by created_at desc
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("load matches: %w", err)
	}
	defer rows.Close()

	out := []domain.MatchRecord{}
	for rows.Next() {
		var m domain.MatchRecord
		if err := rows.Scan(&m.ID, &m.ProfileID, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.IsNew = time.Since(m.CreatedAt) < newMatchWindow
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

func (s *Service) Notifications(ctx context.Context, userID string) ([]domain.AppNotification, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, kind, title, body,
		       coalesce(profile_id::text, ''), read_at is not null, created_at
		from notifications where user_id = $1
		order by created_at desc limit 100
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("load notifications: %w", err)
	}
	defer rows.Close()

	out := []domain.AppNotification{}
	for rows.Next() {
		var n domain.AppNotification
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body,
			&n.ProfileID, &n.Read, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkRead is idempotent, and keeps the first read time rather than the
// latest: "when did they first see this" is the useful question.
func (s *Service) MarkRead(ctx context.Context, userID, id string) error {
	_, err := s.pool.Exec(ctx, `
		update notifications set read_at = now()
		where id = $1 and user_id = $2 and read_at is null
	`, id, userID)
	return translateFK(err)
}

func notify(
	ctx context.Context, tx pgx.Tx, userID string,
	kind domain.NotificationKind, title, body, aboutID string,
) error {
	// Nil rather than "" for the foreign key: an empty string is not a uuid,
	// and this column is null when the notice is not about a person.
	var about any
	if aboutID != "" {
		about = aboutID
	}
	if _, err := tx.Exec(ctx, `
		insert into notifications (id, user_id, kind, title, body, profile_id)
		values ($1, $2, $3, $4, $5, $6)
	`, uuid.New(), userID, kind, title, body, about); err != nil {
		return fmt.Errorf("write notification: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// firstName is empty for an account that has not onboarded, which the callers
// paper over rather than treat as an error — a like from someone mid-signup is
// still a like.
func firstName(ctx context.Context, tx pgx.Tx, userID string) (string, error) {
	var name string
	err := tx.QueryRow(ctx,
		`select coalesce((select first_name from profiles where user_id = $1), '')`,
		userID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return name, err
}

func or(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// translateFK turns "that id is not a real user" into something a handler can
// answer with a 404 rather than a 500. These ids come from URLs and request
// bodies, so a stale link is an ordinary way to arrive here.
func translateFK(err error) error {
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
