package chatpush

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

func TestEndpointAllowlist(t *testing.T) {
	for _, raw := range []string{"http://fcm.googleapis.com/send", "https://127.0.0.1", "https://fcm.googleapis.com.evil.test/send", "https://user@fcm.googleapis.com/send", "https://fcm.googleapis.com:8080/send"} {
		if allowedEndpoint(raw) {
			t.Errorf("accepted %s", raw)
		}
	}
	if !allowedEndpoint("https://fcm.googleapis.com/send/test") {
		t.Fatal("rejected supported provider")
	}
}

// The provider is replaced with a recorder: this never sends an external push.
func TestWorkerSuppressionAndProviderResults(t *testing.T) {
	dsn := os.Getenv("CHAT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set CHAT_TEST_DATABASE_URL to a migrated isolated database")
	}
	if !strings.Contains(dsn, "velora_chat_test") {
		t.Fatal("integration database name must contain velora_chat_test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a, b, cid, mid, sid := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	if a > b {
		a, b = b, a
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`insert into users(id,status) values($1,'active'),($2,'active')`, a, b)
	defer pool.Exec(ctx, `delete from users where id in($1,$2)`, a, b)
	exec(`insert into matches(id,user_a,user_b) values($1,$2,$3)`, uuid.NewString(), a, b)
	exec(`insert into conversations(id,user_a,user_b,origin,state,last_seq) values($1,$2,$3,'match','open',1)`, cid, a, b)
	exec(`insert into conversation_participants(conversation_id,user_id) values($1,$2),($1,$3)`, cid, a, b)
	exec(`insert into messages(id,conversation_id,seq,sender_id,body,client_message_id) values($1,$2,1,$3,'Private body must not appear in push',$4)`, mid, cid, a, uuid.NewString())
	exec(`insert into chat_push_subscriptions(id,user_id,endpoint,p256dh,auth) values($1,$2,'https://fcm.googleapis.com/send/test','test-key','test-auth')`, sid, b)
	sends := 0
	providerStatus := 201
	q := &Queue{pool: pool, cfg: Config{"public", "private", "mailto:test@example.test"}, send: func(_ context.Context, body []byte, _ *webpush.Subscription, _ *webpush.Options) (*http.Response, error) {
		sends++
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "Private body") || payload["tag"] != "chat-"+mid {
			t.Fatalf("unsafe notification: %s", body)
		}
		return &http.Response{StatusCode: providerStatus, Body: io.NopCloser(strings.NewReader(""))}, nil
	}}
	w := &Worker{q: q}
	job := &river.Job[Args]{Args: Args{mid, b, sid}}
	check := func(want int, wantErr bool) {
		t.Helper()
		sends = 0
		err := w.Work(ctx, job)
		if sends != want || (err != nil) != wantErr {
			t.Fatalf("sends=%d error=%v", sends, err)
		}
	}
	check(1, false)
	exec(`update conversation_participants set last_read_seq=1 where conversation_id=$1 and user_id=$2`, cid, b)
	check(0, false)
	exec(`update conversation_participants set last_read_seq=0,muted=true where conversation_id=$1 and user_id=$2`, cid, b)
	check(0, false)
	exec(`update conversation_participants set muted=false where conversation_id=$1`, cid)
	exec(`update conversations set state='closed' where id=$1`, cid)
	check(0, false)
	exec(`update conversations set state='open' where id=$1`, cid)
	exec(`insert into blocks(user_id,blocked_id) values($1,$2)`, b, a)
	check(0, false)
	exec(`delete from blocks where user_id=$1`, b)
	exec(`update users set status='suspended' where id=$1`, a)
	check(0, false)
	exec(`update users set status='active' where id=$1`, a)
	exec(`delete from matches where user_a=$1 and user_b=$2`, a, b)
	check(0, false)
	exec(`insert into matches(id,user_a,user_b) values($1,$2,$3)`, uuid.NewString(), a, b)
	providerStatus = 503
	check(1, true)
	providerStatus = 410
	check(1, false)
	check(0, false) // Expired subscription was removed by the preceding attempt.
}
