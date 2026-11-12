// Command velora is the whole service: one binary, three modes.
//
//	velora serve     HTTP + WebSocket API
//	velora work      background jobs (browser and native push delivery)
//	velora migrate   apply database migrations, then exit
//
// Running the API and the worker from the same binary keeps them on one
// codebase and one deploy; they differ only in which goroutines start.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/shak0x90/velora_backend/internal/account"
	"github.com/shak0x90/velora_backend/internal/apidocs"
	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/blindhour"
	"github.com/shak0x90/velora_backend/internal/chat"
	"github.com/shak0x90/velora_backend/internal/chatpush"
	"github.com/shak0x90/velora_backend/internal/config"
	"github.com/shak0x90/velora_backend/internal/db"
	"github.com/shak0x90/velora_backend/internal/discovery"
	"github.com/shak0x90/velora_backend/internal/httpx"
	"github.com/shak0x90/velora_backend/internal/media"
	"github.com/shak0x90/velora_backend/internal/mobilepush"
	"github.com/shak0x90/velora_backend/internal/profile"
	"github.com/shak0x90/velora_backend/internal/safety"
	"github.com/shak0x90/velora_backend/internal/settings"
	"github.com/shak0x90/velora_backend/internal/social"

	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging(cfg)

	switch command {
	case "serve":
		return serve(cfg)
	case "work":
		return work(cfg)
	case "migrate":
		return migrate(cfg)
	case "moderator":
		return moderator(cfg, os.Args[2:])
	case "push-test":
		return pushTest(cfg, os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `velora — the Velora API

Usage:
  velora serve     Start the HTTP and WebSocket API
  velora work      Start the background job worker
  velora migrate   Apply database migrations and exit

  velora moderator <email>          Let this account read the report queue
  velora moderator --revoke <email> Take that away

  velora push-test <username>       Send a test push to that account's phones
                                    and wait for them to confirm receipt

Configuration is read from the environment; see .env.example.
`)
}

func setupLogging(cfg config.Config) {
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	// Structured JSON in production so the log shipper can parse it; plain
	// text locally because humans read that one.
	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	if cfg.IsProduction() {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler))
}

