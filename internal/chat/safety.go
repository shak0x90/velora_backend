package chat

import (
	"github.com/shak0x90/velora_backend/internal/httpx"
	"github.com/shak0x90/velora_backend/internal/safety"
	"net/http"
	"strings"
)

// Resolve a conversation-local identity on the server, including before reveal.
func (s *Service) handleSafety(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	var in struct {
		Action    string `json:"action"`
		Reason    string `json:"reason"`
		Detail    string `json:"detail"`
		MessageID string `json:"messageId"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	c, err := s.access(r.Context(), tx, user(r), r.PathValue("id"), false)
	if err != nil {
		tx.Rollback(r.Context())
		httpx.Error(w, r, err)
		return
	}
	detail := strings.TrimSpace(in.Detail)
	if len([]rune(detail)) > 500 {
		tx.Rollback(r.Context())
		httpx.Error(w, r, httpx.BadRequest("Keep the explanation under 500 characters."))
		return
	}
	if in.MessageID != "" {
		var body string
		err = tx.QueryRow(r.Context(), `select body from messages where id::text=$1 and conversation_id=$2 and sender_id<>$3`, in.MessageID, c.ID, user(r)).Scan(&body)
		if err != nil {
			tx.Rollback(r.Context())
			httpx.Error(w, r, httpx.BadRequest("Choose a message from this person."))
			return
		}
		runes := []rune(body)
		if len(runes) > 1200 {
			runes = runes[:1200]
		}
		detail += "\nReported message " + in.MessageID + ": " + string(runes)
	}
	tx.Rollback(r.Context())
	svc := safety.New(s.pool, s.users)
	switch in.Action {
	case "block":
		err = svc.Block(r.Context(), user(r), c.peer(user(r)), "")
	case "report":
		allowed := map[string]bool{"harassment": true, "spam": true, "fake_profile": true, "inappropriate_photos": true, "underage": true, "offline_behaviour": true, "other": true}
		if !allowed[in.Reason] {
			httpx.Error(w, r, httpx.BadRequest("Choose a report reason."))
			return
		}
		err = svc.Report(r.Context(), user(r), safety.ReportInput{ProfileID: c.peer(user(r)), Reason: in.Reason, Detail: detail, Context: "message"})
	default:
		httpx.Error(w, r, httpx.BadRequest("Choose block or report."))
		return
	}
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 204, nil)
}
