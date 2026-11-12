// Package account is the end of an account's life: self-service deletion.
//
// Deletion keeps the users row as a tombstone and removes everything personal
// hanging off it. The row stays because other people's records point at it —
// their side of a conversation, the reports they filed — and a hard delete
// would cascade through those and erase what belongs to someone else.
//
// What is removed, kept, or redacted, and why:
//
//   - Profile, prompts, personality answers, photos (rows and stored files),
//     likes and passes in both directions, matches, notifications to or about
//     the account, push subscriptions, the anonymous queue entry, chat tickets,
//     refresh tokens, blocks the account made, and its sign-in identities.
//     Removing the identities frees the Google subject and email, so signing in
//     again later starts a new, empty account rather than reviving this one.
//   - Every open or pending conversation is closed through chatlog, exactly as
//     a block closes one, so the other person's client is told the thread is
//     closed instead of finding it silently broken.
//   - Messages the account sent are redacted: body cleared, deleted_at set. The
//     rows stay so each thread's sequence remains gap-free, which is what the
//     other person's client relies on to resync. The other person's own
//     messages are theirs and are left alone.
//   - Exception: messages in a conversation with someone who has an open or
//     in-review report against this account are kept intact until a moderator
//     resolves it. Deleting the account must not be a way to destroy the
//     evidence a report is about.
//   - Reports filed about the account, and by it, are kept for moderation. The
//     tombstone holds no personal data, so subject_id is already a pseudonym.
//   - Blocks other people placed on the account are kept; they are those
//     people's records.
package account

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/chat"
	"github.com/shak0x90/velora_backend/internal/chatlog"
	"github.com/shak0x90/velora_backend/internal/media"
)

// ErrNotActive means the account was already deleted or suspended by the time
// the deletion ran. Suspended accounts cannot delete themselves: that would be
// a way to shed a moderation record and start over.
var ErrNotActive = errors.New("account is not active")

type Service struct {
	pool       *pgxpool.Pool
	store      media.Store
	chatPhotos chat.PhotoStore
	guard      func(http.Handler) http.Handler
}

func New(pool *pgxpool.Pool, store media.Store, users *auth.Service) *Service {
	return &Service{pool: pool, store: store, guard: users.RequireAuth}
}

