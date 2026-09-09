package chat

import (
	"github.com/shak0x90/velora_backend/internal/httpx"
	"time"
)

var unavailable = httpx.NotFound("This conversation is not available.")
var forbidden = httpx.Forbidden("You cannot send messages in this conversation.")

type Message struct {
	ID              string     `json:"id"`
	ConversationID  string     `json:"conversationId"`
	Seq             int64      `json:"seq,string"`
	ClientMessageID string     `json:"clientMessageId"`
	SenderID        string     `json:"senderParticipantId"`
	Mine            bool       `json:"mine"`
	Body            string     `json:"body"`
	Kind            string     `json:"kind"`
	ReplyToID       *string    `json:"replyToId,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	DeletedAt       *time.Time `json:"deletedAt,omitempty"`
}
type Conversation struct {
	ID               string    `json:"id"`
	Origin           string    `json:"origin"`
	State            string    `json:"state"`
	ProfileID        string    `json:"profileId,omitempty"`
	Name             string    `json:"name"`
	ParticipantID    string    `json:"participantId"`
	LastSeq          int64     `json:"lastSeq,string"`
	LastReadSeq      int64     `json:"lastReadSeq,string"`
	PeerReadSeq      int64     `json:"peerReadSeq,string"`
	PeerDeliveredSeq int64     `json:"peerDeliveredSeq,string"`
	UnreadCount      int       `json:"unreadCount"`
	Preview          string    `json:"preview"`
	UpdatedAt        time.Time `json:"updatedAt"`
	Muted            bool      `json:"muted"`
	Archived         bool      `json:"archived"`
	CanSend          bool      `json:"canSend"`
	CanAccept        bool      `json:"canAccept"`
	IdentityHidden   bool      `json:"identityHidden"`
	RevealConsented  bool      `json:"revealConsented"`
}
type SendInput struct {
	ClientMessageID string  `json:"clientMessageId"`
	Body            string  `json:"body"`
	Kind            string  `json:"kind"`
	ReplyToID       *string `json:"replyToId"`
}
type Event struct {
	Cursor         int64  `json:"cursor,string"`
	ConversationID string `json:"conversationId"`
	Kind           string `json:"kind"`
}
type EventPage struct {
	Events  []Event `json:"events"`
	Cursor  int64   `json:"cursor,string"`
	HasMore bool    `json:"hasMore"`
}
type History struct {
	Messages []Message `json:"messages"`
	HasMore  bool      `json:"hasMore"`
}
type Snapshot struct {
	Conversations []Conversation `json:"conversations"`
	Cursor        int64          `json:"cursor,string"`
	UnreadCount   int            `json:"unreadCount"`
	NextOffset    int            `json:"nextOffset,omitempty"`
}
