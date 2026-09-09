package chat

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/chatlog"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

type Enqueue func(context.Context, pgx.Tx, string, string, int64) error
type Service struct {
	pool         *pgxpool.Pool
	users        *auth.Service
	origins      []string
	hub          *Hub
	enqueue      Enqueue
	sendLimit    *httpx.Limiter
	requestLimit *httpx.Limiter
}

func New(pool *pgxpool.Pool, users *auth.Service, origins []string, enqueue Enqueue) *Service {
	s := &Service{pool: pool, users: users, origins: origins, enqueue: enqueue, sendLimit: httpx.NewLimiter(60, time.Minute), requestLimit: httpx.NewLimiter(20, 24*time.Hour)}
	s.hub = NewHub(s)
	return s
}
func (s *Service) Run(ctx context.Context) { s.hub.Run(ctx) }
func (s *Service) Close()                  { s.hub.Close() }

type thread struct {
	ID, A, B, Origin, State, Requester string
	Last                               int64
	Revealed                           *time.Time
}

func (c thread) peer(user string) string {
	if user == c.A {
		return c.B
	}
	return c.A
}

// All writes (including block and unmatch) serialize on the same canonical pair.
func (s *Service) access(ctx context.Context, tx pgx.Tx, user, id string, lock bool) (thread, error) {
	var c thread
	err := tx.QueryRow(ctx, `select id::text,user_a::text,user_b::text,origin,state,coalesce(requested_by::text,''),last_seq,revealed_at from conversations where id=$1 and ($2=user_a or $2=user_b)`, id, user).Scan(&c.ID, &c.A, &c.B, &c.Origin, &c.State, &c.Requester, &c.Last, &c.Revealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, unavailable
	}
	if err != nil {
		return c, err
	}
	if lock {
		if err = chatlog.LockPair(ctx, tx, c.A, c.B); err != nil {
			return c, err
		}
		err = tx.QueryRow(ctx, `select state,last_seq,revealed_at from conversations where id=$1 for update`, id).Scan(&c.State, &c.Last, &c.Revealed)
		if err != nil {
			return c, err
		}
	}
	var allowed bool
	err = tx.QueryRow(ctx, `select
  (select count(*)=2 from users where id in ($1,$2) and status='active')
  and not exists(select 1 from blocks where (user_id=$1 and blocked_id=$2) or (user_id=$2 and blocked_id=$1))
  and ($3 <> 'match' or exists(select 1 from matches where user_a=$1 and user_b=$2))`, c.A, c.B, c.Origin).Scan(&allowed)
	if err != nil {
		return c, err
	}
	if !allowed || c.State == "closed" || c.State == "declined" {
		return c, unavailable
	}
	return c, nil
}

