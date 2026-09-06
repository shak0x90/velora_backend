// Command velora is the whole service: one binary, three modes.
//
//	velora serve     HTTP + WebSocket API
//	velora work      background jobs (photos, daily picks, push)
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

	"github.com/shak0x90/velora_backend/internal/auth"
	"github.com/shak0x90/velora_backend/internal/config"
	"github.com/shak0x90/velora_backend/internal/db"
	"github.com/shak0x90/velora_backend/internal/httpx"
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

	// Middleware runs outermost first: recover before logging, so a panic is
	// still reported as a completed request with a 500.
	var handler http.Handler = mux
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func work(cfg config.Config) error {
	slog.Info("worker starting", "env", cfg.Env)
	// River job registration lands here in Phase 1, when the first job
	// (photo processing) exists. Until then the worker just idles so the
	// Compose file and deploy path can be exercised end to end.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	slog.Info("worker stopped")
	return nil
}

func migrate(cfg config.Config) error {
	slog.Info("migrate", "database", redactDSN(cfg.DatabaseURL))
	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return err
	}
	version, err := db.Version(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	slog.Info("migrated", "version", version)
	return nil
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
