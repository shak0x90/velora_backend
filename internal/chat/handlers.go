package chat

import (
	"github.com/google/uuid"
	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/httpx"
	"net/http"
	"strconv"
)

func (s *Service) Routes(mux *http.ServeMux) {
	guard := func(pattern string, f http.HandlerFunc) { mux.Handle(pattern, s.users.RequireAuth(f)) }
	guard("GET /chat/bootstrap", s.handleBootstrap)
	guard("GET /conversations", s.handleBootstrap)
	guard("POST /conversations", s.handleOpen)
	guard("GET /conversations/{id}/messages", s.handleHistory)
	guard("POST /conversations/{id}/messages", s.handleSend)
	guard("POST /conversations/{id}/read", s.handleReceipt(true))
	guard("POST /conversations/{id}/delivered", s.handleReceipt(false))
	guard("POST /conversations/{id}/actions", s.handleAction)
	guard("POST /conversations/{id}/safety", s.handleSafety)
	guard("GET /chat/events", s.handleEvents)
	guard("POST /chat/realtime-ticket", s.handleTicket)
	guard("POST /chat/anonymous", s.handleAnonymous)
	guard("DELETE /chat/anonymous", s.handleLeaveQueue)
	mux.HandleFunc("GET /chat/ws", s.handleSocket)
}
func user(r *http.Request) string { return auth.UserIDFrom(r.Context()) }
func validID(w http.ResponseWriter, r *http.Request) bool {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		httpx.Error(w, r, unavailable)
		return false
	}
	return true
}
func (s *Service) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 10000 {
			httpx.Error(w, r, httpx.BadRequest("Invalid inbox offset."))
			return
		}
		offset = n
	}
	out, err := s.Bootstrap(r.Context(), user(r), offset)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 200, out)
}
func (s *Service) handleOpen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProfileID string `json:"profileId"`
		Origin    string `json:"origin"`
		SendInput
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if _, err := uuid.Parse(in.ProfileID); err != nil {
		httpx.Error(w, r, httpx.BadRequest("Invalid profile ID."))
		return
	}
	id, err := s.Open(r.Context(), user(r), in.ProfileID, in.Origin, in.SendInput)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 200, map[string]string{"id": id})
}
func (s *Service) handleSend(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	var in SendInput
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	m, err := s.Send(r.Context(), user(r), r.PathValue("id"), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 201, m)
}
func (s *Service) handleHistory(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	q := r.URL.Query()
	after, before := int64(-1), int64(0)
	limit := 50
	for _, key := range []string{"afterSeq", "beforeSeq", "limit"} {
		if raw := q.Get(key); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || v < 0 {
				httpx.Error(w, r, httpx.BadRequest("Invalid pagination value."))
				return
			}
			switch key {
			case "afterSeq":
				after = v
			case "beforeSeq":
				before = v
			case "limit":
				if v < 1 || v > 100 {
					httpx.Error(w, r, httpx.BadRequest("Limit must be 1 to 100."))
					return
				}
				limit = int(v)
			}
		}
	}
	if after >= 0 && before > 0 {
		httpx.Error(w, r, httpx.BadRequest("Choose either beforeSeq or afterSeq."))
		return
	}
	out, err := s.History(r.Context(), user(r), r.PathValue("id"), after, before, limit)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 200, out)
}
func (s *Service) handleReceipt(read bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validID(w, r) {
			return
		}
		var in struct {
			Seq string `json:"seq"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			httpx.Error(w, r, err)
			return
		}
		seq, err := strconv.ParseInt(in.Seq, 10, 64)
		if err != nil {
			httpx.Error(w, r, httpx.BadRequest("seq must be a decimal string."))
			return
		}
		if err = s.Receipt(r.Context(), user(r), r.PathValue("id"), seq, read); err != nil {
			httpx.Error(w, r, err)
			return
		}
		httpx.JSON(w, 204, nil)
	}
}
func (s *Service) handleAction(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	var in struct {
		Action string `json:"action"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := s.Action(r.Context(), user(r), r.PathValue("id"), in.Action); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 204, nil)
}
func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		httpx.Error(w, r, httpx.BadRequest("Invalid event cursor."))
		return
	}
	out, err := s.Events(r.Context(), user(r), after)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, 200, out)
}
