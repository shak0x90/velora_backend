package chat

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shak0x90/velora_backend/internal/httpx"
)

// The hello board: blind hellos that matching people can see and reply to.
// A card shows the author's age, a rough distance and a few interests, never
// a name or photo. Each reply opens its own blind chat with the author, up
// to helloMaxReplies, so a hello is never wasted on one unanswered pairing.

const helloMaxReplies = 5

// Three different people's reports take a hello off the board for everyone
// (the literal 3 in the SQL here and in blindhour's status count).
var helloReportReasons = map[string]bool{"harassment": true, "spam": true, "underage": true, "other": true}

// BoardHello is one card on the board.
type BoardHello struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	Age       int       `json:"age"`
	// DistanceKm is rounded up to 5 km steps (5, 10, 15...) and omitted when
	// either person has no location: an anonymous author must not be
	// locatable from readings taken at a few spots.
	DistanceKm *int `json:"distanceKm,omitempty"`
	// Interests are up to three of the author's, shared ones first.
	Interests []string `json:"interests"`
	// Shared counts interests the author and viewer have in common.
	Shared int `json:"shared"`
}

// helloBoardSQL lists the compatible, open hellos for $1, newest first.
// $2 = nearby only (within the viewer's own max distance); $3 = reply cap.
const helloBoardSQL = `select h.id::text, h.body, h.created_at, h.expires_at,
  date_part('year', age(p.birth_date))::int,
  case when me.lat is null or me.lng is null or p.lat is null or p.lng is null then null
       else (greatest(1, ceil(earth_distance(ll_to_earth(me.lat, me.lng), ll_to_earth(p.lat, p.lng)) / 5000)) * 5)::int end,
  array(select i from unnest(p.interests) i order by (i = any(me.interests)) desc, i limit 3),
  cardinality(array(select i from unnest(p.interests) i where i = any(me.interests)))
 from blind_hellos h
 join users u on u.id = h.user_id and u.status = 'active'
 join profiles p on p.user_id = h.user_id
 join profiles me on me.user_id = $1
 where h.user_id <> $1 and h.expires_at > now() and h.reply_count < $3
   and not exists (select 1 from blind_hello_replies r where r.hello_id = h.id and r.user_id = $1)
   and h.report_count < 3
   and not exists (select 1 from blind_hello_reports x where x.hello_id = h.id and x.user_id = $1)
   and (not $2::bool or (me.lat is not null and p.lat is not null
        and earth_distance(ll_to_earth(me.lat, me.lng), ll_to_earth(p.lat, p.lng)) <= me.max_distance_km * 1000))
   and ` + compatibleSQL + `
 order by h.created_at desc limit 50`

func (s *Service) handleHelloBoard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nearby := r.URL.Query().Get("scope") == "nearby"
	rows, err := s.pool.Query(ctx, helloBoardSQL, user(r), nearby, helloMaxReplies)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	defer rows.Close()
	out := []BoardHello{}
	for rows.Next() {
		var h BoardHello
		if err = rows.Scan(&h.ID, &h.Body, &h.CreatedAt, &h.ExpiresAt, &h.Age, &h.DistanceKm, &h.Interests, &h.Shared); err != nil {
			httpx.Error(w, r, err)
			return
		}
		out = append(out, h)
	}
	if err = rows.Err(); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"hellos": out})
}