func serve(cfg config.Config) error {
	ctx := context.Background()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	authService := auth.New(pool, auth.Config{
		SigningKey:      auth.DecodeSigningKey(cfg.TokenSigningKey),
		AccessTokenTTL:  cfg.AccessTokenTTL,
		RefreshTokenTTL: cfg.RefreshTokenTTL,
		GoogleClientIDs: cfg.GoogleClientIDs,
		GoogleOnly:      cfg.GoogleOnly,
	})
	if len(cfg.GoogleClientIDs) == 0 {
		// Not fatal: the service still serves health and can be deployed
		// before the OAuth clients exist. Sign-in will refuse until they do.
		slog.Warn("GOOGLE_CLIENT_IDS is empty — Google sign-in will reject every request")
	}

	mux := http.NewServeMux()

	// Catch-all first: without it, unmatched routes get Go's plain-text 404,
	// which breaks the single error path the clients parse.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, httpx.NotFound("No route matches "+r.Method+" "+r.URL.Path))
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"status": "ok", "env": cfg.Env}
		if err := pool.Ping(r.Context()); err != nil {
			body["status"] = "degraded"
			body["database"] = "unreachable"
			httpx.JSON(w, http.StatusServiceUnavailable, body)
			return
		}
		body["database"] = "ok"
		httpx.JSON(w, http.StatusOK, body)
	})

	authService.Routes(mux)

	mediaStore := media.LocalStore{
		Root:    cfg.MediaRoot,
		BaseURL: cfg.MediaBaseURL,
	}
	mediaService := media.New(pool, mediaStore, authService.RequireAuth)
	mediaService.Routes(mux)

	// After media: assembling a profile joins photo URLs onto the row, so it
	// needs the store's URL scheme. GET /me is registered here, not by auth.
	profileService := profile.New(pool, mediaService, authService)
	profileService.Routes(mux)

	// Discovery reads profiles through the profile service rather than the
	// pool, so there is one definition of how a row becomes a Profile.
	discovery.New(profileService, authService.RequireAuth).
		WithDailyEdits(discovery.NewDailyStore(pool)).
		Routes(mux)

	// Native push: the API only inserts jobs; `velora work` sends them.
	pushRouter, err := mobilepush.NewRouter(ctx, mobilePushConfig(cfg))
	if err != nil {
		return err
	}
	if !pushRouter.Any() {
		slog.Warn("FCM_CREDENTIALS_FILE and APNS_* are unset — devices register but no native push is sent")
	}
	mobileQueue, err := mobilepush.New(pool, pushRouter, false)
	if err != nil {
		return err
	}
	mobilepush.NewDevices(pool, authService).Routes(mux)
	settings.New(pool, authService).Routes(mux)
	blindhour.New(pool, authService).Routes(mux)

	social.New(pool, authService).WithPush(mobileQueue.SocialHook).Routes(mux)
	safety.New(pool, authService).Routes(mux)
	// DELETE /me. Needs the store directly: photo files go after the commit.
	chatPhotos := chat.DirStore{Root: cfg.ChatMediaRoot}
	account.New(pool, mediaStore, authService).WithChatPhotos(chatPhotos).Routes(mux)
	pushQueue, err := chatpush.New(pool, chatpush.Config{PublicKey: cfg.VAPIDPublicKey, PrivateKey: cfg.VAPIDPrivateKey, Subject: cfg.VAPIDSubject}, false)
	if err != nil {
		return err
	}
	pushQueue.Routes(mux, authService)
	// One hook, two channels: browser Web Push and native devices.
	chatService := chat.New(pool, authService, cfg.AllowedOrigins,
		func(ctx context.Context, tx pgx.Tx, messageID, recipientID string, seq int64) error {
			if err := pushQueue.Enqueue(ctx, tx, messageID, recipientID, seq); err != nil {
				return err
			}
			return mobileQueue.MessageHook(ctx, tx, messageID, recipientID, seq)
		})
	chatService.WithPhotos(chatPhotos)
	chatService.Routes(mux)
	chatCtx, cancelChat := context.WithCancel(ctx)
	defer cancelChat()
	defer chatService.Close()
	go chatService.Run(chatCtx)

	if cfg.EnableAPIDocs {
		apidocs.Routes(mux)
		slog.Info("api explorer enabled", "path", "/docs")
	}

	// Rate limits sit innermost, so the outer layers still log and trace a
	// rejected request.
	//
	// Registration is the one that matters: public by necessity, and every
	// call writes a row. The rest of /auth is looser but still bounded, since
	// an unlimited login endpoint is a password-guessing endpoint. The global
	// ceiling is generous enough that no real client meets it and low enough
	// that one address cannot saturate a small box.
	var handler http.Handler = mux
	handler = httpx.LimitPaths(
		httpx.NewLimiter(cfg.RegisterPerHour, time.Hour), "/auth/register")(handler)
	handler = httpx.LimitPaths(
		httpx.NewLimiter(60, 15*time.Minute), "/auth/")(handler)
	handler = httpx.NewLimiter(cfg.RequestsPerMinute, time.Minute).Middleware(handler)

	// Middleware runs outermost first: recover before logging, so a panic is
	// still reported as a completed request with a 500.
	handler = httpx.CORS(cfg.AllowedOrigins)(handler)
	handler = httpx.Logger(handler)
	handler = httpx.Recoverer(handler)
	handler = httpx.RequestID(handler)

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Shut down on SIGINT/SIGTERM, giving in-flight requests time to finish.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", server.Addr, "env", cfg.Env)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		chatService.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func work(cfg config.Config) error {
	slog.Info("worker starting", "env", cfg.Env)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	queue, err := chatpush.New(pool, chatpush.Config{PublicKey: cfg.VAPIDPublicKey, PrivateKey: cfg.VAPIDPrivateKey, Subject: cfg.VAPIDSubject}, true)
	if err != nil {
		return err
	}
	if err = queue.Start(ctx); err != nil {
		return err
	}
	// Native push runs its own River client on its own queue, so neither
	// client fetches a job kind it has no worker for.
	router, err := mobilepush.NewRouter(ctx, mobilePushConfig(cfg))
	if err != nil {
		return err
	}
	mobile, err := mobilepush.New(pool, router, true)
	if err != nil {
		return err
	}
	if err = mobile.Start(ctx); err != nil {
		return err
	}
	// Daily "Blind hour is open" reminders, at 21:00 in each person's zone.
	go blindhour.RunReminders(ctx, pool, mobile)
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = mobile.Stop(shutdown); err != nil {
		return err
	}
	if err = queue.Stop(shutdown); err != nil {
		return err
	}
	slog.Info("worker stopped")
	return nil
}

