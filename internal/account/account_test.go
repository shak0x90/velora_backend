package account

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/chatlog"
	"github.com/shak0x90/velora_backend/internal/db"
	"github.com/shak0x90/velora_backend/internal/media"
)

type fakeStore struct {
	mu      sync.Mutex
	deleted []string
	fail    bool
}

func (f *fakeStore) Put(context.Context, string, []byte, string) error { return nil }
func (f *fakeStore) URL(key string) string                             { return key }
func (f *fakeStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, key)
	if f.fail {
		return errors.New("store unavailable")
	}
	return nil
}

func TestRemoveFilesDeletesEveryVariant(t *testing.T) {
	store := &fakeStore{}
	removeFiles(context.Background(), store, "u", []string{"u/abc-full.jpg", "u/def-full.jpg"})
	want := []string{
		"u/abc-full.jpg", "u/abc-card.jpg", "u/abc-thumb.jpg",
		"u/def-full.jpg", "u/def-card.jpg", "u/def-thumb.jpg",
	}
	if strings.Join(store.deleted, ",") != strings.Join(want, ",") {
		t.Fatalf("deleted %v, want %v", store.deleted, want)
	}
}

func TestRemoveFilesKeepsGoingAfterFailure(t *testing.T) {
	store := &fakeStore{fail: true}
	removeFiles(context.Background(), store, "u", []string{"u/abc-full.jpg"})
	if len(store.deleted) != len(media.AllVariants) {
		t.Fatalf("a failed delete stopped the rest: %v", store.deleted)
	}
}

