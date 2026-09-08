package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

type Service struct {
	pool  *pgxpool.Pool
	store Store
	guard func(http.Handler) http.Handler
}

// New wires the service. guard is auth.RequireAuth — every photo route is
// scoped to the caller, so there is no route here that works unauthenticated.
func New(pool *pgxpool.Pool, store Store, guard func(http.Handler) http.Handler) *Service {
	return &Service{pool: pool, store: store, guard: guard}
}

func (s *Service) Routes(mux *http.ServeMux) {
	mux.Handle("GET /me/photos", s.guard(http.HandlerFunc(s.handleList)))
	mux.Handle("POST /me/photos", s.guard(http.HandlerFunc(s.handleUpload)))
	mux.Handle("DELETE /me/photos/{id}", s.guard(http.HandlerFunc(s.handleDelete)))
	mux.Handle("PATCH /me/photos/order", s.guard(http.HandlerFunc(s.handleReorder)))
}

// Photo is what the clients render. Every variant URL is given explicitly so
// the client picks a size rather than guessing or scaling a large one down.
type Photo struct {
	ID       string `json:"id"`
	Position int    `json:"position"`
	Status   string `json:"status"`
	Full     string `json:"full"`
	Card     string `json:"card"`
	Thumb    string `json:"thumb"`
}

const maxPhotosPerProfile = 6

// ErrUnknownPhoto means a caller referenced a photo URL that is not one of
// their own. It is a client-state problem, not a server one.
var ErrUnknownPhoto = errors.New("photo does not belong to this profile")

// URLs returns the caller's photos as card URLs in position order. This is the
// `photos` array a Profile carries: one URL per photo, at the size the grids
// actually render, so nothing downloads a 1080px hero to draw a tile.
func (s *Service) URLs(ctx context.Context, userID string) ([]string, error) {
	photos, err := s.list(ctx, userID)
	if err != nil {
		return nil, err
	}
	urls := make([]string, 0, len(photos))
	for _, p := range photos {
		urls = append(urls, p.Card)
	}
	return urls, nil
}

// URLsFor is the batched form of URLs, for a feed of candidates. One query for
// forty profiles rather than forty queries; the map is keyed by user id and
// omits anyone with no photos.
func (s *Service) URLsFor(ctx context.Context, userIDs []string) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `
		select user_id::text, object_key from profile_photos
		where user_id = any($1) order by user_id, position
	`, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var userID, key string
		if err := rows.Scan(&userID, &key); err != nil {
			return nil, err
		}
		out[userID] = append(out[userID], s.store.URL(KeyFor(key, VariantCard)))
	}
	return out, rows.Err()
}

// Reconcile makes the caller's photo set match the given URLs exactly: the
// listed photos take that order, and any photo left out is deleted.
//
// It exists so `PATCH /me` can carry the photo grid the user is looking at,
// which is how both clients model editing a profile. A URL matching none of
// the caller's photos aborts the whole thing with ErrUnknownPhoto rather than
// being skipped — on a stale list, quietly ignoring the unmatched entry would
// delete every photo the client did not know about.
func (s *Service) Reconcile(ctx context.Context, userID string, urls []string) error {
	photos, err := s.list(ctx, userID)
	if err != nil {
		return err
	}

	// Any variant URL identifies the photo, because a client holds whichever
	// size it happened to render.
	byURL := make(map[string]Photo, len(photos)*3)
	for _, p := range photos {
		byURL[p.Full] = p
		byURL[p.Card] = p
		byURL[p.Thumb] = p
	}

	keep := make([]string, 0, len(urls))
	seen := make(map[string]bool, len(urls))
	for _, url := range urls {
		p, ok := byURL[url]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownPhoto, url)
		}
		if seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		keep = append(keep, p.ID)
	}

	for _, p := range photos {
		if !seen[p.ID] {
			if err := s.deletePhoto(ctx, userID, p.ID); err != nil {
				return err
			}
		}
	}
	if len(keep) == 0 {
		return nil
	}
	return s.reorder(ctx, userID, keep)
}

func (s *Service) list(ctx context.Context, userID string) ([]Photo, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, position, status, object_key
		from profile_photos
		where user_id = $1
		order by position
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	photos := []Photo{}
	for rows.Next() {
		var p Photo
		var key string
		if err := rows.Scan(&p.ID, &p.Position, &p.Status, &key); err != nil {
			return nil, err
		}
		p.Full = s.store.URL(KeyFor(key, VariantFull))
		p.Card = s.store.URL(KeyFor(key, VariantCard))
		p.Thumb = s.store.URL(KeyFor(key, VariantThumb))
		photos = append(photos, p)
	}
	return photos, rows.Err()
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	photos, err := s.list(r.Context(), auth.UserIDFrom(r.Context()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"photos": photos})
}