// pushTest sends a test notification straight to every phone registered to
// one account, prints what the provider answered and then waits for the app
// to confirm receipt, so "I get nothing" splits into "the server could not
// send", "the phone dropped it" and "it arrived".
func pushTest(cfg config.Config, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: velora push-test <username>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	router, err := mobilepush.NewRouter(ctx, mobilePushConfig(cfg))
	if err != nil {
		return err
	}

	rows, err := pool.Query(ctx, `
		select d.id::text, d.platform, d.token, d.app_version from devices d
		join profiles p on p.user_id = d.user_id
		where p.username = $1
	`, strings.TrimPrefix(strings.ToLower(strings.TrimSpace(args[0])), "@"))
	if err != nil {
		return err
	}
	type device struct{ id, platform, token, version string }
	var devices []device
	for rows.Next() {
		var d device
		if err := rows.Scan(&d.id, &d.platform, &d.token, &d.version); err != nil {
			return err
		}
		devices = append(devices, d)
	}
	rows.Close()
	if len(devices) == 0 {
		return fmt.Errorf("@%s has no registered phone", args[0])
	}
	receipts := map[string]string{} // receipt id -> device id
	for _, d := range devices {
		sender, ok := router.For(d.platform)
		if !ok {
			fmt.Println(d.id, d.platform+": no sender configured for this platform")
			continue
		}
		id, err := mobilepush.SendTracked(ctx, pool, sender, d.id, d.version != "", mobilepush.Message{
			Token: d.token,
			Title: "Thapaw test",
			Body:  "If you can see this, notifications reach this phone.",
			Data:  map[string]string{"type": "test"},
		})
		switch {
		case err != nil:
			fmt.Println(d.id, d.platform+": FAILED:", err)
		case id == "":
			fmt.Println(d.id, d.platform+": accepted by the provider (this app version does not confirm receipt)")
		default:
			fmt.Println(d.id, d.platform+": accepted by the provider; waiting for the phone to confirm...")
			receipts[id] = d.id
		}
	}
	for waited := 0; waited < 20 && len(receipts) > 0; waited++ {
		time.Sleep(time.Second)
		for id, deviceID := range receipts {
			var got bool
			if err := pool.QueryRow(ctx, `select received_at is not null from push_receipts where id = $1`, id).Scan(&got); err == nil && got {
				fmt.Println(deviceID, "confirmed: the phone received it")
				delete(receipts, id)
			}
		}
	}
	for _, deviceID := range receipts {
		fmt.Println(deviceID, "NOT confirmed within 20s: the phone dropped it (battery manager or app force-stopped)")
	}
	return nil
}

// moderator grants or revokes access to the report queue.
//
// A subcommand rather than an endpoint on purpose: the first moderator has to
// come from somewhere, and an API that can promote its own caller is a hole no
// amount of guarding closes. Running this needs shell access to the box, which
// is the same bar as reading the database directly.
func moderator(cfg config.Config, args []string) error {
	revoke := false
	if len(args) > 0 && args[0] == "--revoke" {
		revoke, args = true, args[1:]
	}
	if len(args) != 1 {
		return errors.New("usage: velora moderator [--revoke] <email>")
	}
	email := strings.ToLower(strings.TrimSpace(args[0]))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	var userID string
	err = pool.QueryRow(ctx, `
		update users set is_moderator = $1
		where id = (
			select user_id from auth_identities
			where method = 'email' and identifier = $2
		)
		returning id::text
	`, !revoke, email).Scan(&userID)
	if err != nil {
		return fmt.Errorf("no account with the email %q", email)
	}

	if revoke {
		slog.Info("moderator revoked", "email", email, "user", userID)
	} else {
		slog.Info("moderator granted", "email", email, "user", userID)
	}
	return nil
}

func migrate(cfg config.Config) error {
	slog.Info("migrate", "database", redactDSN(cfg.DatabaseURL))
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Connect(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err = chatpush.Migrate(context.Background(), pool); err != nil {
		return err
	}
	version, err := db.Version(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	slog.Info("migrated", "version", version)
	return nil
}

func mobilePushConfig(cfg config.Config) mobilepush.Config {
	return mobilepush.Config{
		FCMProjectID:       cfg.FCMProjectID,
		FCMCredentialsFile: cfg.FCMCredentialsFile,
		APNsKeyFile:        cfg.APNsKeyFile,
		APNsKeyID:          cfg.APNsKeyID,
		APNsTeamID:         cfg.APNsTeamID,
		APNsTopic:          cfg.APNsTopic,
		APNsProduction:     cfg.APNsProduction,
	}
}

// redactDSN keeps a connection string loggable by dropping the credentials.
func redactDSN(dsn string) string {
	at := strings.Index(dsn, "@")
	scheme := strings.Index(dsn, "://")
	if at > 0 && scheme >= 0 && scheme+3 < at {
		return dsn[:scheme+3] + "***" + dsn[at:]
	}
	return dsn
}
