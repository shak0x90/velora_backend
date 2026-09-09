package chat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

type socketClient struct {
	user      string
	conn      *websocket.Conn
	wake      chan struct{}
	ephemeral chan any
}
type Hub struct {
	s       *Service
	mu      sync.Mutex
	clients map[*socketClient]bool
	closed  bool
}

func NewHub(s *Service) *Hub { return &Hub{s: s, clients: map[*socketClient]bool{}} }
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		c.conn.Close()
	}
}
func (h *Hub) signal(ids ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if len(ids) == 0 || slices.Contains(ids, c.user) {
			select {
			case c.wake <- struct{}{}:
			default:
			}
		}
	}
}
func (h *Hub) logout(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.user == id {
			c.conn.Close()
		}
	}
}
func (h *Hub) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				h.Close()
				return
			case <-t.C:
				h.signal()
			}
		}
	}()
	for ctx.Err() == nil {
		conn, err := pgx.ConnectConfig(ctx, h.s.pool.Config().ConnConfig.Copy())
		if err == nil {
			_, err = conn.Exec(ctx, "LISTEN velora_chat")
		}
		if err == nil {
			h.signal()
			for ctx.Err() == nil {
				n, e := conn.WaitForNotification(ctx)
				if e != nil {
					err = e
					break
				}
				if strings.HasPrefix(n.Payload, "logout:") {
					h.logout(strings.TrimPrefix(n.Payload, "logout:"))
				} else {
					h.signal(strings.Split(n.Payload, ",")...)
				}
			}
		}
		if conn != nil {
			conn.Close(context.Background())
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("chat listener reconnecting", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) handleTicket(w http.ResponseWriter, r *http.Request) {
	// RequireAuth already validated this token. Read its expiry to bound socket life.
	claims := jwt.MapClaims{}
	_, _, err := jwt.NewParser().ParseUnverified(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), claims)
	if err != nil {
		httpx.Error(w, r, httpx.Unauthorized("Sign in again."))
		return
	}
	expiry, err := claims.GetExpirationTime()
	if err != nil || expiry == nil {
		httpx.Error(w, r, httpx.Unauthorized("Sign in again."))
		return
	}
	if _, err = s.users.CurrentUser(r.Context(), user(r)); err != nil {
		httpx.Error(w, r, httpx.Unauthorized("Sign in again."))
		return
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		httpx.Error(w, r, err)
		return
	}
	ticket := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(ticket))
	_, err = s.pool.Exec(r.Context(), `with expired as(delete from chat_tickets where expires_at<now()) insert into chat_tickets(token_hash,user_id,expires_at,access_expires_at) values($1,$2,now()+interval '30 seconds',$3)`, sum[:], user(r), expiry.Time)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 200, map[string]string{"ticket": ticket})
}
func (s *Service) handleSocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 4096, HandshakeTimeout: 5 * time.Second, CheckOrigin: func(r *http.Request) bool { return slices.Contains(s.origins, r.Header.Get("Origin")) }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(4096)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	conn.SetWriteDeadline(time.Time{})
	var hello struct {
		Ticket string `json:"ticket"`
		After  string `json:"after"`
	}
	if err = conn.ReadJSON(&hello); err != nil {
		return
	}
	after, err := strconv.ParseInt(hello.After, 10, 64)
	if err != nil || after < 0 {
		return
	}
	sum := sha256.Sum256([]byte(hello.Ticket))
	var uid string
	var deadline time.Time
	err = s.pool.QueryRow(r.Context(), `delete from chat_tickets where token_hash=$1 and expires_at>now() and access_expires_at>now() returning user_id::text,access_expires_at`, sum[:]).Scan(&uid, &deadline)
	if err != nil {
		return
	}
	client := &socketClient{user: uid, conn: conn, wake: make(chan struct{}, 1), ephemeral: make(chan any, 8)}
	s.hub.mu.Lock()
	count := 0
	for c := range s.hub.clients {
		if c.user == uid {
			count++
		}
	}
	if s.hub.closed || len(s.hub.clients) >= 2000 || count >= 8 {
		s.hub.mu.Unlock()
		return
	}
	s.hub.clients[client] = true
	s.hub.mu.Unlock()
	defer func() { s.hub.mu.Lock(); delete(s.hub.clients, client); s.hub.mu.Unlock() }()
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	conn.SetReadDeadline(time.Now().Add(75 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(75 * time.Second)) })
	go func() {
		defer cancel()
		var lastTyping time.Time
		for {
			var frame struct {
				Type           string `json:"type"`
				ConversationID string `json:"conversationId"`
			}
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Type == "typing" && time.Since(lastTyping) > 2*time.Second {
				lastTyping = time.Now()
				s.typing(ctx, client, frame.ConversationID)
			}
		}
	}()
	write := func(v any) error { conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); return conn.WriteJSON(v) }
	client.wake <- struct{}{}
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-client.ephemeral:
			if err = write(v); err != nil {
				return
			}
		case <-ticker.C:
			if _, err = s.users.CurrentUser(ctx, uid); err != nil {
				return
			}
			if err = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		case <-client.wake:
			for {
				page, e := s.Events(ctx, uid, after)
				if e != nil {
					return
				}
				// Events contain no content or peer identity; clients refetch authorized views.
				if err = write(map[string]any{"type": "events", "data": page}); err != nil {
					return
				}
				after = page.Cursor
				if !page.HasMore {
					break
				}
			}
		}
	}
}
func (s *Service) typing(ctx context.Context, from *socketClient, id string) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	c, err := s.access(ctx, tx, from.user, id, false)
	if err != nil || c.State != "open" {
		return
	}
	// No identity or stored traffic. TTL is owned by the receiver.
	frame := map[string]string{"type": "typing", "conversationId": id}
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	for peer := range s.hub.clients {
		if peer.user == c.peer(from.user) {
			select {
			case peer.ephemeral <- frame:
			default:
			}
		}
	}
}