func (s *Service) handleUpload(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFrom(r.Context())

	// Cap the request body before parsing, not after: without this a large
	// upload is already in memory by the time we could reject it.
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(MaxUploadBytes); err != nil {
		httpx.Error(w, r, httpx.BadRequest("Could not read the upload. Maximum size is 10 MB."))
		return
	}

	file, _, err := r.FormFile("photo")
	if err != nil {
		httpx.Error(w, r, httpx.BadRequest("Attach the image as the 'photo' field."))
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, MaxUploadBytes+1))
	if err != nil {
		httpx.Error(w, r, httpx.BadRequest("Could not read the upload."))
		return
	}

	var count int
	if err := s.pool.QueryRow(r.Context(),
		`select count(*) from profile_photos where user_id = $1`, userID).Scan(&count); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if count >= maxPhotosPerProfile {
		httpx.Error(w, r, httpx.BadRequest("You can have at most 6 photos. Remove one first."))
		return
	}

	derived, err := Process(userID, raw)
	if err != nil {
		if errors.Is(err, ErrUnsupportedImage) {
			httpx.Error(w, r, httpx.BadRequest(err.Error()))
			return
		}
		httpx.Error(w, r, httpx.BadRequest(err.Error()))
		return
	}

	// Store bytes before the row. A file with no row is invisible clutter; a
	// row pointing at a missing file is a broken image in someone's profile.
	for _, d := range derived {
		if err := s.store.Put(r.Context(), d.Key, d.Data, d.ContentType); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}

	photoID := uuid.New()
	if _, err := s.pool.Exec(r.Context(), `
		insert into profile_photos (id, user_id, position, object_key, status)
		values ($1, $2, $3, $4, 'ready')
	`, photoID, userID, count, derived[0].Key); err != nil {
		httpx.Error(w, r, err)
		return
	}

	photos, err := s.list(r.Context(), userID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"photos": photos})
}

// deletePhoto removes one photo and closes the gap it leaves in the ordering.
// ErrUnknownPhoto means the id is not the caller's.
func (s *Service) deletePhoto(ctx context.Context, userID, id string) error {
	var key string
	// The user_id predicate is the authorization check: without it, any
	// authenticated caller could delete any photo by guessing an id.
	err := s.pool.QueryRow(ctx, `
		delete from profile_photos where id = $1 and user_id = $2 returning object_key
	`, id, userID).Scan(&key)
	if err != nil {
		return ErrUnknownPhoto
	}

	for _, variant := range AllVariants {
		// The row is already gone, so the photo is no longer shown. A few
		// orphaned files are better than failing a delete the user asked for.
		_ = s.store.Delete(ctx, KeyFor(key, variant))
	}

	// Close the gap the delete left, so positions stay 0..n-1.
	_, err = s.pool.Exec(ctx, `
		with ordered as (
			select id, row_number() over (order by position) - 1 as new_position
			from profile_photos where user_id = $1
		)
		update profile_photos p set position = o.new_position
		from ordered o where p.id = o.id
	`, userID)
	return err
}

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFrom(r.Context())

	if err := s.deletePhoto(r.Context(), userID, r.PathValue("id")); err != nil {
		if errors.Is(err, ErrUnknownPhoto) {
			httpx.Error(w, r, httpx.NotFound("That photo is not on your profile."))
			return
		}
		httpx.Error(w, r, err)
		return
	}

	photos, err := s.list(r.Context(), userID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"photos": photos})
}

type reorderRequest struct {
	// IDs in the order they should appear. First is the main photo.
	IDs []string `json:"ids"`
}

// reorder assigns positions 0..n-1 in the given id order.
func (s *Service) reorder(ctx context.Context, userID string, ids []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Shift out of the way first. Position is unique per user, so assigning
	// the new values directly would collide with rows not yet moved.
	if _, err := tx.Exec(ctx,
		`update profile_photos set position = position + 100 where user_id = $1`,
		userID); err != nil {
		return err
	}
	for index, id := range ids {
		if _, err := tx.Exec(ctx, `
			update profile_photos set position = $1 where id = $2 and user_id = $3
		`, index, id, userID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Service) handleReorder(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFrom(r.Context())

	var req reorderRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if len(req.IDs) == 0 {
		httpx.Error(w, r, httpx.BadRequest("Send the photo ids in their new order."))
		return
	}

	if err := s.reorder(r.Context(), userID, req.IDs); err != nil {
		httpx.Error(w, r, err)
		return
	}

	photos, err := s.list(r.Context(), userID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"photos": photos})
}
