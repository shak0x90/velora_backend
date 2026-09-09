package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/chatlog"
	"github.com/shak0x90/velora_backend/internal/chatpush"
	"github.com/shak0x90/velora_backend/internal/db"
	"github.com/shak0x90/velora_backend/internal/httpx"
	"github.com/shak0x90/velora_backend/internal/safety"
)

var migrated sync.Once
var migrationErr error
var signingKey = []byte("local-chat-integration-signing-key-32-bytes")

type fixture struct {
	s                       *Service
	pool                    *pgxpool.Pool
	users                   *auth.Service
	a, b, third, cid, match string
	ctx                     context.Context
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("CHAT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CHAT_TEST_DATABASE_URL to an isolated migrated PostgreSQL database")
	}
	if !strings.Contains(dsn, "velora_chat_test") {
		t.Fatal("integration database name must contain velora_chat_test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	migrated.Do(func() {
		migrationErr = db.Migrate(dsn)
		if migrationErr == nil {
			migrationErr = chatpush.Migrate(ctx, pool)
		}
	})
	if migrationErr != nil {
		t.Fatal(migrationErr)
	}
	users := auth.New(pool, auth.Config{SigningKey: signingKey, AccessTokenTTL: time.Minute})
	f := &fixture{pool: pool, users: users, ctx: ctx, a: uuid.NewString(), b: uuid.NewString(), third: uuid.NewString(), match: uuid.NewString()}
	f.s = New(pool, users, []string{"http://chat.test"}, nil)
	for _, id := range []string{f.a, f.b, f.third} {
		if _, err = pool.Exec(ctx, `insert into users(id,status) values($1,'active')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, `insert into profiles(user_id,first_name,birth_date,gender) values($1,'Chat tester','1996-01-01','woman')`, id); err != nil {
			t.Fatal(err)
		}
	}
	a, b := chatlog.Pair(f.a, f.b)
	if _, err = pool.Exec(ctx, `insert into matches(id,user_a,user_b) values($1,$2,$3)`, f.match, a, b); err != nil {
		t.Fatal(err)
	}
	f.cid, err = f.s.Open(ctx, f.a, f.b, "match", SendInput{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.s.Close()
		pool.Exec(ctx, `delete from users where id=any($1::uuid[])`, []string{f.a, f.b, f.third})
		pool.Close()
	})
	return f
}
func input(body string) SendInput {
	return SendInput{ClientMessageID: uuid.NewString(), Body: body, Kind: "text"}
}
func mustSend(t *testing.T, f *fixture, user, cid string, in SendInput) Message {
	t.Helper()
	m, err := f.s.Send(f.ctx, user, cid, in)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestValidation(t *testing.T) {
	for _, in := range []SendInput{{Body: "hello", ClientMessageID: "bad"}, {Body: " ", ClientMessageID: uuid.NewString()}, {Body: strings.Repeat("界", 4001), ClientMessageID: uuid.NewString()}, {Body: "hi", Kind: "image", ClientMessageID: uuid.NewString()}} {
		if _, err := validateMessage(in); err == nil {
			t.Errorf("accepted invalid message: %+v", in)
		}
	}
	good := input("  Hello  ")
	got, err := validateMessage(good)
	if err != nil || got.Body != "Hello" {
		t.Fatal(got, err)
	}
	data, err := json.Marshal(Message{Seq: 9007199254740993})
	if err != nil || !strings.Contains(string(data), `"seq":"9007199254740993"`) {
		t.Fatal(string(data), err)
	}
}

func TestConcurrentRetryAndReceipts(t *testing.T) {
	f := setup(t)
	in := input("One message despite a lost response")
	const n = 12
	out := make(chan Message, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m, err := f.s.Send(f.ctx, f.a, f.cid, in); out <- m; errs <- err }()
	}
	wg.Wait()
	close(out)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for m := range out {
		if id != "" && m.ID != id {
			t.Fatal("retry duplicated message")
		}
		id = m.ID
		if m.Seq != 1 {
			t.Fatal(m.Seq)
		}
	}
	changed := in
	changed.Body = "different"
	if _, err := f.s.Send(f.ctx, f.a, f.cid, changed); err == nil {
		t.Fatal("reused ID overwrote content")
	}
	if _, err := f.s.Send(f.ctx, f.third, f.cid, input("intruder")); err == nil {
		t.Fatal("nonmember sent")
	}
	if _, err := f.s.History(f.ctx, f.third, f.cid, -1, 0, 50); err == nil {
		t.Fatal("nonmember read")
	}
	snap, err := f.s.Bootstrap(f.ctx, f.b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Conversations[0].UnreadCount != 1 {
		t.Fatal(snap)
	}
	if err = f.s.Receipt(f.ctx, f.b, f.cid, 1, false); err != nil {
		t.Fatal(err)
	}
	snap, err = f.s.Bootstrap(f.ctx, f.a, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Conversations[0].PeerDeliveredSeq != 1 || snap.Conversations[0].PeerReadSeq != 0 {
		t.Fatal(snap)
	}
	if err = f.s.Receipt(f.ctx, f.b, f.cid, 2, true); err == nil {
		t.Fatal("receipt ahead of committed history")
	}
	if err = f.s.Receipt(f.ctx, f.b, f.cid, 1, true); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Receipt(f.ctx, f.b, f.cid, 0, true); err != nil {
		t.Fatal(err)
	}
	snap, err = f.s.Bootstrap(f.ctx, f.b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Conversations[0].UnreadCount != 0 || snap.Conversations[0].LastReadSeq != 1 {
		t.Fatal(snap)
	}
	mustSend(t, f, f.b, f.cid, input("my own outgoing"))
	snap, err = f.s.Bootstrap(f.ctx, f.b, 0)
	if err != nil || snap.Conversations[0].UnreadCount != 0 {
		t.Fatal(snap, err)
	}
}

func TestPaginationAndEventRecovery(t *testing.T) {
	f := setup(t)
	for i := 0; i < 55; i++ {
		mustSend(t, f, f.a, f.cid, input(fmt.Sprintf("message %d", i)))
	}
	page, err := f.s.History(f.ctx, f.b, f.cid, -1, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || len(page.Messages) != 20 || page.Messages[0].Seq != 36 || page.Messages[19].Seq != 55 {
		t.Fatal(page)
	}
	before, err := f.s.History(f.ctx, f.b, f.cid, -1, 36, 20)
	if err != nil || before.Messages[0].Seq != 16 || before.Messages[19].Seq != 35 {
		t.Fatal(before, err)
	}
	recovered, err := f.s.History(f.ctx, f.b, f.cid, 0, 0, 50)
	if err != nil || !recovered.HasMore || recovered.Messages[0].Seq != 1 {
		t.Fatal(recovered, err)
	}
	rest, err := f.s.History(f.ctx, f.b, f.cid, 50, 0, 50)
	if err != nil || rest.HasMore || len(rest.Messages) != 5 {
		t.Fatal(rest, err)
	}
	events, err := f.s.Events(f.ctx, f.a, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range events.Events {
		if e.Cursor != int64(i+1) {
			t.Fatal("event gap", events)
		}
	}
	if len(events.Events) != 56 {
		t.Fatal("missing sender echo", len(events.Events))
	}
}

func TestRequestsRepliesAndReveal(t *testing.T) {
	f := setup(t)
	intro := input("Hello socially")
	cid, err := f.s.Open(f.ctx, f.a, f.third, "request", intro)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.Open(f.ctx, f.a, f.third, "request", intro); err != nil {
		t.Fatal("intro retry", err)
	}
	if _, err = f.s.Send(f.ctx, f.a, cid, input("second intro")); err == nil {
		t.Fatal("pending sender spam")
	}
	if _, err = f.s.Send(f.ctx, f.third, cid, input("reply before accepting")); err == nil {
		t.Fatal("pending recipient sent")
	}
	if err = f.s.Action(f.ctx, f.a, cid, "accept"); err == nil {
		t.Fatal("sender accepted own request")
	}
	if err = f.s.Action(f.ctx, f.third, cid, "accept"); err != nil {
		t.Fatal(err)
	}
	m := mustSend(t, f, f.third, cid, input("Accepted"))
	reply := input("cross-thread reply")
	reply.ReplyToID = &m.ID
	if _, err = f.s.Send(f.ctx, f.a, f.cid, reply); err == nil {
		t.Fatal("cross-thread reply accepted")
	}
	// Server-authorized anonymous fixture; no public API accepts an arbitrary peer.
	a, b := chatlog.Pair(f.a, f.third)
	anon := uuid.NewString()
	if _, err = f.pool.Exec(f.ctx, `insert into conversations(id,user_a,user_b,origin) values($1,$2,$3,'anonymous')`, anon, a, b); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(f.ctx, `insert into conversation_participants(conversation_id,user_id) values($1,$2),($1,$3)`, anon, a, b); err != nil {
		t.Fatal(err)
	}
	mustSend(t, f, f.a, anon, input("Anonymous hello"))
	for _, u := range []string{f.a, f.third} {
		snap, e := f.s.Bootstrap(f.ctx, u, 0)
		if e != nil {
			t.Fatal(e)
		}
		for _, c := range snap.Conversations {
			if c.ID == anon && (!c.IdentityHidden || c.ProfileID != "") {
				t.Fatal("identity leaked", c)
			}
		}
	}
	if err = f.s.Action(f.ctx, f.a, anon, "reveal"); err != nil {
		t.Fatal(err)
	}
	var at *time.Time
	if err = f.pool.QueryRow(f.ctx, `select revealed_at from conversations where id=$1`, anon).Scan(&at); err != nil || at != nil {
		t.Fatal("one consent revealed", at, err)
	}
	if err = f.s.Action(f.ctx, f.third, anon, "reveal"); err != nil {
		t.Fatal(err)
	}
	if err = f.pool.QueryRow(f.ctx, `select revealed_at from conversations where id=$1`, anon).Scan(&at); err != nil || at == nil {
		t.Fatal("two consents did not reveal", err)
	}
}

func TestBlockAndUnmatchCloseChat(t *testing.T) {
	f := setup(t)
	mustSend(t, f, f.a, f.cid, input("before block"))
	svc := safety.New(f.pool, f.users)
	if err := svc.Block(f.ctx, f.b, f.a, ""); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{f.a, f.b} {
		if _, err := f.s.Send(f.ctx, uid, f.cid, input("blocked")); err == nil {
			t.Fatal("blocked send")
		}
		if _, err := f.s.History(f.ctx, uid, f.cid, -1, 0, 50); err == nil {
			t.Fatal("blocked history")
		}
		snap, err := f.s.Bootstrap(f.ctx, uid, 0)
		if err != nil || len(snap.Conversations) != 0 {
			t.Fatal("blocked inbox", snap, err)
		}
	}
	if err := svc.Unblock(f.ctx, f.b, f.a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Send(f.ctx, f.a, f.cid, input("unblock does not reopen")); err == nil {
		t.Fatal("reopened on unblock")
	}
	g := setup(t)
	if err := safety.New(g.pool, g.users).Unmatch(g.ctx, g.a, g.match); err != nil {
		t.Fatal(err)
	}
	if _, err := g.s.Send(g.ctx, g.a, g.cid, input("unmatched")); err == nil {
		t.Fatal("unmatched send")
	}
}

func token(uid string) string {
	raw, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": uid, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()}).SignedString(signingKey)
	return raw
}
func ticket(t *testing.T, base, uid string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/chat/realtime-ticket", nil)
	req.Header.Set("Authorization", "Bearer "+token(uid))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data map[string]string
	if err = json.NewDecoder(resp.Body).Decode(&data); err != nil || resp.StatusCode != 200 {
		t.Fatal(data, err, resp.StatusCode)
	}
	return data["ticket"]
}

func TestWebSocketUpgradeReplayAndTicketReuse(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	go f.s.Run(ctx)
	mux := http.NewServeMux()
	f.s.Routes(mux)
	server := httptest.NewUnstartedServer(httpx.Logger(mux))
	server.Config.WriteTimeout = 50 * time.Millisecond
	server.Start()
	defer server.Close()
	connect := func(tok string) *websocket.Conn {
		t.Helper()
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/chat/ws", http.Header{"Origin": []string{"http://chat.test"}})
		if err != nil {
			t.Fatal(err)
		}
		if err = ws.WriteJSON(map[string]string{"ticket": tok, "after": "0"}); err != nil {
			t.Fatal(err)
		}
		return ws
	}
	tok := ticket(t, server.URL, f.b)
	ws := connect(tok)
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var frame struct {
		Type string    `json:"type"`
		Data EventPage `json:"data"`
	}
	if err := ws.ReadJSON(&frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "events" || len(frame.Data.Events) != 1 {
		t.Fatal(frame)
	}
	// Past the HTTP write deadline, delivery must still succeed after hijack.
	time.Sleep(100 * time.Millisecond)
	mustSend(t, f, f.a, f.cid, input("socket notification"))
	found := false
	for !found {
		if err := ws.ReadJSON(&frame); err != nil {
			t.Fatal(err)
		}
		for _, e := range frame.Data.Events {
			if e.Kind == "message.new" {
				found = true
			}
		}
	}
	replay := connect(tok)
	defer replay.Close()
	replay.SetReadDeadline(time.Now().Add(time.Second))
	if err := replay.ReadJSON(&frame); err == nil {
		t.Fatal("spent ticket reused")
	}
	bad, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/chat/ws", http.Header{"Origin": []string{"https://evil.test"}})
	if bad != nil {
		bad.Close()
	}
	if err == nil || resp.StatusCode != 403 {
		t.Fatal("bad Origin accepted")
	}
}

func TestPushEnqueueRollbackAndCommit(t *testing.T) {
	f := setup(t)
	q, err := chatpush.New(f.pool, chatpush.Config{PublicKey: "test", PrivateKey: "test", Subject: "mailto:test@example.test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	sub := uuid.NewString()
	_, err = f.pool.Exec(f.ctx, `insert into chat_push_subscriptions(id,user_id,endpoint,p256dh,auth) values($1,$2,$3,'test','test')`, sub, f.b, "https://fcm.googleapis.com/test/"+sub)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Enqueue(f.ctx, tx, uuid.NewString(), f.b, 1); err != nil {
		t.Fatal(err)
	}
	tx.Rollback(f.ctx)
	var n int
	if err = f.pool.QueryRow(f.ctx, `select count(*) from river_job where args->>'subscriptionId'=$1`, sub).Scan(&n); err != nil || n != 0 {
		t.Fatal("rolled-back job persisted", n, err)
	}
	f.s.enqueue = q.Enqueue
	m := mustSend(t, f, f.a, f.cid, input("push"))
	if err = f.pool.QueryRow(f.ctx, `select count(*) from river_job where args->>'messageId'=$1`, m.ID).Scan(&n); err != nil || n != 1 {
		t.Fatal("missing push job", n, err)
	}
}

func TestAnonymousPairingEndpoint(t *testing.T) {
	f := setup(t)
	mux := http.NewServeMux()
	f.s.Routes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	// a and b already have a dating context; pair a with the fresh third account.
	join := func(uid string) map[string]any {
		req, _ := http.NewRequest("POST", server.URL+"/chat/anonymous", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Authorization", "Bearer "+token(uid))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("pair: %s", data)
		}
		out := map[string]any{}
		json.Unmarshal(data, &out)
		return out
	}
	if out := join(f.a); out["waiting"] != true {
		t.Fatal(out)
	}
	out := join(f.third)
	if out["id"] == nil {
		t.Fatal(out)
	}
	if again := join(f.a); again["id"] != out["id"] {
		t.Fatal("pair wasn't visible to waiting account", again)
	}
}

func TestPairLockSerializesBlockAndSend(t *testing.T) {
	f := setup(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var blockErr error
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, _ = f.s.Send(f.ctx, f.a, f.cid, input("race")) }()
	go func() { defer wg.Done(); <-start; blockErr = safety.New(f.pool, f.users).Block(f.ctx, f.b, f.a, "") }()
	close(start)
	wg.Wait()
	if blockErr != nil {
		t.Fatal(blockErr)
	}
	_, err := f.s.Send(f.ctx, f.a, f.cid, input("after block returned"))
	if !errors.Is(err, unavailable) {
		t.Fatal("post-block send", err)
	}
}
