package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/shak0x90/velora_backend/internal/chatlog"
)

// Opt-in capacity smoke test, not a production sizing benchmark. It holds 500
// distinct authenticated users online and sends 50 concurrent HTTP messages to
// different pairs, verifying both sender and recipient event delivery.
func TestLoad500Connections(t *testing.T) {
	if os.Getenv("CHAT_LOAD_TEST") != "1" {
		t.Skip("set CHAT_LOAD_TEST=1 with the isolated integration database")
	}
	f := setup(t)
	ids := make([]string, 500)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	if _, err := f.pool.Exec(f.ctx, `insert into users(id,status) select unnest($1::uuid[]),'active'`, ids); err != nil {
		t.Fatal(err)
	}
	defer f.pool.Exec(f.ctx, `delete from users where id=any($1::uuid[])`, ids)
	if _, err := f.pool.Exec(f.ctx, `insert into profiles(user_id,first_name,birth_date,gender) select unnest($1::uuid[]),'Load tester','1996-01-01','woman'`, ids); err != nil {
		t.Fatal(err)
	}
	cids := make([]string, 50)
	for i := range cids {
		a, b := chatlog.Pair(ids[2*i], ids[2*i+1])
		if _, err := f.pool.Exec(f.ctx, `insert into matches(id,user_a,user_b) values($1,$2,$3)`, uuid.NewString(), a, b); err != nil {
			t.Fatal(err)
		}
		var err error
		cids[i], err = f.s.Open(f.ctx, a, b, "match", SendInput{})
		if err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	f.s.Routes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	// Use the service's real Postgres listener and fallback timer.
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	go f.s.Run(ctx)
	sockets := make([]*websocket.Conn, 500)
	defer func() {
		for _, ws := range sockets {
			if ws != nil {
				ws.Close()
			}
		}
	}()
	errCh := make(chan error, 500)
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup
	for i, uid := range ids {
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req, _ := http.NewRequest("POST", server.URL+"/chat/realtime-ticket", nil)
			req.Header.Set("Authorization", "Bearer "+token(uid))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			var out map[string]string
			err = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 {
				errCh <- fmt.Errorf("ticket: %d %v", resp.StatusCode, err)
				return
			}
			ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/chat/ws", http.Header{"Origin": []string{"http://chat.test"}})
			if err != nil {
				errCh <- err
				return
			}
			sockets[i] = ws
			if err = ws.WriteJSON(map[string]string{"ticket": out["ticket"], "after": "0"}); err != nil {
				errCh <- err
				return
			}
			ws.SetReadDeadline(time.Now().Add(15 * time.Second))
			var frame struct {
				Type string `json:"type"`
			}
			if err = ws.ReadJSON(&frame); err != nil {
				errCh <- err
				return
			}
			if frame.Type != "events" {
				errCh <- fmt.Errorf("unexpected hello: %s", frame.Type)
			}
		}(i, uid)
	}
	wg.Wait()
	if len(errCh) > 0 {
		t.Fatal(<-errCh)
	}
	start := time.Now()
	delivered := make(chan time.Duration, 100)
	for i, ws := range sockets {
		wg.Add(1)
		go func(i int, ws *websocket.Conn) {
			defer wg.Done()
			ws.SetReadDeadline(time.Now().Add(20 * time.Second))
			for {
				var frame struct {
					Type string    `json:"type"`
					Data EventPage `json:"data"`
				}
				if err := ws.ReadJSON(&frame); err != nil {
					if i < 100 {
						errCh <- err
					}
					return
				}
				for _, e := range frame.Data.Events {
					if e.Kind == "message.new" {
						delivered <- time.Since(start)
						return
					}
				}
			}
		}(i, ws)
	}
	acks := make(chan time.Duration, 50)
	var writers sync.WaitGroup
	for i, cid := range cids {
		writers.Add(1)
		go func(i int, cid string) {
			defer writers.Done()
			body, _ := json.Marshal(input("Capacity smoke test"))
			req, _ := http.NewRequest("POST", server.URL+"/conversations/"+cid+"/messages", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token(ids[i*2]))
			req.Header.Set("Content-Type", "application/json")
			began := time.Now()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			resp.Body.Close()
			if resp.StatusCode != 201 {
				errCh <- fmt.Errorf("send: %d", resp.StatusCode)
				return
			}
			acks <- time.Since(began)
		}(i, cid)
	}
	writers.Wait()
	if len(errCh) > 0 {
		t.Fatal(<-errCh)
	}
	latencies := make([]time.Duration, 0, 100)
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for len(latencies) < 100 {
		select {
		case d := <-delivered:
			latencies = append(latencies, d)
		case err := <-errCh:
			t.Fatal(err)
		case <-timer.C:
			t.Fatalf("only %d/100 message events arrived", len(latencies))
		}
	}
	for _, ws := range sockets {
		ws.Close()
	}
	wg.Wait()
	close(acks)
	ackTimes := make([]time.Duration, 0, 50)
	for d := range acks {
		ackTimes = append(ackTimes, d)
	}
	sort.Slice(ackTimes, func(i, j int) bool { return ackTimes[i] < ackTimes[j] })
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("500 authenticated sockets; 50 concurrent sends; 100 sender/recipient events; HTTP ACK p95=%s; burst-to-event p95=%s", ackTimes[47], latencies[94])
}
