package chat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shak0x90/velora_backend/internal/chatlog"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

// compatibleSQL is the mutual-preference test for blind chat, with "me" the
// person searching and "p" the candidate's profile: both adults and visible,
// each within the other's gender and age preferences, not blocked either way,
// and never paired before.
const compatibleSQL = `p.hidden=false and p.birth_date<=current_date-interval '18 years'
 and (cardinality(me.interested_in)=0 or p.gender=any(me.interested_in))
 and (cardinality(p.interested_in)=0 or me.gender=any(p.interested_in))
 and date_part('year',age(p.birth_date)) between me.min_age and me.max_age
 and date_part('year',age(me.birth_date)) between p.min_age and p.max_age
 and not exists(select 1 from blocks where (user_id=me.user_id and blocked_id=p.user_id) or (user_id=p.user_id and blocked_id=me.user_id))
 and not exists(select 1 from conversations where user_a=least(me.user_id,p.user_id) and user_b=greatest(me.user_id,p.user_id))`

// CompatibleSQL is exported for the blind-chat alerts, which pick recipients
// with the same rule (aliases: me = the searcher's profile, p = candidate's).
const CompatibleSQL = compatibleSQL

// BlindHooks connect blind chat to notifications without this package
// importing them. Both are optional.
type BlindHooks struct {
	// Looking runs (outside any transaction, at most every few minutes per
	// person) when someone searches and finds nobody, so compatible people
	// can be told someone is looking.
	Looking func(ctx context.Context, userID string)
	// HelloAnswered runs in the pairing transaction when someone is paired
	// with a blind hello, to tell its author their chat has started.
	HelloAnswered func(ctx context.Context, tx pgx.Tx, authorID, conversationID string) error
}

// WithBlindHooks sets the notification hooks for blind chat.
func (s *Service) WithBlindHooks(h BlindHooks) *Service {
	s.blind = h
	return s
}

const (
	helloMaxRunes = 500
	helloLifetime = 24 * time.Hour
)

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Service) eligible(ctx context.Context, q rowQuerier, uid string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `select exists(select 1 from users u join profiles p on p.user_id=u.id where u.id=$1 and u.status='active' and p.hidden=false and p.birth_date<=current_date-interval '18 years')`, uid).Scan(&ok)
	return ok, err
}

// Joining the short-lived queue is explicit consent to a text-only pairing.
// Instant blind chat: a search pairs with a compatible person searching right
// now; otherwise the caller waits in the queue.
func (s *Service) handleAnonymous(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := user(r)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `select pg_advisory_xact_lock(8716354201)`); err != nil {
		httpx.Error(w, r, err)
		return
	}
	ok, err := s.eligible(ctx, tx, uid)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !ok {
		httpx.Error(w, r, httpx.Forbidden("Complete an adult, visible profile before joining."))
		return
	}
	var id string
	err = tx.QueryRow(ctx, `select c.id::text from conversations c where c.origin='anonymous' and c.state='open' and c.revealed_at is null and $1 in(c.user_a,c.user_b) and not exists(select 1 from blocks where (user_id=c.user_a and blocked_id=c.user_b) or (user_id=c.user_b and blocked_id=c.user_a)) limit 1`, uid).Scan(&id)
	if err == nil {
		httpx.JSON(w, 200, map[string]string{"id": id})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, r, err)
		return
	}
	if _, err = tx.Exec(ctx, `delete from chat_queue where expires_at<now()`); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if _, err = tx.Exec(ctx, `insert into chat_queue(user_id,expires_at) values($1,now()+interval '30 seconds') on conflict(user_id) do update set expires_at=excluded.expires_at`, uid); err != nil {
		httpx.Error(w, r, err)
		return
	}

	// 1. Someone searching right now.
	var peer string
	err = tx.QueryRow(ctx, `select q.user_id::text from chat_queue q
 join users u on u.id=q.user_id and u.status='active'
 join profiles p on p.user_id=q.user_id join profiles me on me.user_id=$1
 where q.user_id<>$1 and q.expires_at>now() and `+compatibleSQL+`
 order by q.waiting_since limit 1`, uid).Scan(&peer)
	if err == nil {
		c, err := s.pairAnonymous(ctx, tx, uid, peer)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if err = tx.Commit(ctx); err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.JSON(w, 200, map[string]string{"id": c.ID})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, r, err)
		return
	}

	// 2. Nobody: wait, and let compatible people know someone is looking.
	// Hellos are no longer consumed here; they live on the board, where
	// people choose which to answer (hellos.go).
	if err = tx.Commit(ctx); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if s.blind.Looking != nil {
		if ok, _ := s.lookLimit.Allow(uid); ok {
			go s.blind.Looking(context.WithoutCancel(ctx), uid)
		}
	}
	httpx.JSON(w, 200, map[string]bool{"waiting": true})
}