// Delete removes the account in one transaction, then the photo files.
//
// Files go after the commit because a store cannot take part in the
// transaction. A rolled-back deletion that had already removed files would
// leave a live profile full of broken images; the opposite failure — a few
// orphaned files after a committed deletion — is logged and harmless, since
// nothing references them any more.
func (s *Service) Delete(ctx context.Context, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Two deletions racing (a double tap, two devices) should not both run the
	// closure loop. The advisory lock serialises them without taking the user
	// row early, which would break chatlog's lock order.
	if _, err = tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('account-delete:' || $1, 0))`, userID); err != nil {
		return err
	}

	if err = closeConversations(ctx, tx, userID); err != nil {
		return err
	}

	// Status flips after the closures, not before: closing a thread already
	// takes user rows in chatlog's order, and locking ours up front would
	// invert it against a concurrent send.
	tag, err := tx.Exec(ctx, `
		update users set status = 'deleted', deleted_at = now(),
		       is_premium = false, is_moderator = false
		where id = $1 and status = 'active'
	`, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotActive
	}

	if _, err = tx.Exec(ctx, `
		update messages m set body = '', deleted_at = coalesce(m.deleted_at, now())
		from conversations c
		where m.conversation_id = c.id and m.sender_id = $1
		  and not exists (
		      select 1 from reports r
		      where r.subject_id = $1 and r.status in ('open', 'reviewing')
		        and r.reporter_id in (c.user_a, c.user_b))
	`, userID); err != nil {
		return err
	}

	keys, err := deletePhotoRows(ctx, tx, userID)
	if err != nil {
		return err
	}

	// Chat photos follow their messages: gone where the message was just
	// redacted (or was never sent), kept where an open report holds the
	// thread as evidence.
	rows, err := tx.Query(ctx, `
		delete from message_attachments a
		where a.uploader_id = $1
		  and (a.message_id is null
		       or exists (select 1 from messages m where m.id = a.message_id and m.deleted_at is not null))
		returning a.object_key
	`, userID)
	if err != nil {
		return err
	}
	chatPhotos, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}

	for _, statement := range purgeStatements {
		if _, err = tx.Exec(ctx, statement, userID); err != nil {
			return err
		}
	}

	// The same signal logout sends: the chat hub closes this user's sockets.
	if _, err = tx.Exec(ctx, `select pg_notify('velora_chat', 'logout:' || $1::text)`, userID); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return err
	}

	removeFiles(ctx, s.store, userID, keys)
	chat.RemovePhotoFiles(ctx, s.chatPhotos, chatPhotos)
	return nil
}

// WithChatPhotos lets deletion remove the account's chat photo files.
func (s *Service) WithChatPhotos(store chat.PhotoStore) *Service {
	s.chatPhotos = store
	return s
}

// purgeStatements remove every row that is the account's own data. Each takes
// the user id as $1. Nothing in the list references anything else in it, so
// the order is for reading, not for correctness.
var purgeStatements = []string{
	`delete from profile_prompts where user_id = $1`,
	`delete from personality_answers where user_id = $1`,
	`delete from profiles where user_id = $1`,
	`delete from likes where from_user_id = $1 or to_user_id = $1`,
	`delete from passes where user_id = $1 or target_id = $1`,
	`delete from matches where user_a = $1 or user_b = $1`,
	`delete from notifications where user_id = $1 or profile_id = $1`,
	`delete from blocks where user_id = $1`,
	`delete from reveal_consents where user_id = $1`,
	`delete from chat_push_subscriptions where user_id = $1`,
	`delete from devices where user_id = $1`,
	`delete from blind_hour_reminders where user_id = $1`,
	`delete from message_reactions where user_id = $1`,
	`delete from user_settings where user_id = $1`,
	`delete from daily_edits where user_id = $1 or profile_id = $1`,
	`delete from chat_queue where user_id = $1`,
	`delete from chat_tickets where user_id = $1`,
	`delete from user_events where user_id = $1`,
	`delete from refresh_tokens where user_id = $1`,
	`delete from auth_identities where user_id = $1`,
}

// closeConversations closes every live thread the account is in, one peer at
// a time, through the same path a block uses. Peers are visited in id order so
// two deletions that share a peer take the pair locks in the same order.
func closeConversations(ctx context.Context, tx pgx.Tx, userID string) error {
	rows, err := tx.Query(ctx, `
		select distinct (case when user_a = $1 then user_b else user_a end)::text as peer
		from conversations
		where (user_a = $1 or user_b = $1) and state in ('open', 'pending')
		order by peer
	`, userID)
	if err != nil {
		return err
	}
	peers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, peer := range peers {
		if err = chatlog.LockPair(ctx, tx, userID, peer); err != nil {
			return err
		}
		if err = chatlog.ClosePair(ctx, tx, userID, peer, false); err != nil {
			return err
		}
	}
	return nil
}

func deletePhotoRows(ctx context.Context, tx pgx.Tx, userID string) ([]string, error) {
	rows, err := tx.Query(ctx, `delete from profile_photos where user_id = $1 returning object_key`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// removeFiles deletes every variant of every photo. Failures are logged, not
// returned: the account is already gone, and the files are unreachable from
// any row.
func removeFiles(ctx context.Context, store media.Store, userID string, keys []string) {
	for _, key := range keys {
		for _, variant := range media.AllVariants {
			variantKey := media.KeyFor(key, variant)
			if err := store.Delete(ctx, variantKey); err != nil {
				slog.Warn("account deletion left a photo file behind",
					"user", userID, "key", variantKey, "error", err)
			}
		}
	}
}
