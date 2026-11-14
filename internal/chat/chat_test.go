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
	"github.com/jackc/pgx/v5"
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
	snap, err := f.s.Bootstrap(f.ctx, f.b, "")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Conversations[0].UnreadCount != 1 {
		t.Fatal(snap)
	}
	if err = f.s.Receipt(f.ctx, f.b, f.cid, 1, false); err != nil {
		t.Fatal(err)
	}
	snap, err = f.s.Bootstrap(f.ctx, f.a, "")
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
	snap, err = f.s.Bootstrap(f.ctx, f.b, "")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Conversations[0].UnreadCount != 0 || snap.Conversations[0].LastReadSeq != 1 {
		t.Fatal(snap)
	}
	mustSend(t, f, f.b, f.cid, input("my own outgoing"))
	snap, err = f.s.Bootstrap(f.ctx, f.b, "")
	if err != nil || snap.Conversations[0].UnreadCount != 0 {
		t.Fatal(snap, err)
	}
}

// The inbox shows when the other person was last active; sending a message
// counts as activity, and incognito hides it.
func TestPeerLastActive(t *testing.T) {
	f := setup(t)
	if _, err := f.pool.Exec(f.ctx, `update profiles set last_active_at=now()-interval '2 hours' where user_id=$1`, f.a); err != nil {
		t.Fatal(err)
	}
	peer := func() *time.Time {
		t.Helper()
		snap, err := f.s.Bootstrap(f.ctx, f.b, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range snap.Conversations {
			if c.ID == f.cid {
				return c.PeerLastActive
			}
		}
		t.Fatal("conversation missing")
		return nil
	}
	if at := peer(); at == nil || time.Since(*at) < time.Hour {
		t.Fatalf("want about two hours ago, got %v", at)
	}
	mustSend(t, f, f.a, f.cid, input("hi"))
	if at := peer(); at == nil || time.Since(*at) > time.Minute {
		t.Fatalf("sending should count as active, got %v", at)
	}
	if _, err := f.pool.Exec(f.ctx, `update profiles set incognito=true where user_id=$1`, f.a); err != nil {
		t.Fatal(err)
	}
	if at := peer(); at != nil {
		t.Fatalf("incognito leaked activity: %v", at)
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
		snap, e := f.s.Bootstrap(f.ctx, u, "")
		if e != nil {
			t.Fatal(e)
		}
		for _, c := range snap.Conversations {
			if c.ID == anon && (!c.IdentityHidden || c.ProfileID != "" || c.PeerLastActive != nil) {
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
		snap, err := f.s.Bootstrap(f.ctx, uid, "")
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
	// The 50 ms write timeout above is for the hijacked socket. A ticket
	// request does database work that can outlast it on a slow machine, which
	// cut the response off as EOF; tickets live in the database, so any server
	// on the same service can issue one.
	plain := httptest.NewServer(mux)
	defer plain.Close()
	tok := ticket(t, plain.URL, f.b)
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

func TestHelloBoardAndReplies(t *testing.T) {
	f := setup(t)
	var looked []string
	var lookedMu sync.Mutex
	var answered struct{ author, cid string }
	f.s.WithBlindHooks(BlindHooks{
		Looking: func(_ context.Context, uid string) {
			lookedMu.Lock()
			looked = append(looked, uid)
			lookedMu.Unlock()
		},
		HelloAnswered: func(_ context.Context, _ pgx.Tx, author, cid string) error {
			answered.author, answered.cid = author, cid
			return nil
		},
	})
	mux := http.NewServeMux()
	f.s.Routes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	call := func(method, path, uid, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+token(uid))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		json.Unmarshal(data, &out)
		return resp.StatusCode, out
	}

	// a searches, finds nobody: waits, and the "someone is looking" hook runs.
	if _, out := call("POST", "/chat/anonymous", f.a, `{}`); out["waiting"] != true {
		t.Fatal(out)
	}
	time.Sleep(100 * time.Millisecond)
	lookedMu.Lock()
	if len(looked) != 1 || looked[0] != f.a {
		t.Fatalf("looking hook: %v", looked)
	}
	lookedMu.Unlock()
	if code, _ := call("DELETE", "/chat/anonymous", f.a, ``); code != 204 {
		t.Fatal("leave queue", code)
	}

	// a leaves a blind hello instead of waiting.
	if code, out := call("POST", "/chat/anonymous/hello", f.a, `{"body":"  What's the best thing you ate this week?  "}`); code != 200 || out["body"] != "What's the best thing you ate this week?" {
		t.Fatal("hello", code, out)
	}
	if code, _ := call("POST", "/chat/anonymous/hello", f.a, `{"body":""}`); code != 400 {
		t.Fatal("empty hello accepted", code)
	}

	// Instant search no longer swallows hellos: with nobody searching, the
	// next searcher just waits.
	if _, out := call("POST", "/chat/anonymous", f.third, `{}`); out["waiting"] != true {
		t.Fatal("search consumed a hello", out)
	}
	call("DELETE", "/chat/anonymous", f.third, ``)

	// The hello is on the board for matching people, with age and interests
	// but no name.
	if _, err := f.pool.Exec(f.ctx, `update profiles set interests='{Hiking,Coffee,Jazz}' where user_id=$1`, f.a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `update profiles set interests='{Jazz}' where user_id=$1`, f.third); err != nil {
		t.Fatal(err)
	}
	board := func(uid string) []any {
		t.Helper()
		code, out := call("GET", "/chat/anonymous/hellos", uid, ``)
		if code != 200 {
			t.Fatal("board", code, out)
		}
		return out["hellos"].([]any)
	}
	cards := board(f.third)
	if len(cards) != 1 {
		t.Fatalf("board: %v", cards)
	}
	card := cards[0].(map[string]any)
	if card["body"] != "What's the best thing you ate this week?" || card["age"] == nil || card["shared"] != float64(1) {
		t.Fatalf("card: %v", card)
	}
	if ints := card["interests"].([]any); len(ints) != 3 || ints[0] != "Jazz" {
		t.Fatalf("shared interest should come first: %v", ints)
	}
	if _, named := card["name"]; named {
		t.Fatal("card leaks a name")
	}
	if len(board(f.a)) != 0 {
		t.Fatal("author sees own hello")
	}
	// b already chats with a (their match), so blind chat never pairs them.
	if len(board(f.b)) != 0 {
		t.Fatal("hello shown to someone a already chats with")
	}

	// Replying opens a blind chat: the hello first (from a), then the reply.
	helloID := card["id"].(string)
	code, out := call("POST", "/chat/anonymous/hellos/"+helloID+"/reply", f.third, `{"body":"Mango sticky rice, no contest"}`)
	if code != 200 || out["id"] == nil {
		t.Fatal("reply", code, out)
	}
	cid := out["id"].(string)
	rows, err := f.pool.Query(f.ctx, `select m.body, m.sender_id::text, c.origin from messages m join conversations c on c.id=m.conversation_id where m.conversation_id=$1 order by m.seq`, cid)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var body, sender, origin string
		if err = rows.Scan(&body, &sender, &origin); err != nil || origin != "anonymous" {
			t.Fatal(err, origin)
		}
		got = append(got, sender+":"+body)
	}
	rows.Close()
	if len(got) != 2 || got[0] != f.a+":What's the best thing you ate this week?" || got[1] != f.third+":Mango sticky rice, no contest" {
		t.Fatalf("messages: %v", got)
	}
	if answered.author != f.a || answered.cid != cid {
		t.Fatalf("hello-answered hook: %+v", answered)
	}
	// The hello stays up for others; the replier can't answer twice.
	var replies int
	if err = f.pool.QueryRow(f.ctx, `select reply_count from blind_hellos where user_id=$1`, f.a).Scan(&replies); err != nil || replies != 1 {
		t.Fatal("reply count", replies, err)
	}
	if len(board(f.third)) != 0 {
		t.Fatal("hello still offered to someone who replied")
	}
	if code, _ := call("POST", "/chat/anonymous/hellos/"+helloID+"/reply", f.third, `{"body":"again"}`); code != 404 {
		t.Fatal("replied twice", code)
	}

	// Five replies close it.
	fourth := uuid.NewString()
	if _, err = f.pool.Exec(f.ctx, `insert into users(id,status) values($1,'active')`, fourth); err != nil {
		t.Fatal(err)
	}
	defer f.pool.Exec(f.ctx, `delete from users where id=$1`, fourth)
	if _, err = f.pool.Exec(f.ctx, `insert into profiles(user_id,first_name,birth_date,gender) values($1,'Chat tester','1996-01-01','woman')`, fourth); err != nil {
		t.Fatal(err)
	}
	if len(board(fourth)) != 1 {
		t.Fatal("open hello hidden from a new matching person")
	}
	if _, err = f.pool.Exec(f.ctx, `update blind_hellos set reply_count=5 where user_id=$1`, f.a); err != nil {
		t.Fatal(err)
	}
	if len(board(fourth)) != 0 {
		t.Fatal("full hello still on the board")
	}
	if code, _ := call("POST", "/chat/anonymous/hellos/"+helloID+"/reply", fourth, `{"body":"hi"}`); code != 404 {
		t.Fatal("replied to a full hello", code)
	}
}

// Reporting a hello hides it from the reporter at once, files a report about
// its author, and three different reporters take it off the board.
func TestHelloReports(t *testing.T) {
	f := setup(t)
	mux := http.NewServeMux()
	f.s.Routes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	call := func(method, path, uid, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Authorization", "Bearer "+token(uid))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		out := map[string]any{}
		json.Unmarshal(data, &out)
		return resp.StatusCode, out
	}
	board := func(uid string) int {
		t.Helper()
		code, out := call("GET", "/chat/anonymous/hellos", uid, ``)
		if code != 200 {
			t.Fatal("board", code, out)
		}
		return len(out["hellos"].([]any))
	}
	// Three more matching people besides f.third.
	extra := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for _, id := range extra {
		if _, err := f.pool.Exec(f.ctx, `insert into users(id,status) values($1,'active')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(f.ctx, `insert into profiles(user_id,first_name,birth_date,gender) values($1,'Chat tester','1996-01-01','woman')`, id); err != nil {
			t.Fatal(err)
		}
	}
	defer f.pool.Exec(f.ctx, `delete from users where id=any($1::uuid[])`, extra)

	if code, out := call("POST", "/chat/anonymous/hello", f.a, `{"body":"something rude"}`); code != 200 {
		t.Fatal("hello", code, out)
	}
	var helloID string
	if err := f.pool.QueryRow(f.ctx, `select id::text from blind_hellos where user_id=$1`, f.a).Scan(&helloID); err != nil {
		t.Fatal(err)
	}
	path := "/chat/anonymous/hellos/" + helloID + "/report"
	if code, _ := call("POST", path, f.a, `{}`); code != 404 {
		t.Fatal("reported own hello", code)
	}
	if code, _ := call("POST", path, f.third, `{"reason":"made-up"}`); code != 400 {
		t.Fatal("unknown reason accepted", code)
	}
	if code, out := call("POST", path, f.third, `{"reason":"harassment"}`); code != 204 {
		t.Fatal("report", code, out)
	}
	// Twice counts once.
	call("POST", path, f.third, `{"reason":"harassment"}`)
	var count int
	var detail string
	if err := f.pool.QueryRow(f.ctx, `select report_count from blind_hellos where id=$1`, helloID).Scan(&count); err != nil || count != 1 {
		t.Fatal("report count", count, err)
	}
	if err := f.pool.QueryRow(f.ctx, `select detail from reports where reporter_id=$1 and subject_id=$2`, f.third, f.a).Scan(&detail); err != nil || detail != "Blind hello: something rude" {
		t.Fatal("moderation report", detail, err)
	}
	if board(f.third) != 0 {
		t.Fatal("reporter still sees the hello")
	}
	if board(extra[0]) != 1 {
		t.Fatal("one report hid the hello from everyone")
	}
	if code, _ := call("POST", "/chat/anonymous/hellos/"+helloID+"/reply", f.third, `{"body":"hi"}`); code != 404 {
		t.Fatal("replied to a hello they reported", code)
	}
	call("POST", path, extra[0], `{}`)
	call("POST", path, extra[1], `{"reason":"spam"}`)
	if board(extra[2]) != 0 {
		t.Fatal("three reports left the hello on the board")
	}
}
