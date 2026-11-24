package chat

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestFillBoard(t *testing.T) {
	cards := func(fit, other int) []BoardHello {
		var out []BoardHello
		for range fit {
			out = append(out, BoardHello{Fits: true})
		}
		for range other {
			out = append(out, BoardHello{})
		}
		return out
	}
	count := func(list []BoardHello) (fit, other int) {
		for _, h := range list {
			if h.Fits {
				fit++
			} else {
				other++
			}
		}
		return
	}
	// Few fit: everyone else fills in after them.
	if fit, other := count(fillBoard(cards(3, 4))); fit != 3 || other != 4 {
		t.Fatalf("few fitting: got %d fit, %d other", fit, other)
	}
	// Nobody fits: the board is everyone, rather than empty.
	if fit, other := count(fillBoard(cards(0, 5))); fit != 0 || other != 5 {
		t.Fatalf("none fitting: got %d fit, %d other", fit, other)
	}
	// Enough fit: preferences hold again.
	if fit, other := count(fillBoard(cards(HelloFillBelow, 6))); fit != HelloFillBelow || other != 0 {
		t.Fatalf("enough fitting: got %d fit, %d other", fit, other)
	}
	// Capped at the board size.
	if got := len(fillBoard(cards(2, 98))); got != helloBoardSize {
		t.Fatalf("cap: got %d", got)
	}
}

// When few people fit, a hello reaches people outside the author's
// preferences too, and they can answer it; once enough fit, it doesn't.
func TestHelloBoardFillsBeyondPreferences(t *testing.T) {
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
	board := func(uid string) []map[string]any {
		t.Helper()
		code, out := call("GET", "/chat/anonymous/hellos", uid, ``)
		if code != 200 {
			t.Fatal("board", code, out)
		}
		var cards []map[string]any
		for _, c := range out["hellos"].([]any) {
			cards = append(cards, c.(map[string]any))
		}
		return cards
	}
	var made []string
	person := func(gender, wants string) string {
		t.Helper()
		id := uuid.NewString()
		if _, err := f.pool.Exec(f.ctx, `insert into users(id,status) values($1,'active')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(f.ctx, `insert into profiles(user_id,first_name,birth_date,gender,interested_in) values($1,'Chat tester','1996-01-01',$2,$3)`, id, gender, []string{wants}); err != nil {
			t.Fatal(err)
		}
		made = append(made, id)
		return id
	}
	defer func() { f.pool.Exec(f.ctx, `delete from users where id=any($1::uuid[])`, made) }()

	// a is a woman. Two men who only want men are outside her preferences
	// both ways, and no other hellos are up for them.
	if code, out := call("POST", "/chat/anonymous/hello", f.a, `{"body":"Tea or coffee?"}`); code != 200 {
		t.Fatal("hello", code, out)
	}
	outsider, later := person("man", "man"), person("man", "man")
	cards := board(outsider)
	if len(cards) != 1 || cards[0]["body"] != "Tea or coffee?" || cards[0]["fits"] != false {
		t.Fatalf("outsider should see a's hello, marked as outside preferences: %v", cards)
	}
	if mine := board(f.third); len(mine) != 1 || mine[0]["fits"] != true {
		t.Fatalf("a matching person should see it as fitting: %v", mine)
	}
	if code, out := call("POST", "/chat/anonymous/hellos/"+cards[0]["id"].(string)+"/reply", outsider, `{"body":"Coffee, black"}`); code != 200 || out["id"] == nil {
		t.Fatal("outsider could not answer", code, out)
	}
	// b already chats with a: never shown, preferences or not.
	if len(board(f.b)) != 0 {
		t.Fatal("hello shown to someone a already chats with")
	}

	// Once enough hellos fit "later", a's no longer fills his board.
	for range HelloFillBelow {
		author := person("man", "man")
		if _, err := f.pool.Exec(f.ctx, `insert into blind_hellos(user_id,body,client_message_id,expires_at) values($1,'hi',$2,now()+interval '1 day')`, author, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	cards = board(later)
	if len(cards) != HelloFillBelow {
		t.Fatalf("want only the %d fitting hellos, got %d", HelloFillBelow, len(cards))
	}
	for _, c := range cards {
		if c["fits"] != true {
			t.Fatalf("non-fitting hello shown once enough fit: %v", c)
		}
	}
}
