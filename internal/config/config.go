package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"time"
)

type Config struct {
	DatabaseURL   string
	ListenAddr    string
	AdminToken    string
	EncryptionKey []byte
	UIDir         string
	AutoSync      bool
	// DiscoverEvery is how often clusters are polled for models. 0 turns polling off.
	DiscoverEvery time.Duration
	// SyncEvery is how often every cluster is synced again even though nothing
	// changed, to retry failures and undo manual edits. 0 turns it off.
	SyncEvery time.Duration
}

func Load() (*Config, error) {
	c := &Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		ListenAddr:  envOr("LISTEN_ADDR", ":8080"),
		AdminToken:  os.Getenv("ADMIN_TOKEN"),
		UIDir:       envOr("UI_DIR", "web/dist"),
		AutoSync:    envOr("AUTO_SYNC", "true") == "true",
	}
	if c.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if len(c.AdminToken) < 16 {
		return nil, errors.New("ADMIN_TOKEN is required and must be at least 16 characters")
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("ENCRYPTION_KEY"))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("ENCRYPTION_KEY must be 32 bytes, base64 encoded (generate with: openssl rand -base64 32)")
	}
	c.EncryptionKey = key

	c.DiscoverEvery, err = time.ParseDuration(envOr("DISCOVERY_INTERVAL", "60s"))
	if err != nil || c.DiscoverEvery < 0 || (c.DiscoverEvery > 0 && c.DiscoverEvery < 5*time.Second) {
		return nil, errors.New("DISCOVERY_INTERVAL must be a duration of at least 5s such as 60s, or 0 to disable")
	}
	c.SyncEvery, err = time.ParseDuration(envOr("SYNC_INTERVAL", "5m"))
	if err != nil || c.SyncEvery < 0 || (c.SyncEvery > 0 && c.SyncEvery < 30*time.Second) {
		return nil, errors.New("SYNC_INTERVAL must be a duration of at least 30s such as 5m, or 0 to disable")
	}
	return c, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