func (s *Service) Bootstrap(ctx context.Context, user string, offset int) (Snapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback(ctx)
	out := Snapshot{Conversations: []Conversation{}}
	if err = tx.QueryRow(ctx, `select event_seq from users where id=$1 and status='active'`, user).Scan(&out.Cursor); err != nil {
		return out, httpx.Unauthorized("Sign in again.")
	}
	rows, err := tx.Query(ctx, `select c.id::text,c.origin,c.state,
  case when c.origin='anonymous' and c.revealed_at is null then '' else other.user_id::text end,
  case when c.origin='anonymous' and c.revealed_at is null then 'Anonymous connection' else coalesce(p.first_name,'Velora member') end,
  me.participant_id::text,c.last_seq,me.last_read_seq,other.last_read_seq,other.last_delivered_seq,
  (select count(*) from messages m where m.conversation_id=c.id and m.seq>me.last_read_seq and m.sender_id<>$1 and m.deleted_at is null),
  coalesce((select case when deleted_at is null then body else 'Message deleted' end from messages where conversation_id=c.id order by seq desc limit 1),''),
  coalesce(c.last_message_at,c.created_at),me.muted,me.archived,
  c.state='open' or (c.state='pending' and c.requested_by=$1 and c.last_seq=0),
  c.state='pending' and c.requested_by<>$1,
  c.origin='anonymous' and c.revealed_at is null,
  exists(select 1 from reveal_consents rc where rc.conversation_id=c.id and rc.user_id=$1)
 from conversation_participants me join conversations c on c.id=me.conversation_id
 join conversation_participants other on other.conversation_id=c.id and other.user_id<>me.user_id
 join users peer on peer.id=other.user_id and peer.status='active'
 left join profiles p on p.user_id=other.user_id
 where me.user_id=$1 and c.state in ('open','pending')
 and not exists(select 1 from blocks where (user_id=$1 and blocked_id=other.user_id) or (user_id=other.user_id and blocked_id=$1))
 and (c.origin<>'match' or exists(select 1 from matches m where m.user_a=c.user_a and m.user_b=c.user_b))
 order by coalesce(c.last_message_at,c.created_at) desc,c.id desc limit 101 offset $2`, user, offset)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var c Conversation
		err = rows.Scan(&c.ID, &c.Origin, &c.State, &c.ProfileID, &c.Name, &c.ParticipantID, &c.LastSeq, &c.LastReadSeq, &c.PeerReadSeq, &c.PeerDeliveredSeq, &c.UnreadCount, &c.Preview, &c.UpdatedAt, &c.Muted, &c.Archived, &c.CanSend, &c.CanAccept, &c.IdentityHidden, &c.RevealConsented)
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Conversations = append(out.Conversations, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Conversations) > 100 {
		out.Conversations = out.Conversations[:100]
		out.NextOffset = offset + 100
	}
	// The app badge covers the whole inbox, not just its currently loaded page.
	err = tx.QueryRow(ctx, `select count(*) from messages m
 join conversations c on c.id=m.conversation_id
 join conversation_participants me on me.conversation_id=c.id and me.user_id=$1
 join users peer on peer.id=m.sender_id and peer.status='active'
 where m.sender_id<>$1 and m.seq>me.last_read_seq and m.deleted_at is null and c.state in ('open','pending')
 and not exists(select 1 from blocks where (user_id=$1 and blocked_id=m.sender_id) or (user_id=m.sender_id and blocked_id=$1))
 and (c.origin<>'match' or exists(select 1 from matches where user_a=c.user_a and user_b=c.user_b))`, user).Scan(&out.UnreadCount)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (s *Service) Open(ctx context.Context, user, peer, origin string, input SendInput) (string, error) {
	if user == peer {
		return "", httpx.BadRequest("Choose another person.")
	}
	if origin != "match" && origin != "request" {
		return "", httpx.BadRequest("Choose a match or message request.")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	a, b := chatlog.Pair(user, peer)
	if err = chatlog.LockPair(ctx, tx, a, b); err != nil {
		return "", err
	}
	var allowed bool
	err = tx.QueryRow(ctx, `select (select count(*)=2 from users u join profiles p on p.user_id=u.id where u.id in ($1,$2) and u.status='active' and p.hidden=false)
 and not exists(select 1 from blocks where (user_id=$1 and blocked_id=$2) or (user_id=$2 and blocked_id=$1))`, a, b).Scan(&allowed)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", unavailable
	}
	var id string
	if origin == "match" {
		if err = tx.QueryRow(ctx, `select exists(select 1 from matches where user_a=$1 and user_b=$2)`, a, b).Scan(&allowed); err != nil {
			return "", err
		}
		if !allowed {
			return "", forbidden
		}
		id, err = chatlog.EnsureMatch(ctx, tx, a, b)
		if err != nil {
			return "", err
		}
	} else {
		// Existing requests are stable across retries; declined requests cannot be spammed.
		err = tx.QueryRow(ctx, `select id::text from conversations where user_a=$1 and user_b=$2 and origin='request'`, a, b).Scan(&id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		if id == "" {
			if ok, _ := s.requestLimit.Allow(user); !ok {
				return "", httpx.TooManyRequests("Try another introduction tomorrow.")
			}
			id = uuid.NewString()
			if _, err = tx.Exec(ctx, `insert into conversations(id,user_a,user_b,origin,state,requested_by) values($1,$2,$3,'request','pending',$4)`, id, a, b, user); err != nil {
				return "", err
			}
			if _, err = tx.Exec(ctx, `insert into conversation_participants(conversation_id,user_id) values($1,$2),($1,$3)`, id, a, b); err != nil {
				return "", err
			}
		}
		c, e := s.access(ctx, tx, user, id, true)
		if e != nil {
			return "", e
		}
		if _, err = s.insert(ctx, tx, c, user, input); err != nil {
			return "", err
		}
	}
	return id, tx.Commit(ctx)
}

func validateMessage(in SendInput) (SendInput, error) {
	in.Body = strings.TrimSpace(in.Body)
	if _, err := uuid.Parse(in.ClientMessageID); err != nil {
		return in, httpx.BadRequest("clientMessageId must be a UUID.")
	}
	if in.Kind == "" {
		in.Kind = "text"
	}
	if in.Kind != "text" || !utf8.ValidString(in.Body) || in.Body == "" || utf8.RuneCountInString(in.Body) > 4000 || len(in.Body) > 16000 {
		return in, httpx.BadRequest("Send text between 1 and 4,000 characters.")
	}
	if in.ReplyToID != nil {
		if _, err := uuid.Parse(*in.ReplyToID); err != nil {
			return in, httpx.BadRequest("Invalid reply target.")
		}
	}
	return in, nil
}
func (s *Service) Send(ctx context.Context, user, id string, in SendInput) (Message, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback(ctx)
	c, err := s.access(ctx, tx, user, id, true)
	if err != nil {
		return Message{}, err
	}
	m, err := s.insert(ctx, tx, c, user, in)
	if err != nil {
		return m, err
	}
	return m, tx.Commit(ctx)
}
func (s *Service) insert(ctx context.Context, tx pgx.Tx, c thread, user string, in SendInput) (Message, error) {
	in, err := validateMessage(in)
	if err != nil {
		return Message{}, err
	}
	var existing Message
	existing, err = readMessage(ctx, tx, user, `m.conversation_id=$2 and m.sender_id=$1 and m.client_message_id=$3`, c.ID, in.ClientMessageID)
	if err == nil {
		sameReply := (existing.ReplyToID == nil && in.ReplyToID == nil) || (existing.ReplyToID != nil && in.ReplyToID != nil && *existing.ReplyToID == *in.ReplyToID)
		if existing.Body != in.Body || !sameReply {
			return Message{}, httpx.BadRequest("This clientMessageId already identifies a different message.")
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Message{}, err
	}
	if c.State != "open" && (c.State != "pending" || c.Requester != user || c.Last != 0) {
		return Message{}, forbidden
	}
	if ok, _ := s.sendLimit.Allow(user); !ok {
		return Message{}, httpx.TooManyRequests("Please slow down before sending another message.")
	}
	if in.ReplyToID != nil {
		var ok bool
		if err = tx.QueryRow(ctx, `select exists(select 1 from messages where id=$1 and conversation_id=$2 and deleted_at is null)`, *in.ReplyToID, c.ID).Scan(&ok); err != nil {
			return Message{}, err
		}
		if !ok {
			return Message{}, httpx.BadRequest("Reply to a message in this conversation.")
		}
	}
	var seq int64
	if err = tx.QueryRow(ctx, `update conversations set last_seq=last_seq+1,last_message_at=now() where id=$1 returning last_seq`, c.ID).Scan(&seq); err != nil {
		return Message{}, err
	}
	id := uuid.NewString()
	if _, err = tx.Exec(ctx, `insert into messages(id,conversation_id,seq,sender_id,body,kind,client_message_id,reply_to_id) values($1,$2,$3,$4,$5,'text',$6,$7)`, id, c.ID, seq, user, in.Body, in.ClientMessageID, in.ReplyToID); err != nil {
		return Message{}, err
	}
	if _, err = tx.Exec(ctx, `update conversation_participants set archived=false where conversation_id=$1`, c.ID); err != nil {
		return Message{}, err
	}
	if err = chatlog.Emit(ctx, tx, c.A, c.B, c.ID, "message.new", map[string]string{"messageId": id, "seq": strconv.FormatInt(seq, 10)}); err != nil {
		return Message{}, err
	}
	// User rows are now locked by Emit; do not race an account suspension.
	var active bool
	if err = tx.QueryRow(ctx, `select count(*)=2 from users where id in($1,$2) and status='active'`, c.A, c.B).Scan(&active); err != nil {
		return Message{}, err
	}
	if !active {
		return Message{}, forbidden
	}
	if s.enqueue != nil {
		if err = s.enqueue(ctx, tx, id, c.peer(user), seq); err != nil {
			return Message{}, err
		}
	}
	return readMessage(ctx, tx, user, `m.id=$2`, id)
}

const messageColumns = `m.id::text,m.conversation_id::text,m.seq,m.client_message_id,p.participant_id::text,m.sender_id=$1,case when m.deleted_at is null then m.body else '' end,m.kind,m.reply_to_id::text,m.created_at,m.deleted_at`

func scanMessage(row pgx.Row) (Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.ConversationID, &m.Seq, &m.ClientMessageID, &m.SenderID, &m.Mine, &m.Body, &m.Kind, &m.ReplyToID, &m.CreatedAt, &m.DeletedAt)
	return m, err
}
func readMessage(ctx context.Context, tx pgx.Tx, user, where string, args ...any) (Message, error) {
	params := append([]any{user}, args...)
	return scanMessage(tx.QueryRow(ctx, `select `+messageColumns+` from messages m join conversation_participants p on p.conversation_id=m.conversation_id and p.user_id=m.sender_id where `+where, params...))
}
func (s *Service) History(ctx context.Context, user, id string, after, before int64, limit int) (History, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return History{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = s.access(ctx, tx, user, id, false); err != nil {
		return History{}, err
	}
	order := "desc"
	if after >= 0 {
		order = "asc"
	}
	rows, err := tx.Query(ctx, `select `+messageColumns+` from messages m join conversation_participants p on p.conversation_id=m.conversation_id and p.user_id=m.sender_id where m.conversation_id=$2 and ($3::bigint<0 or m.seq>$3) and ($4::bigint=0 or m.seq<$4) order by m.seq `+order+` limit $5`, user, id, after, before, limit+1)
	if err != nil {
		return History{}, err
	}
	out := History{Messages: []Message{}}
	for rows.Next() {
		m, e := scanMessage(rows)
		if e != nil {
			rows.Close()
			return out, e
		}
		out.Messages = append(out.Messages, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Messages) > limit {
		out.Messages = out.Messages[:limit]
		out.HasMore = true
	}
	if after < 0 {
		for i, j := 0, len(out.Messages)-1; i < j; i, j = i+1, j-1 {
			out.Messages[i], out.Messages[j] = out.Messages[j], out.Messages[i]
		}
	}
	return out, tx.Commit(ctx)
}

func (s *Service) Receipt(ctx context.Context, user, id string, seq int64, read bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	c, err := s.access(ctx, tx, user, id, true)
	if err != nil {
		return err
	}
	if c.State != "open" {
		return nil
	}
	if seq < 0 || seq > c.Last {
		return httpx.BadRequest("Receipt is beyond available history.")
	}
	query := `update conversation_participants set last_delivered_seq=greatest(last_delivered_seq,$3) where conversation_id=$1 and user_id=$2 and last_delivered_seq<$3`
	kind := "receipt.delivered"
	if read {
		query = `update conversation_participants set last_read_seq=greatest(last_read_seq,$3),last_delivered_seq=greatest(last_delivered_seq,$3) where conversation_id=$1 and user_id=$2 and last_read_seq<$3`
		kind = "receipt.read"
	}
	tag, err := tx.Exec(ctx, query, id, user, seq)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		if err = chatlog.Emit(ctx, tx, c.A, c.B, id, kind, map[string]string{"seq": strconv.FormatInt(seq, 10)}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) Action(ctx context.Context, user, id, action string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	c, err := s.access(ctx, tx, user, id, true)
	if err != nil {
		return err
	}
	kind := "conversation.access"
	switch action {
	case "accept", "decline":
		if c.Origin != "request" || c.Requester == user {
			return forbidden
		}
		if c.State != "pending" {
			if c.State == "open" && action == "accept" {
				return nil
			}
			return forbidden
		}
		state := "open"
		if action == "decline" {
			state = "declined"
		}
		_, err = tx.Exec(ctx, `update conversations set state=$2 where id=$1`, id, state)
	case "reveal":
		if c.Origin != "anonymous" || c.State != "open" {
			return forbidden
		}
		if c.Revealed != nil {
			return nil
		}
		_, err = tx.Exec(ctx, `insert into reveal_consents(conversation_id,user_id) values($1,$2) on conflict do nothing`, id, user)
		if err == nil {
			tag, updateErr := tx.Exec(ctx, `update conversations set revealed_at=now() where id=$1 and (select count(*) from reveal_consents where conversation_id=$1)=2`, id)
			err = updateErr
			if tag.RowsAffected() > 0 {
				kind = "identity.revealed"
			}
		}
	case "mute", "unmute", "archive", "unarchive":
		column := "muted"
		value := action == "mute"
		if action == "archive" || action == "unarchive" {
			column = "archived"
			value = action == "archive"
		}
		_, err = tx.Exec(ctx, `update conversation_participants set `+column+`=$3 where conversation_id=$1 and user_id=$2`, id, user, value)
	default:
		return httpx.BadRequest("Unknown conversation action.")
	}
	if err != nil {
		return err
	}
	if err = chatlog.Emit(ctx, tx, c.A, c.B, id, kind, map[string]string{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) Events(ctx context.Context, user string, after int64) (EventPage, error) {
	out := EventPage{Events: []Event{}, Cursor: after}
	var current int64
	if err := s.pool.QueryRow(ctx, `select event_seq from users where id=$1 and status='active'`, user).Scan(&current); err != nil {
		return out, httpx.Unauthorized("Sign in again.")
	}
	if after > current {
		return out, httpx.BadRequest("Event cursor is ahead of this account.")
	}
	rows, err := s.pool.Query(ctx, `select seq,conversation_id::text,kind from user_events where user_id=$1 and seq>$2 order by seq limit 101`, user, after)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.Cursor, &e.ConversationID, &e.Kind); err != nil {
			return out, err
		}
		out.Events = append(out.Events, e)
	}
	if len(out.Events) > 100 {
		out.Events = out.Events[:100]
		out.HasMore = true
	}
	if len(out.Events) > 0 {
		out.Cursor = out.Events[len(out.Events)-1].Cursor
	}
	return out, rows.Err()
}