// pairAnonymous opens a blind chat between uid and peer inside tx: the pair
// lock, a block recheck under it, the conversation, and both leaving the
// queue. Pair selection alone is not authorization.
func (s *Service) pairAnonymous(ctx context.Context, tx pgx.Tx, uid, peer string) (thread, error) {
	a, b := chatlog.Pair(uid, peer)
	if err := chatlog.LockPair(ctx, tx, a, b); err != nil {
		return thread{}, err
	}
	var blocked bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from blocks where (user_id=$1 and blocked_id=$2) or (user_id=$2 and blocked_id=$1))`, a, b).Scan(&blocked); err != nil {
		return thread{}, err
	}
	if blocked {
		return thread{}, unavailable
	}
	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `insert into conversations(id,user_a,user_b,origin,state) values($1,$2,$3,'anonymous','open')`, id, a, b); err != nil {
		return thread{}, err
	}
	if _, err := tx.Exec(ctx, `insert into conversation_participants(conversation_id,user_id) values($1,$2),($1,$3)`, id, a, b); err != nil {
		return thread{}, err
	}
	if _, err := tx.Exec(ctx, `delete from chat_queue where user_id in($1,$2)`, a, b); err != nil {
		return thread{}, err
	}
	if err := chatlog.Emit(ctx, tx, a, b, id, "conversation.new", map[string]string{}); err != nil {
		return thread{}, err
	}
	return thread{ID: id, A: a, B: b, Origin: "anonymous", State: "open"}, nil
}

func (s *Service) handleLeaveQueue(w http.ResponseWriter, r *http.Request) {
	_, err := s.pool.Exec(r.Context(), `delete from chat_queue where user_id=$1`, user(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 204, nil)
}

// handleLeaveHello saves (or replaces) the caller's blind hello, kept for a
// day: the opening line for whoever starts a compatible blind chat next.
func (s *Service) handleLeaveHello(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := user(r)
	var in struct {
		Body string `json:"body"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in.Body = strings.TrimSpace(in.Body)
	if in.Body == "" || !utf8.ValidString(in.Body) || utf8.RuneCountInString(in.Body) > helloMaxRunes {
		httpx.Error(w, r, httpx.BadRequest("Write a hello between 1 and 500 characters."))
		return
	}
	ok, err := s.eligible(ctx, s.pool, uid)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !ok {
		httpx.Error(w, r, httpx.Forbidden("Complete an adult, visible profile before joining."))
		return
	}
	if allowed, _ := s.requestLimit.Allow("hello:" + uid); !allowed {
		httpx.Error(w, r, httpx.TooManyRequests("You've changed your hello a lot today. Try again tomorrow."))
		return
	}
	// A new hello is a new card: fresh id, no replies yet. Chats that came
	// from the old one carry on.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `delete from blind_hellos where user_id=$1`, uid); err != nil {
		httpx.Error(w, r, err)
		return
	}
	var expires time.Time
	err = tx.QueryRow(ctx, `insert into blind_hellos(user_id,body,client_message_id,expires_at) values($1,$2,$3,now()+$4::interval)
 returning expires_at`, uid, in.Body, uuid.NewString(), helloLifetime.String()).Scan(&expires)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"body": in.Body, "expiresAt": expires})
}

func (s *Service) handleDeleteHello(w http.ResponseWriter, r *http.Request) {
	if _, err := s.pool.Exec(r.Context(), `delete from blind_hellos where user_id=$1`, user(r)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}
