package media

import (
	"context"
	"errors"
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

func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFrom(r.Context())
	id := r.PathValue("id")

	var key string
	// The user_id predicate is the authorization check: without it, any
	// authenticated caller could delete any photo by guessing an id.
	err := s.pool.QueryRow(r.Context(), `
		delete from profile_photos where id = $1 and user_id = $2 returning object_key
	`, id, userID).Scan(&key)
	if err != nil {
		httpx.Error(w, r, httpx.NotFound("That photo is not on your profile."))
		return
	}

	for _, variant := range AllVariants {
		// The row is already gone, so the photo is no longer shown. A few
		// orphaned files are better than failing a delete the user asked for.
		_ = s.store.Delete(r.Context(), KeyFor(key, variant))
	}

	// Close the gap the delete left, so positions stay 0..n-1.
	if _, err := s.pool.Exec(r.Context(), `
		with ordered as (
			select id, row_number() over (order by position) - 1 as new_position
			from profile_photos where user_id = $1
		)
		update profile_photos p set position = o.new_position
		from ordered o where p.id = o.id
	`, userID); err != nil {
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

	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	// Shift out of the way first. Position is unique per user, so assigning
	// the new values directly would collide with rows not yet moved.
	if _, err := tx.Exec(r.Context(),
		`update profile_photos set position = position + 100 where user_id = $1`,
		userID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	for index, id := range req.IDs {
		if _, err := tx.Exec(r.Context(), `
			update profile_photos set position = $1 where id = $2 and user_id = $3
		`, index, id, userID); err != nil {
			httpx.Error(w, r, err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
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
