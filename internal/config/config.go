package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"aigw-ui/internal/render"
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

	// RedisURL is the Redis the gateways' quota rate limit services count in.
	// It is only read, to show usage. Empty turns usage monitoring off.
	RedisURL         string
	RedisCAFile      string
	RedisTLSInsecure bool
	RedisKeyPrefix   string
	// RedisAllowReset lets an admin delete a quota's counter to reset its usage.
	RedisAllowReset bool

	// OverageEvery is how often the counters are read to find the tenants
	// whose budget is nearly spent, for the models in best-effort mode.
	OverageEvery time.Duration
	// OverageThreshold is the share of its budget a tenant has to have used
	// to be moved to best-effort. It is below 1 so the move comes before
	// the gateway starts refusing.
	OverageThreshold float64

	// Fleet is what every site's entry route has in common. PeerSNI is
	// empty when neither FLEET_PEER_SNI nor FLEET_DOMAIN is set, and no
	// entry route can be turned on then.
	Fleet render.FleetConfig
}

func Load() (*Config, error) {
	c := &Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		ListenAddr:  envOr("LISTEN_ADDR", ":8080"),
		AdminToken:  os.Getenv("ADMIN_TOKEN"),
		UIDir:       envOr("UI_DIR", "web/dist"),
		AutoSync:    envOr("AUTO_SYNC", "true") == "true",

		RedisURL:         os.Getenv("REDIS_URL"),
		RedisCAFile:      os.Getenv("REDIS_CA_FILE"),
		RedisTLSInsecure: os.Getenv("REDIS_TLS_INSECURE") == "true",
		RedisKeyPrefix:   os.Getenv("REDIS_KEY_PREFIX"),
		RedisAllowReset:  os.Getenv("REDIS_ALLOW_RESET") == "true",
	}
	c.Fleet = render.FleetConfig{
		PeerSNI:      os.Getenv("FLEET_PEER_SNI"),
		CAConfigMap:  envOr("FLEET_PEER_CA_CONFIGMAP", "llm-peer-ca"),
		ClientSecret: envOr("FLEET_PEER_CLIENT_SECRET", "llm-peer-client"),
	}
	// One name per kind of client: Claude Code and Open WebUI by default.
	for _, name := range strings.Split(envOr("FLEET_SESSION_HEADER", "x-claude-code-session-id,x-openwebui-chat-id"), ",") {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			c.Fleet.SessionHeaders = append(c.Fleet.SessionHeaders, name)
		}
	}
	if domain := os.Getenv("FLEET_DOMAIN"); c.Fleet.PeerSNI == "" && domain != "" {
		c.Fleet.PeerSNI = "peers.llm." + domain
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
	c.OverageEvery, err = time.ParseDuration(envOr("OVERAGE_INTERVAL", "15s"))
	if err != nil || c.OverageEvery < 5*time.Second {
		return nil, errors.New("OVERAGE_INTERVAL must be a duration of at least 5s such as 15s")
	}
	c.OverageThreshold, err = strconv.ParseFloat(envOr("OVERAGE_THRESHOLD", "0.9"), 64)
	if err != nil || c.OverageThreshold < 0.5 || c.OverageThreshold > 1 {
		return nil, errors.New("OVERAGE_THRESHOLD must be a number from 0.5 to 1 such as 0.9")
	}
	return c, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
