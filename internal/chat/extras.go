package chat

// Photos and reactions in chat.
//
// Photos: the client uploads first (POST /conversations/{id}/photos), gets an
// attachment id, then sends a message of kind "image" naming it. The upload
// is re-encoded (which strips EXIF, GPS included), stored outside the public
// media directory, and only ever served through GET
// /conversations/{id}/photos/{photoId}, which runs the same access check as
// reading the conversation. A blocked, closed or unmatched thread stops
// serving its photos the moment it stops serving its text.
//
// Reactions: one per person per message, from a fixed set. Setting or
// clearing one emits a message.edited event to both people, so open threads
// refetch and see it live.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/shak0x90/velora_backend/internal/chatlog"
	"github.com/shak0x90/velora_backend/internal/httpx"
	"github.com/shak0x90/velora_backend/internal/media"
)

// ReactionEmoji is the fixed set, matching the database check.
var ReactionEmoji = []string{"❤️", "😂", "😮", "😢", "😡", "👍"}

const (
	photoFullSide  = 1600
	photoThumbSide = 480
	// An upload nobody sent is swept after this long.
	unsentPhotoTTL = 24 * time.Hour
)

// PhotoStore keeps chat photo bytes. Keys are relative, slash-separated.
type PhotoStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Open(key string) (io.ReadSeekCloser, time.Time, error)
	Delete(ctx context.Context, key string) error
}

// DirStore is a PhotoStore on local disk. Root must not be a directory any
// web server publishes: privacy rests on these files being reachable only
// through the authenticated route.
type DirStore struct{ Root string }

