package mobilepush

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/httpx"
)

// Device is what the app registers after the OS hands it a push token.
type Device struct {
	Platform   string `json:"platform"`
	Token      string `json:"token"`
	AppVersion string `json:"appVersion"`
}

// DeviceStore is the persistence the device handlers need; tests replace it.
type DeviceStore interface {
	// Register saves the device and returns how many recent pushes to it
	// were never confirmed by the app (see push_receipts).
	Register(ctx context.Context, userID string, d Device) (missed int, err error)
	Unregister(ctx context.Context, userID, token string) error
	// Ack records that the app received push id. Unknown ids are ignored.
	Ack(ctx context.Context, id uuid.UUID) error
}

// Registration is the POST /devices answer.
type Registration struct {
	// MissedPushes counts pushes from the last 7 days that the provider
	// accepted but this phone never confirmed, since its last confirmed one.
	// Above zero, the phone is dropping them (battery manager, app killed),
	// and the app asks the person to allow it in the background.
	MissedPushes int `json:"missedPushes"`
}

type pgDevices struct{ pool *pgxpool.Pool }

// Register is an upsert on the token. A token is one install, so if it is
// already on file under another account the row moves here: the phone has
// signed into a different account, and the previous one must stop receiving
// pushes on it.
func (p pgDevices) Register(ctx context.Context, userID string, d Device) (int, error) {
	var deviceID string
	err := p.pool.QueryRow(ctx, `
		insert into devices (id, user_id, platform, token, app_version)
		values ($1, $2, $3, $4, $5)
		on conflict (token) do update set
			user_id = excluded.user_id, platform = excluded.platform,
			app_version = excluded.app_version, last_seen_at = now()
		returning id::text
	`, uuid.New(), userID, d.Platform, d.Token, d.AppVersion).Scan(&deviceID)
	if err != nil {
		return 0, err
	}
	// Two minutes of grace for a push still on its way; nothing older than a
	// week; and only since the last push the phone did confirm, so fixing the
	// settings clears the count at the next confirmed push.
	var missed int
	err = p.pool.QueryRow(ctx, `
		select count(*) from push_receipts r
		where r.device_id = $1 and r.received_at is null
		  and r.sent_at < now() - interval '2 minutes'
		  and r.sent_at > now() - interval '7 days'
		  and r.sent_at > coalesce(
		        (select max(sent_at) from push_receipts where device_id = $1 and received_at is not null),
		        '-infinity')
	`, deviceID).Scan(&missed)
	return missed, err
}

func (p pgDevices) Ack(ctx context.Context, id uuid.UUID) error {
	_, err := p.pool.Exec(ctx, `update push_receipts set received_at = now() where id = $1 and received_at is null`, id)
	return err
}

// Unregister is scoped to the caller, so nobody can switch off someone
// else's pushes by guessing a token.
func (p pgDevices) Unregister(ctx context.Context, userID, token string) error {
	_, err := p.pool.Exec(ctx, `delete from devices where user_id = $1 and token = $2`, userID, token)
	return err
}

type Devices struct {
	store DeviceStore
	guard func(http.Handler) http.Handler
}

func NewDevices(pool *pgxpool.Pool, users *auth.Service) *Devices {
	return &Devices{store: pgDevices{pool: pool}, guard: users.RequireAuth}
}

// Routes registers POST /devices, DELETE /devices/{token} and POST /push/ack.
func (d *Devices) Routes(mux *http.ServeMux) {
	mux.Handle("POST /devices", d.guard(http.HandlerFunc(d.handleRegister)))
	mux.Handle("DELETE /devices/{token}", d.guard(http.HandlerFunc(d.handleUnregister)))
	// No sign-in: the app confirms from a background handler that may have
	// no fresh access token, and the random receipt id is the credential.
	mux.HandleFunc("POST /push/ack", d.handleAck)
}

// handleAck answers 204 for any well-formed id, known or not, so it reveals
// nothing about which pushes exist.
func (d *Devices) handleAck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(in.ID))
	if err != nil {
		httpx.Error(w, r, httpx.BadRequest("id must be a push id."))
		return
	}
	if err := d.store.Ack(r.Context(), id); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

const (
	maxTokenLength      = 4096
	maxAppVersionLength = 64
)

func (d *Devices) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in Device
	if err := httpx.Decode(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in.Platform = strings.ToLower(strings.TrimSpace(in.Platform))
	in.Token = strings.TrimSpace(in.Token)
	in.AppVersion = strings.TrimSpace(in.AppVersion)
	if err := validateDevice(in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	missed, err := d.store.Register(r.Context(), auth.UserIDFrom(r.Context()), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, Registration{MissedPushes: missed})
}

// handleUnregister answers 204 whether or not the token was on file: signing
// out twice, or after the worker already pruned a dead token, is not an error.
func (d *Devices) handleUnregister(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.PathValue("token"))
	if token == "" || len(token) > maxTokenLength {
		httpx.Error(w, r, httpx.BadRequest("A device token is required."))
		return
	}
	if err := d.store.Unregister(r.Context(), auth.UserIDFrom(r.Context()), token); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusNoContent, nil)
}

func validateDevice(in Device) error {
	switch {
	case in.Platform != PlatformAndroid && in.Platform != PlatformIOS:
		return httpx.BadRequest(`platform must be "android" or "ios".`)
	case in.Token == "":
		return httpx.BadRequest("token is required.")
	case len(in.Token) > maxTokenLength:
		return httpx.BadRequest("token is too long.")
	case strings.ContainsAny(in.Token, " \t\r\n/"):
		// Never valid in an FCM or APNs token, and a slash would make the
		// token unusable as the DELETE path segment.
		return httpx.BadRequest("token is not a push token.")
	case len(in.AppVersion) > maxAppVersionLength:
		return httpx.BadRequest("appVersion is too long.")
	}
	return nil
}
