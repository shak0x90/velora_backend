package chat

import (
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shak0x90/velora_backend/internal/chatlog"
	"github.com/shak0x90/velora_backend/internal/httpx"
	"net/http"
)

// Joining the short-lived queue is explicit consent to a text-only pairing.
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
	var eligible bool
	err = tx.QueryRow(ctx, `select exists(select 1 from users u join profiles p on p.user_id=u.id where u.id=$1 and u.status='active' and p.hidden=false and p.birth_date<=current_date-interval '18 years')`, uid).Scan(&eligible)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !eligible {
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
	var peer string
	err = tx.QueryRow(ctx, `select q.user_id::text from chat_queue q
 join users u on u.id=q.user_id and u.status='active'
 join profiles p on p.user_id=q.user_id join profiles me on me.user_id=$1
 where q.user_id<>$1 and q.expires_at>now() and p.hidden=false and p.birth_date<=current_date-interval '18 years'
 and (cardinality(me.interested_in)=0 or p.gender=any(me.interested_in))
 and (cardinality(p.interested_in)=0 or me.gender=any(p.interested_in))
 and date_part('year',age(p.birth_date)) between me.min_age and me.max_age
 and date_part('year',age(me.birth_date)) between p.min_age and p.max_age
 and not exists(select 1 from blocks where (user_id=$1 and blocked_id=q.user_id) or (user_id=q.user_id and blocked_id=$1))
 and not exists(select 1 from conversations where user_a=least($1::uuid,q.user_id) and user_b=greatest($1::uuid,q.user_id))
 order by q.waiting_since limit 1`, uid).Scan(&peer)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = tx.Commit(ctx); err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.JSON(w, 200, map[string]bool{"waiting": true})
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	a, b := chatlog.Pair(uid, peer)
	if err = chatlog.LockPair(ctx, tx, a, b); err != nil {
		httpx.Error(w, r, err)
		return
	}
	// Recheck after the same lock used by block. Pair selection alone is not authorization.
	var blocked bool
	if err = tx.QueryRow(ctx, `select exists(select 1 from blocks where (user_id=$1 and blocked_id=$2) or (user_id=$2 and blocked_id=$1))`, a, b).Scan(&blocked); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if blocked {
		httpx.Error(w, r, unavailable)
		return
	}
	id = uuid.NewString()
	if _, err = tx.Exec(ctx, `insert into conversations(id,user_a,user_b,origin,state) values($1,$2,$3,'anonymous','open')`, id, a, b); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if _, err = tx.Exec(ctx, `insert into conversation_participants(conversation_id,user_id) values($1,$2),($1,$3)`, id, a, b); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if _, err = tx.Exec(ctx, `delete from chat_queue where user_id in($1,$2)`, a, b); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err = chatlog.Emit(ctx, tx, a, b, id, "conversation.new", map[string]string{}); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err = tx.Commit(ctx); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 200, map[string]string{"id": id})
}
func (s *Service) handleLeaveQueue(w http.ResponseWriter, r *http.Request) {
	_, err := s.pool.Exec(r.Context(), `delete from chat_queue where user_id=$1`, user(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 204, nil)
}
