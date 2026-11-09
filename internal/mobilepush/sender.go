package mobilepush

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Message is one push to one device, already decided on: the worker has
// checked preferences and suppression before it builds one.
type Message struct {
	Token string
	Title string
	Body  string
	// Data is delivered to the app alongside the alert, for routing a tap.
	Data map[string]string
	// Collapse lets a newer push replace an older one still on the lock
	// screen. At most 64 bytes.
	Collapse string
}

// Sender delivers a Message through one provider.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

var (
	// ErrUnregistered means the provider says this token will never work
	// again (app uninstalled, token rotated). The device row is deleted.
	ErrUnregistered = errors.New("push token is no longer registered")
	// ErrPermanent is a rejection retrying cannot fix, such as a payload the
	// provider refuses. The job is cancelled rather than retried.
	ErrPermanent = errors.New("push rejected by provider")
)

// Noop is the sender for a platform with no credentials configured. It
// delivers nothing and says so at debug level.
type Noop struct{ Platform string }

func (n Noop) Send(_ context.Context, m Message) error {
	slog.Debug("push not configured; dropping", "platform", n.Platform, "title", m.Title)
	return nil
}

// Router picks the sender for a device's platform. A nil entry means that
// platform is not configured: Enqueue skips its devices entirely, so an
// unconfigured server does not fill the job table with work that goes
// nowhere.
type Router struct {
	Android Sender
	IOS     Sender
}

// For returns the platform's sender, or Noop and false when it has none.
func (r Router) For(platform string) (Sender, bool) {
	var s Sender
	switch platform {
	case PlatformAndroid:
		s = r.Android
	case PlatformIOS:
		s = r.IOS
	}
	if s == nil {
		return Noop{Platform: platform}, false
	}
	return s, true
}

// Any reports whether any platform can actually deliver.
func (r Router) Any() bool { return r.Android != nil || r.IOS != nil }

// Config is read from the environment by package config.
type Config struct {
	// FCM HTTP v1. The credentials are a service-account JSON key with the
	// Firebase Messaging role; ProjectID defaults to the key's project.
	FCMProjectID       string
	FCMCredentialsFile string
	// APNs token auth (.p8 key). Topic is the iOS bundle id. When these are
	// unset, iOS devices go through FCM instead — which is what an app built
	// with firebase_messaging registers anyway.
	APNsKeyFile    string
	APNsKeyID      string
	APNsTeamID     string
	APNsTopic      string
	APNsProduction bool
}

// NewRouter builds senders for whatever is configured. Half-configured
// providers are an error rather than a silent no-op: someone who set three of
// the four APNs values meant to turn it on.
func NewRouter(ctx context.Context, cfg Config) (Router, error) {
	var r Router
	if cfg.FCMCredentialsFile != "" {
		raw, err := readFile(cfg.FCMCredentialsFile)
		if err != nil {
			return r, fmt.Errorf("FCM_CREDENTIALS_FILE: %w", err)
		}
		fcm, err := NewFCM(ctx, cfg.FCMProjectID, raw)
		if err != nil {
			return r, err
		}
		r.Android, r.IOS = fcm, fcm
	} else if cfg.FCMProjectID != "" {
		return r, errors.New("FCM_PROJECT_ID is set but FCM_CREDENTIALS_FILE is not")
	}

	apnsSet := []string{cfg.APNsKeyFile, cfg.APNsKeyID, cfg.APNsTeamID, cfg.APNsTopic}
	n := 0
	for _, v := range apnsSet {
		if v != "" {
			n++
		}
	}
	switch n {
	case 0:
	case len(apnsSet):
		raw, err := readFile(cfg.APNsKeyFile)
		if err != nil {
			return r, fmt.Errorf("APNS_KEY_FILE: %w", err)
		}
		apns, err := NewAPNs(raw, cfg.APNsKeyID, cfg.APNsTeamID, cfg.APNsTopic, cfg.APNsProduction)
		if err != nil {
			return r, err
		}
		r.IOS = apns
	default:
		return r, errors.New("APNs needs all of APNS_KEY_FILE, APNS_KEY_ID, APNS_TEAM_ID and APNS_TOPIC")
	}
	return r, nil
}

// readFile is a variable so tests can feed keys without touching disk.
var readFile = os.ReadFile

// ---------------------------------------------------------------------------
// FCM (HTTP v1)
// ---------------------------------------------------------------------------

const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

type FCM struct {
	project string
	client  *http.Client
	base    string
}

// NewFCM authenticates with a service-account key. The OAuth token is cached
// and refreshed by the oauth2 transport.
func NewFCM(ctx context.Context, projectID string, credentialsJSON []byte) (*FCM, error) {
	creds, err := google.CredentialsFromJSONWithType(ctx, credentialsJSON, google.ServiceAccount, fcmScope)
	if err != nil {
		return nil, fmt.Errorf("FCM credentials: %w", err)
	}
	if projectID == "" {
		projectID = creds.ProjectID
	}
	if projectID == "" {
		return nil, errors.New("FCM_PROJECT_ID is empty and the key names no project")
	}
	// Background context: the token source outlives the startup call.
	client := oauth2.NewClient(context.Background(), creds.TokenSource)
	client.Timeout = 10 * time.Second
	return newFCM(projectID, client, "https://fcm.googleapis.com"), nil
}

