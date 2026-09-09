// Package chatlog holds transaction helpers shared by chat, matching and safety.
// Lock order: canonical pair, conversation rows, then user rows in UUID order.
package chatlog

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func Pair(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}
func LockPair(ctx context.Context, tx pgx.Tx, a, b string) error {
	a, b = Pair(a, b)
	_, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1 || $2)::bigint)`, a, b)
	return err
}

// Emit serializes each user's event allocation and commits the notification with
// its rows. Payloads contain IDs/positions only, never private message content.
func Emit(ctx context.Context, tx pgx.Tx, a, b, cid, kind string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ids := []string{a, b}
	sort.Strings(ids)
	for _, id := range ids {
		var seq int64
		if err = tx.QueryRow(ctx, `update users set event_seq=event_seq+1 where id=$1 returning event_seq`, id).Scan(&seq); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `insert into user_events(user_id,seq,kind,conversation_id,payload) values($1,$2,$3,$4,$5)`, id, seq, kind, cid, data); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `select pg_notify('velora_chat',$1)`, ids[0]+","+ids[1])
	return err
}

func EnsureMatch(ctx context.Context, tx pgx.Tx, a, b string) (string, error) {
	a, b = Pair(a, b)
	var id string
	err := tx.QueryRow(ctx, `insert into conversations(id,user_a,user_b,origin,state) values($1,$2,$3,'match','open') on conflict(user_a,user_b,origin) do nothing returning id::text`, uuid.New(), a, b).Scan(&id)
	if err == pgx.ErrNoRows {
		err = tx.QueryRow(ctx, `select id::text from conversations where user_a=$1 and user_b=$2 and origin='match'`, a, b).Scan(&id)
		return id, err
	}
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `insert into conversation_participants(conversation_id,user_id) values($1,$2),($1,$3)`, id, a, b); err != nil {
		return "", err
	}
	return id, Emit(ctx, tx, a, b, id, "conversation.new", map[string]string{})
}

// Caller must hold the pair lock before changing the associated match/block.
func ClosePair(ctx context.Context, tx pgx.Tx, a, b string, datingOnly bool) error {
	a, b = Pair(a, b)
	rows, err := tx.Query(ctx, `select id::text from conversations where user_a=$1 and user_b=$2 and state in ('open','pending') and (not $3 or origin='match') order by id for update`, a, b, datingOnly)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.Exec(ctx, `update conversations set state='closed' where id=$1`, id); err != nil {
			return err
		}
		if err = Emit(ctx, tx, a, b, id, "conversation.access", map[string]string{"state": "closed"}); err != nil {
			return err
		}
	}
	return nil
}