// handleHelloReply answers a hello: a new blind chat with its author, the
// hello as the first message (from the author) and the reply as the second.
func (s *Service) handleHelloReply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := user(r)
	helloID := r.PathValue("id")
	if _, err := uuid.Parse(helloID); err != nil {
		httpx.Error(w, r, httpx.NotFound("This hello is no longer open."))
		return
	}
	var in struct {
		Body            string `json:"body"`
		ClientMessageID string `json:"clientMessageId"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in.Body = strings.TrimSpace(in.Body)
	if in.Body == "" || !utf8.ValidString(in.Body) || utf8.RuneCountInString(in.Body) > helloMaxRunes {
		httpx.Error(w, r, httpx.BadRequest("Write a reply between 1 and 500 characters."))
		return
	}
	if _, err := uuid.Parse(in.ClientMessageID); err != nil {
		in.ClientMessageID = uuid.NewString()
	}
	if allowed, _ := s.requestLimit.Allow("helloreply:" + uid); !allowed {
		httpx.Error(w, r, httpx.TooManyRequests("You've replied to a lot of hellos. Try again a little later."))
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	ok, err := s.eligible(ctx, tx, uid)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if !ok {
		httpx.Error(w, r, httpx.Forbidden("Complete an adult, visible profile before joining."))
		return
	}
	// Same rule as the board, re-checked under a row lock so two replies
	// racing for the last slot cannot both win.
	var author, body string
	err = tx.QueryRow(ctx, `select h.user_id::text, h.body from blind_hellos h
 join users u on u.id = h.user_id and u.status = 'active'
 join profiles p on p.user_id = h.user_id
 join profiles me on me.user_id = $1
 where h.id = $2 and h.user_id <> $1 and h.expires_at > now() and h.reply_count < $3
   and not exists (select 1 from blind_hello_replies r where r.hello_id = h.id and r.user_id = $1)
   and h.report_count < 3
   and not exists (select 1 from blind_hello_reports x where x.hello_id = h.id and x.user_id = $1)
   and `+compatibleSQL+`
 for update of h`, uid, helloID, helloMaxReplies).Scan(&author, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, r, httpx.NotFound("This hello is no longer open."))
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	c, err := s.pairAnonymous(ctx, tx, uid, author)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if _, err = s.insert(ctx, tx, c, author, SendInput{ClientMessageID: uuid.NewString(), Body: body, Kind: "text"}); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if _, err = s.insert(ctx, tx, c, uid, SendInput{ClientMessageID: in.ClientMessageID, Body: in.Body, Kind: "text"}); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if _, err = tx.Exec(ctx, `insert into blind_hello_replies (hello_id, user_id, conversation_id) values ($1, $2, $3)`, helloID, uid, c.ID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if _, err = tx.Exec(ctx, `update blind_hellos set reply_count = reply_count + 1 where id = $1`, helloID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if s.blind.HelloAnswered != nil {
		if err = s.blind.HelloAnswered(ctx, tx, author, c.ID); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		httpx.Error(w, r, err)
		return
	}
	s.touch(ctx, uid)
	httpx.JSON(w, http.StatusOK, map[string]string{"id": c.ID})
}

// handleHelloReport reports a hello on the board. The reporter stops seeing
// it at once; it joins the moderation queue as a report about its author
// (with the text, since a hello lapses after a day); and three different
// reporters take it off the board for everyone.
func (s *Service) handleHelloReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := user(r)
	helloID := r.PathValue("id")
	if _, err := uuid.Parse(helloID); err != nil {
		httpx.Error(w, r, httpx.NotFound("This hello is no longer open."))
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if in.Reason == "" {
		in.Reason = "other"
	}
	if !helloReportReasons[in.Reason] {
		httpx.Error(w, r, httpx.BadRequest("Choose harassment, spam, underage or other."))
		return
	}
	if allowed, _ := s.requestLimit.Allow("helloreport:" + uid); !allowed {
		httpx.Error(w, r, httpx.TooManyRequests("You've sent a lot of reports. Try again a little later."))
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	var author, body string
	err = tx.QueryRow(ctx, `select user_id::text, body from blind_hellos where id = $1 and user_id <> $2 for update`, helloID, uid).Scan(&author, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, r, httpx.NotFound("This hello is no longer open."))
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	tag, err := tx.Exec(ctx, `insert into blind_hello_reports (hello_id, user_id) values ($1, $2) on conflict do nothing`, helloID, uid)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	if tag.RowsAffected() == 1 {
		if _, err = tx.Exec(ctx, `update blind_hellos set report_count = report_count + 1 where id = $1`, helloID); err != nil {
			httpx.Error(w, r, err)
			return
		}
		// Same shape and de-duplication as safety.Report.
		if _, err = tx.Exec(ctx, `
			insert into reports (id, reporter_id, subject_id, reason, detail, context)
			values ($1, $2, $3, $4, $5, 'message')
			on conflict (reporter_id, subject_id) where status in ('open', 'reviewing')
			do update set reason = excluded.reason, detail = excluded.detail, context = excluded.context, created_at = now()
		`, uuid.New(), uid, author, in.Reason, "Blind hello: "+body); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}