func newFCM(project string, client *http.Client, base string) *FCM {
	return &FCM{project: project, client: client, base: base}
}

func (f *FCM) Send(ctx context.Context, m Message) error {
	android := map[string]any{"priority": "high"}
	apnsHeaders := map[string]string{}
	if m.Collapse != "" {
		android["collapse_key"] = m.Collapse
		apnsHeaders["apns-collapse-id"] = m.Collapse
	}
	msg := map[string]any{
		"token":        m.Token,
		"notification": map[string]string{"title": m.Title, "body": m.Body},
		"android":      android,
		"apns": map[string]any{
			"headers": apnsHeaders,
			"payload": map[string]any{"aps": map[string]any{"sound": "default"}},
		},
	}
	if len(m.Data) > 0 {
		msg["data"] = m.Data
	}
	body, err := json.Marshal(map[string]any{"message": msg})
	if err != nil {
		return err
	}
	endpoint := f.base + "/v1/projects/" + url.PathEscape(f.project) + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("fcm: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	var problem struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&problem)
	code := problem.Error.Status
	for _, d := range problem.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
		}
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || code == "UNREGISTERED":
		return ErrUnregistered
	case code == "SENDER_ID_MISMATCH":
		// The token belongs to a different Firebase project; it will never
		// work with this one.
		return ErrUnregistered
	case resp.StatusCode == http.StatusBadRequest || code == "INVALID_ARGUMENT":
		return fmt.Errorf("%w: fcm %d %s", ErrPermanent, resp.StatusCode, code)
	default:
		// 401/403 (credentials), 429 (quota) and 5xx are worth retrying.
		return fmt.Errorf("fcm status %d %s", resp.StatusCode, code)
	}
}

// ---------------------------------------------------------------------------
// APNs (token-based, HTTP/2)
// ---------------------------------------------------------------------------

// apnsTokenLife is under Apple's one-hour limit and over its 20-minute
// minimum between refreshes.
const apnsTokenLife = 50 * time.Minute

type APNs struct {
	key    *ecdsa.PrivateKey
	keyID  string
	teamID string
	topic  string
	base   string
	client *http.Client
	now    func() time.Time

	mu     sync.Mutex
	jwt    string
	issued time.Time
}

// NewAPNs parses an Apple .p8 signing key. Go's client negotiates HTTP/2 over
// TLS on its own, which is all APNs needs.
func NewAPNs(p8 []byte, keyID, teamID, topic string, production bool) (*APNs, error) {
	block, _ := pem.Decode(p8)
	if block == nil {
		return nil, errors.New("APNs key: not a PEM file")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("APNs key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("APNs key: not an EC key")
	}
	base := "https://api.sandbox.push.apple.com"
	if production {
		base = "https://api.push.apple.com"
	}
	return &APNs{
		key: key, keyID: keyID, teamID: teamID, topic: topic, base: base,
		client: &http.Client{Timeout: 10 * time.Second},
		now:    time.Now,
	}, nil
}

func (a *APNs) bearer(forceNew bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if !forceNew && a.jwt != "" && now.Sub(a.issued) < apnsTokenLife {
		return a.jwt, nil
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": a.teamID, "iat": now.Unix()})
	t.Header["kid"] = a.keyID
	signed, err := t.SignedString(a.key)
	if err != nil {
		return "", err
	}
	a.jwt, a.issued = signed, now
	return signed, nil
}

func (a *APNs) Send(ctx context.Context, m Message) error {
	payload := map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{"title": m.Title, "body": m.Body},
			"sound": "default",
		},
	}
	for k, v := range m.Data {
		if k != "aps" {
			payload[k] = v
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	err = a.post(ctx, m, body, false)
	if errors.Is(err, errExpiredToken) {
		err = a.post(ctx, m, body, true)
	}
	return err
}

var errExpiredToken = errors.New("apns provider token expired")

func (a *APNs) post(ctx context.Context, m Message, body []byte, freshToken bool) error {
	bearer, err := a.bearer(freshToken)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.base+"/3/device/"+url.PathEscape(m.Token), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "bearer "+bearer)
	req.Header.Set("apns-topic", a.topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	if m.Collapse != "" {
		req.Header.Set("apns-collapse-id", m.Collapse)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("apns: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var problem struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&problem)
	switch {
	case resp.StatusCode == http.StatusGone,
		problem.Reason == "BadDeviceToken", problem.Reason == "DeviceTokenNotForTopic",
		problem.Reason == "Unregistered":
		return ErrUnregistered
	case problem.Reason == "ExpiredProviderToken" && !freshToken:
		return errExpiredToken
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge:
		return fmt.Errorf("%w: apns %d %s", ErrPermanent, resp.StatusCode, problem.Reason)
	default:
		return fmt.Errorf("apns status %d %s", resp.StatusCode, problem.Reason)
	}
}
