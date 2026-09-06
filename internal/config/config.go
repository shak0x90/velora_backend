// Package config reads everything the service needs from the environment.
// Secrets never have defaults — a missing one is a startup error, not a
// silently-insecure fallback.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env             string
	Port            int
	DatabaseURL     string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	TokenSigningKey string
	AllowedOrigins  []string
	LogLevel        string
	// GoogleClientIDs are the OAuth client IDs accepted as the `aud` claim —
	// one each for web, Android, and iOS. These are not secrets; they ship
	// inside the client apps. Rejecting anything else is what stops a token
	// minted for a different app from working here.
	GoogleClientIDs []string
}

func (c Config) IsProduction() bool { return c.Env == "production" }

// Load reads the environment and validates it. Call once at startup and pass
// the result down; nothing below should reach for os.Getenv on its own.
func Load() (Config, error) {
	cfg := Config{
		Env:             getString("VELORA_ENV", "development"),
		Port:            getInt("PORT", 8080),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		AccessTokenTTL:  getDuration("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL: getDuration("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		TokenSigningKey: os.Getenv("TOKEN_SIGNING_KEY"),
		AllowedOrigins:  getList("ALLOWED_ORIGINS", []string{"http://localhost:3000"}),
		LogLevel:        getString("LOG_LEVEL", "info"),
		GoogleClientIDs: getList("GOOGLE_CLIENT_IDS", nil),
	}

	var missing []string
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	// Development can run without a signing key so `velora serve` starts on a
	// fresh clone; production cannot.
	if cfg.TokenSigningKey == "" && cfg.IsProduction() {
		missing = append(missing, "TOKEN_SIGNING_KEY")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

func getString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return fallback
}

func getDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil {
			return parsed
		}
	}
	return fallback
}

func getList(key string, fallback []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