func (d DirStore) path(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if strings.HasPrefix(key, "/") || strings.HasPrefix(key, `\`) || filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" || strings.HasPrefix(clean, "..") {
		return "", errors.New("invalid photo key")
	}
	return filepath.Join(d.Root, clean), nil
}

func (d DirStore) Put(_ context.Context, key string, data []byte) error {
	full, err := d.path(key)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("create chat photo dir: %w", err)
	}
	tmp := full + ".tmp"
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write chat photo: %w", err)
	}
	if err = os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("commit chat photo: %w", err)
	}
	return nil
}

func (d DirStore) Open(key string) (io.ReadSeekCloser, time.Time, error) {
	full, err := d.path(key)
	if err != nil {
		return nil, time.Time{}, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, time.Time{}, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, time.Time{}, err
	}
	return f, info.ModTime(), nil
}

func (d DirStore) Delete(_ context.Context, key string) error {
	full, err := d.path(key)
	if err != nil {
		return err
	}
	if err = os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// WithPhotos turns on chat photos, stored in store.
func (s *Service) WithPhotos(store PhotoStore) *Service {
	s.photos = store
	return s
}

func photoKey(base, size string) string { return base + "-" + size + ".jpg" }

// RemovePhotoFiles deletes both sizes of each stored photo. Failures are
// logged rather than returned: the rows are already gone, and a stray file
// is unreachable without one.
func RemovePhotoFiles(ctx context.Context, store PhotoStore, bases []string) {
	if store == nil {
		return
	}
	for _, base := range bases {
		for _, size := range []string{"full", "thumb"} {
			if err := store.Delete(ctx, photoKey(base, size)); err != nil {
				slog.Warn("chat photo delete failed", "key", photoKey(base, size), "error", err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Reactions
// ---------------------------------------------------------------------------

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// withReactions fills Reactions on msgs, as seen by user.
func withReactions(ctx context.Context, q querier, user string, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	index := make(map[string]int, len(msgs))
	ids := make([]string, 0, len(msgs))
	for i := range msgs {
		msgs[i].Reactions = []Reaction{}
		if msgs[i].DeletedAt == nil {
			index[msgs[i].ID] = i
			ids = append(ids, msgs[i].ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `select message_id::text, emoji, count(*)::int, bool_or(user_id = $1)
		from message_reactions where message_id = any($2::uuid[])
		group by message_id, emoji order by message_id, min(created_at)`, user, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r Reaction
		if err = rows.Scan(&id, &r.Emoji, &r.Count, &r.Mine); err != nil {
			return err
		}
		if i, ok := index[id]; ok {
			msgs[i].Reactions = append(msgs[i].Reactions, r)
		}
	}
	return rows.Err()
}

func validEmoji(e string) bool {
	for _, ok := range ReactionEmoji {
		if e == ok {
			return true
		}
	}
	return false
}

// React sets (emoji != "") or clears (emoji == "") the user's reaction.
func (s *Service) React(ctx context.Context, user, id, messageID, emoji string) (Message, error) {
	if emoji != "" && !validEmoji(emoji) {
		return Message{}, httpx.BadRequest("React with one of " + strings.Join(ReactionEmoji, " ") + ".")
	}
	if _, err := uuid.Parse(messageID); err != nil {
		return Message{}, httpx.NotFound("That message is not available.")
	}
	if ok, _ := s.reactLimit.Allow(user); !ok {
		return Message{}, httpx.TooManyRequests("Please slow down.")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback(ctx)
	c, err := s.access(ctx, tx, user, id, true)
	if err != nil {
		return Message{}, err
	}
	if c.State != "open" {
		return Message{}, httpx.Forbidden("Reactions work once the chat is open.")
	}
	var live bool
	if err = tx.QueryRow(ctx, `select exists(select 1 from messages where id=$1 and conversation_id=$2 and deleted_at is null)`, messageID, c.ID).Scan(&live); err != nil {
		return Message{}, err
	}
	if !live {
		return Message{}, httpx.NotFound("That message is not available.")
	}
	var changed int64
	if emoji == "" {
		tag, err := tx.Exec(ctx, `delete from message_reactions where message_id=$1 and user_id=$2`, messageID, user)
		if err != nil {
			return Message{}, err
		}
		changed = tag.RowsAffected()
	} else {
		tag, err := tx.Exec(ctx, `insert into message_reactions(message_id,user_id,emoji) values($1,$2,$3)
			on conflict (message_id,user_id) do update set emoji=excluded.emoji, created_at=now()
			where message_reactions.emoji <> excluded.emoji`, messageID, user, emoji)
		if err != nil {
			return Message{}, err
		}
		changed = tag.RowsAffected()
	}
	if changed > 0 {
		if err = chatlog.Emit(ctx, tx, c.A, c.B, c.ID, "message.edited", map[string]string{"messageId": messageID}); err != nil {
			return Message{}, err
		}
	}
	m, err := readMessage(ctx, tx, user, `m.id=$2`, messageID)
	if err != nil {
		return Message{}, err
	}
	return m, tx.Commit(ctx)
}

func (s *Service) handleReact(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	emoji := ""
	if r.Method == http.MethodPut {
		var in struct {
			Emoji string `json:"emoji"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			httpx.Error(w, r, err)
			return
		}
		if in.Emoji == "" {
			httpx.Error(w, r, httpx.BadRequest("Choose an emoji, or DELETE to remove your reaction."))
			return
		}
		emoji = in.Emoji
	}
	m, err := s.React(r.Context(), user(r), r.PathValue("id"), r.PathValue("messageId"), emoji)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, m)
}

// ---------------------------------------------------------------------------
// Photos
// ---------------------------------------------------------------------------

var photosOff = httpx.Problem{
	Type: "about:blank#photos-unavailable", Title: "Photos unavailable",
	Status: http.StatusServiceUnavailable, Detail: "Photos can't be sent right now.",
}

// UploadPhoto stores a photo for the conversation and returns its attachment.
func (s *Service) UploadPhoto(ctx context.Context, user, id string, raw []byte) (Attachment, error) {
	if s.photos == nil {
		return Attachment{}, photosOff
	}
	if ok, _ := s.photoLimit.Allow(user); !ok {
		return Attachment{}, httpx.TooManyRequests("That's a lot of photos. Try again in a little while.")
	}
	// Check access before spending CPU on the image.
	if err := s.checkPhotoAccess(ctx, user, id); err != nil {
		return Attachment{}, err
	}
	img, err := media.Decode(raw)
	if err != nil {
		if errors.Is(err, media.ErrUnsupportedImage) {
			return Attachment{}, httpx.BadRequest("Send a JPEG, PNG or WebP photo.")
		}
		return Attachment{}, httpx.BadRequest(err.Error())
	}
	full, w, h, err := media.EncodeJPEG(img, photoFullSide)
	if err != nil {
		return Attachment{}, err
	}
	thumb, _, _, err := media.EncodeJPEG(img, photoThumbSide)
	if err != nil {
		return Attachment{}, err
	}
	photoID := uuid.NewString()
	base := id + "/" + photoID
	// Bytes before the row, as for profile photos: a file without a row is
	// unreachable; a row without a file is a broken image.
	if err = s.photos.Put(ctx, photoKey(base, "full"), full); err != nil {
		return Attachment{}, err
	}
	if err = s.photos.Put(ctx, photoKey(base, "thumb"), thumb); err != nil {
		return Attachment{}, err
	}
	if _, err = s.pool.Exec(ctx, `insert into message_attachments(id,conversation_id,uploader_id,object_key,width,height) values($1,$2,$3,$4,$5,$6)`,
		photoID, id, user, base, w, h); err != nil {
		RemovePhotoFiles(ctx, s.photos, []string{base})
		return Attachment{}, err
	}
	return Attachment{ID: photoID, Width: w, Height: h}, nil
}