// Run against an isolated database: ACCOUNT_TEST_DATABASE_URL=.../velora_account_test.
func TestDeleteAccount(t *testing.T) {
	dsn := os.Getenv("ACCOUNT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ACCOUNT_TEST_DATABASE_URL to an isolated velora_account_test database")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.ConnConfig.Database, "_account_test") {
		t.Fatal("database name must end in _account_test")
	}
	if err := db.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// gone deletes their account; match is matched and chatting with them;
	// reporter has an open report about them and a thread with them.
	gone, match, reporter := uuid.NewString(), uuid.NewString(), uuid.NewString()
	everyone := []string{gone, match, reporter}
	t.Cleanup(func() {
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer p.Close()
		if _, err := p.Exec(ctx, `delete from reports where reporter_id=any($1::uuid[]) or subject_id=any($1::uuid[])`, everyone); err != nil {
			t.Error(err)
		}
		if _, err := p.Exec(ctx, `delete from users where id=any($1::uuid[])`, everyone); err != nil {
			t.Error(err)
		}
	})
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(sql, err)
		}
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(sql, err)
		}
		return n
	}

	for _, id := range everyone {
		exec(`insert into users(id) values($1)`, id)
		exec(`insert into profiles(user_id,first_name,birth_date,gender) values($1,'Deletion tester','1995-01-01','woman')`, id)
		exec(`insert into auth_identities(user_id,method,identifier) values($1,'google',$2)`, id, "sub-"+id)
	}
	exec(`insert into profile_photos(id,user_id,position,object_key,status) values($1,$2,0,$3,'ready')`, uuid.New(), gone, gone+"/abc-full.jpg")
	exec(`insert into profile_prompts(id,user_id,position,question,answer) values($1,$2,0,'q','a')`, uuid.New(), gone)
	exec(`insert into personality_answers(user_id,question_id,question,answer) values($1,'q1','q','a')`, gone)
	exec(`insert into refresh_tokens(id,user_id,family_id,token_hash,expires_at,auth_method) values($1,$2,$1,$3,now()+interval '1 day','google')`, uuid.New(), gone, []byte(gone))
	exec(`insert into chat_tickets(token_hash,user_id,expires_at,access_expires_at) values($1,$2,now()+interval '1 minute',now()+interval '1 minute')`, []byte(gone), gone)
	exec(`insert into chat_queue(user_id,expires_at) values($1,now()+interval '1 minute')`, gone)
	exec(`insert into chat_push_subscriptions(id,user_id,endpoint,p256dh,auth) values($1,$2,$3,'k','a')`, uuid.New(), gone, "https://push.test/"+gone)
	exec(`insert into likes(id,from_user_id,to_user_id) values($1,$2,$3)`, uuid.New(), reporter, gone)
	exec(`insert into passes(user_id,target_id) values($1,$2)`, gone, reporter)
	exec(`insert into blocks(user_id,blocked_id) values($1,$2)`, gone, reporter)
	exec(`insert into matches(id,user_a,user_b) values($1,least($2::uuid,$3::uuid),greatest($2::uuid,$3::uuid))`, uuid.New(), gone, match)
	exec(`insert into notifications(id,user_id,kind,title,profile_id) values($1,$2,'match','You matched',$3)`, uuid.New(), match, gone)
	exec(`insert into reports(id,reporter_id,subject_id,reason,context) values($1,$2,$3,'harassment','message')`, uuid.New(), reporter, gone)

	conversation := func(peer string) string {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		id, err := chatlog.EnsureMatch(ctx, tx, gone, peer)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `insert into messages(id,conversation_id,seq,sender_id,body,client_message_id) values
			($1,$3,1,$4,'from the deleted account','c1'),($2,$3,2,$5,'from the other person','c2')`,
			uuid.New(), uuid.New(), id, gone, peer); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return id
	}
	withMatch, withReporter := conversation(match), conversation(reporter)

	store := &fakeStore{}
	s := &Service{pool: pool, store: store}
	if err := s.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}

	var status string
	var hasDeletedAt bool
	if err := pool.QueryRow(ctx, `select status, deleted_at is not null from users where id=$1`, gone).Scan(&status, &hasDeletedAt); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" || !hasDeletedAt {
		t.Fatalf("users row: status=%q deletedAt=%v", status, hasDeletedAt)
	}

	for table, where := range map[string]string{
		"profiles":                "user_id=$1",
		"profile_photos":          "user_id=$1",
		"profile_prompts":         "user_id=$1",
		"personality_answers":     "user_id=$1",
		"auth_identities":         "user_id=$1",
		"refresh_tokens":          "user_id=$1",
		"chat_tickets":            "user_id=$1",
		"chat_queue":              "user_id=$1",
		"chat_push_subscriptions": "user_id=$1",
		"devices":                 "user_id=$1",
		"user_settings":           "user_id=$1",
		"daily_edits":             "user_id=$1 or profile_id=$1",
		"likes":                   "from_user_id=$1 or to_user_id=$1",
		"passes":                  "user_id=$1 or target_id=$1",
		"blocks":                  "user_id=$1",
		"matches":                 "user_a=$1 or user_b=$1",
		"notifications":           "user_id=$1 or profile_id=$1",
		"user_events":             "user_id=$1",
	} {
		if n := count(`select count(*) from `+table+` where `+where, gone); n != 0 {
			t.Errorf("%s still has %d rows for the deleted account", table, n)
		}
	}

	if n := count(`select count(*) from conversations where (user_a=$1 or user_b=$1) and state<>'closed'`, gone); n != 0 {
		t.Errorf("%d conversations left open", n)
	}
	if n := count(`select count(*) from user_events where user_id=$1 and conversation_id=$2 and kind='conversation.access'`, match, withMatch); n != 1 {
		t.Errorf("the other person was not told the thread closed (%d events)", n)
	}

	if n := count(`select count(*) from messages where conversation_id=$1 and sender_id=$2 and body='' and deleted_at is not null`, withMatch, gone); n != 1 {
		t.Error("the deleted account's message was not redacted")
	}
	if n := count(`select count(*) from messages where conversation_id=$1 and sender_id=$2 and body='from the other person' and deleted_at is null`, withMatch, match); n != 1 {
		t.Error("the other person's message was touched")
	}
	if n := count(`select count(*) from messages where conversation_id=$1 and sender_id=$2 and body='from the deleted account'`, withReporter, gone); n != 1 {
		t.Error("evidence for an open report was redacted")
	}
	if n := count(`select count(*) from reports where subject_id=$1`, gone); n != 1 {
		t.Error("the report about the deleted account was lost")
	}
	if n := count(`select count(*) from profiles where user_id=any($1::uuid[])`, []string{match, reporter}); n != 2 {
		t.Error("someone else's profile was deleted")
	}

	if len(store.deleted) != len(media.AllVariants) || !strings.HasPrefix(store.deleted[0], gone+"/abc-") {
		t.Errorf("photo files: %v", store.deleted)
	}

	if err := s.Delete(ctx, gone); !errors.Is(err, ErrNotActive) {
		t.Errorf("deleting twice: %v", err)
	}
}
