// Package practice is the AI practice chat: a clearly labelled AI
// conversation partner someone can warm up with while nobody is around for a
// blind chat.
//
// It is never presented as a person. The app labels it as AI on every screen,
// it never becomes a match, it has no reveal, and the model is instructed to
// say it is an AI whenever asked. Nothing is stored: the app sends the short
// history with each turn.
package practice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

// Personas are the characters on offer, by id. The app shows the same list.
var Personas = map[string]string{
	"books":    "loves novels, bookshops and slow Sunday mornings with coffee",
	"outdoors": "loves hiking, camping, sunrise walks and trying new trails",
	"food":     "loves cooking, street food, trying new restaurants and hosting dinners",
	"music":    "loves live music, making playlists and discovering small bands",
	"travel":   "loves travelling, learning bits of languages and planning the next trip",
	"creative": "loves photography, drawing, design and visiting galleries",
}

const (
	maxTurns      = 20
	maxTurnRunes  = 500
	requestsLimit = 60 // per person per hour
)

type Turn struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

type Request struct {
	Persona  string `json:"persona"`
	Messages []Turn `json:"messages"`
}

type Reply struct {
	Reply string `json:"reply"`
}

type Service struct {
	key    string
	model  string
	url    string
	client *http.Client
	guard  func(http.Handler) http.Handler
	limit  *httpx.Limiter
}

// New reads the key from keyFile. An empty path returns nil: the practice
// chat is off and its route is not registered.
func New(keyFile, model string, users *auth.Service) (*Service, error) {
	if keyFile == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("OPENAI_API_KEY_FILE: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY_FILE is empty")
	}
	return &Service{
		key:    key,
		model:  model,
		url:    "https://api.openai.com/v1/chat/completions",
		client: &http.Client{Timeout: 30 * time.Second},
		guard:  users.RequireAuth,
		limit:  httpx.NewLimiter(requestsLimit, time.Hour),
	}, nil
}

// Available reports whether the practice chat is configured.
func (s *Service) Available() bool { return s != nil }

// Routes registers POST /chat/practice.
func (s *Service) Routes(mux *http.ServeMux) {
	if s == nil {
		return
	}
	mux.Handle("POST /chat/practice", s.guard(http.HandlerFunc(s.handle)))
}

func systemPrompt(persona string) string {
	return "You are Thapaw's AI practice partner, inside a dating app. You are an AI, not a person, " +
		"and the person you are talking to knows that: never claim or imply to be human, and if asked, " +
		"say plainly that you are an AI practice partner. Play a friendly, warm character who " + persona + ". " +
		"Help the person practise easy, genuine conversation: reply in one to three short sentences, " +
		"show interest, and usually ask one question back. Keep it kind and PG. Never ask for or encourage " +
		"sharing contact details, addresses, money or photos, and never suggest meeting up. " +
		"If the person asks for advice on how they are coming across, give one short, encouraging tip."
}

func validate(in Request) (Request, error) {
	persona, ok := Personas[in.Persona]
	if !ok {
		return in, httpx.BadRequest("Choose a practice partner.")
	}
	in.Persona = persona
	if len(in.Messages) == 0 {
		return in, httpx.BadRequest("Say something to start.")
	}
	if len(in.Messages) > maxTurns {
		in.Messages = in.Messages[len(in.Messages)-maxTurns:]
	}
	for i, t := range in.Messages {
		if t.Role != "user" && t.Role != "assistant" {
			return in, httpx.BadRequest("Messages are from user or assistant.")
		}
		t.Content = strings.TrimSpace(t.Content)
		if t.Content == "" || !utf8.ValidString(t.Content) || utf8.RuneCountInString(t.Content) > maxTurnRunes {
			return in, httpx.BadRequest("Keep each message under 500 characters.")
		}
		in.Messages[i] = t
	}
	if in.Messages[len(in.Messages)-1].Role != "user" {
		return in, httpx.BadRequest("The last message must be yours.")
	}
	return in, nil
}

func unavailable() httpx.Problem {
	return httpx.Problem{
		Type:   "about:blank#unavailable",
		Title:  "Unavailable",
		Status: http.StatusServiceUnavailable,
		Detail: "The practice partner is unavailable right now. Try again in a moment.",
	}
}

func (s *Service) handle(w http.ResponseWriter, r *http.Request) {
	uid := auth.UserIDFrom(r.Context())
	if ok, _ := s.limit.Allow(uid); !ok {
		httpx.Error(w, r, httpx.TooManyRequests("That's a lot of practice for one hour. Take a break and come back."))
		return
	}
	var in Request
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in, err := validate(in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	reply, err := s.complete(r.Context(), in)
	if err != nil {
		httpx.Error(w, r, unavailable())
		return
	}
	httpx.JSON(w, http.StatusOK, Reply{Reply: reply})
}

func (s *Service) complete(ctx context.Context, in Request) (string, error) {
	msgs := []map[string]string{{"role": "system", "content": systemPrompt(in.Persona)}}
	for _, t := range in.Messages {
		msgs = append(msgs, map[string]string{"role": t.Role, "content": t.Content})
	}
	body, _ := json.Marshal(map[string]any{
		"model":       s.model,
		"messages":    msgs,
		"max_tokens":  160,
		"temperature": 0.8,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai: %s", res.Status)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("openai: empty reply")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