func (s *Service) checkPhotoAccess(ctx context.Context, user, id string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	c, err := s.access(ctx, tx, user, id, false)
	if err != nil {
		return err
	}
	if c.State != "open" {
		return httpx.Forbidden("Photos can be sent once the chat is open.")
	}
	return nil
}

func (s *Service) handleUploadPhoto(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, media.MaxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(media.MaxUploadBytes); err != nil {
		httpx.Error(w, r, httpx.BadRequest("Could not read the upload. Maximum size is 10 MB."))
		return
	}
	file, _, err := r.FormFile("photo")
	if err != nil {
		httpx.Error(w, r, httpx.BadRequest("Attach the image as the 'photo' field."))
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, media.MaxUploadBytes+1))
	if err != nil {
		httpx.Error(w, r, httpx.BadRequest("Could not read the upload."))
		return
	}
	att, err := s.UploadPhoto(r.Context(), user(r), r.PathValue("id"), raw)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, att)
}

// photoBase finds the stored key for a photo the user may see: one sent in a
// message that is not deleted, or their own upload that is not sent yet.
func (s *Service) photoBase(ctx context.Context, user, id, photoID string) (string, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err = s.access(ctx, tx, user, id, false); err != nil {
		return "", err
	}
	var base string
	err = tx.QueryRow(ctx, `select a.object_key from message_attachments a
		left join messages m on m.id = a.message_id
		where a.id=$1 and a.conversation_id=$2
		  and ((a.message_id is not null and m.deleted_at is null) or (a.message_id is null and a.uploader_id=$3))`,
		photoID, id, user).Scan(&base)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", httpx.NotFound("That photo is not available.")
	}
	return base, err
}

func (s *Service) handleServePhoto(w http.ResponseWriter, r *http.Request) {
	if !validID(w, r) {
		return
	}
	if s.photos == nil {
		httpx.Error(w, r, photosOff)
		return
	}
	photoID := r.PathValue("photoId")
	if _, err := uuid.Parse(photoID); err != nil {
		httpx.Error(w, r, httpx.NotFound("That photo is not available."))
		return
	}
	size := r.URL.Query().Get("size")
	if size == "" {
		size = "full"
	}
	if size != "full" && size != "thumb" {
		httpx.Error(w, r, httpx.BadRequest("size is full or thumb."))
		return
	}
	base, err := s.photoBase(r.Context(), user(r), r.PathValue("id"), photoID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	f, mod, err := s.photos.Open(photoKey(base, size))
	if err != nil {
		slog.Warn("chat photo missing", "key", photoKey(base, size), "error", err)
		httpx.Error(w, r, httpx.NotFound("That photo is not available."))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Private: browsers may keep it, shared caches and CDNs may not.
	w.Header().Set("Cache-Control", "private, max-age=604800, immutable")
	http.ServeContent(w, r, "", mod, f)
}

// pruneUnsentPhotos removes uploads that were never attached to a message.
func (s *Service) pruneUnsentPhotos(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `delete from message_attachments where message_id is null and created_at < $1 returning object_key`,
		time.Now().Add(-unsentPhotoTTL))
	if err != nil {
		slog.Warn("chat photo prune failed", "error", err)
		return
	}
	bases, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		slog.Warn("chat photo prune failed", "error", err)
		return
	}
	RemovePhotoFiles(ctx, s.photos, bases)
	if len(bases) > 0 {
		slog.Info("pruned unsent chat photos", "count", len(bases))
	}
}
