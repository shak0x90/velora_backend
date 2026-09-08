package domain

import "time"

// Likes, matches, and notifications. Same rule as profile.go: these mirror the
// Dart and TypeScript models field for field, and the enum values are the
// exact Dart enum names so one JSON shape serves all three clients.

type LikeKind string

const (
	LikeProfile  LikeKind = "profile"
	LikePhoto    LikeKind = "photo"
	LikePrompt   LikeKind = "prompt"
	LikeInterest LikeKind = "interest"
)

// ProfileLike is one person expressing interest in another. Kind and Label say
// what was liked ("Liked your photo", "Liked your answer about Sundays"), which
// is what makes the likes screen readable rather than a wall of names.
type ProfileLike struct {
	ID            string    `json:"id"`
	FromProfileID string    `json:"fromProfileId"`
	ToProfileID   string    `json:"toProfileId"`
	Kind          LikeKind  `json:"kind"`
	Label         string    `json:"label"`
	CreatedAt     time.Time `json:"createdAt"`
	Priority      bool      `json:"priority,omitempty"`
}

// MatchRecord is a mutual like, told from one side: ProfileID is always the
// other person. The message fields stay zero until the messaging phase lands.
type MatchRecord struct {
	ID                 string    `json:"id"`
	ProfileID          string    `json:"profileId"`
	CreatedAt          time.Time `json:"createdAt"`
	LastMessagePreview string    `json:"lastMessagePreview"`
	UnreadCount        int       `json:"unreadCount"`
	IsNew              bool      `json:"isNew,omitempty"`
}

type NotificationKind string

const (
	NotifyLike       NotificationKind = "like"
	NotifyMatch      NotificationKind = "match"
	NotifyMessage    NotificationKind = "message"
	NotifyMatchmaker NotificationKind = "matchmaker"
	NotifyProfile    NotificationKind = "profile"
)

type AppNotification struct {
	ID        string           `json:"id"`
	Kind      NotificationKind `json:"kind"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	CreatedAt time.Time        `json:"createdAt"`
	ProfileID string           `json:"profileId,omitempty"`
	Read      bool             `json:"read"`
}

type SuggestionTone string

const (
	ToneOpener  SuggestionTone = "opener"
	TonePlayful SuggestionTone = "playful"
	ToneSincere SuggestionTone = "sincere"
	ToneDeeper  SuggestionTone = "deeper"
	ToneShared  SuggestionTone = "shared"
)

// ConversationSuggestion is a question the user may choose to send. It is
// never written on their behalf and never sent automatically — the client
// offers it, the person decides.
type ConversationSuggestion struct {
	Text   string         `json:"text"`
	Tone   SuggestionTone `json:"tone"`
	Reason string         `json:"reason"`
}
