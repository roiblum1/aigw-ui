package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// valid sets the three settings Load cannot do without.
func valid(t *testing.T) {
	t.Helper()
	// Every setting Load reads starts empty, whatever the caller's shell has.
	for _, name := range []string{
		"LISTEN_ADDR", "UI_DIR", "AUTO_SYNC", "DISCOVERY_INTERVAL", "SYNC_INTERVAL",
		"REDIS_URL", "REDIS_CA_FILE", "REDIS_TLS_INSECURE", "REDIS_KEY_PREFIX", "REDIS_ALLOW_RESET",
		"OVERAGE_INTERVAL", "OVERAGE_THRESHOLD", "BEST_EFFORT_PRIORITY",
		"FLEET_PEER_SNI", "FLEET_DOMAIN", "FLEET_PEER_CA_CONFIGMAP", "FLEET_PEER_CLIENT_SECRET", "FLEET_SESSION_HEADER",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("DATABASE_URL", "postgres://localhost/aigw")
	t.Setenv("ADMIN_TOKEN", "0123456789abcdef")
	t.Setenv("ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

func TestLoadDefaults(t *testing.T) {
	valid(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || !c.AutoSync || c.DiscoverEvery != time.Minute || c.SyncEvery != 5*time.Minute {
		t.Errorf("defaults = %+v", c)
	}
	if c.OverageEvery != 15*time.Second || c.OverageThreshold != 0.9 {
		t.Errorf("overage defaults = %v, %v", c.OverageEvery, c.OverageThreshold)
	}
	if c.Fleet.BestEffortPriority != -1 {
		t.Errorf("best-effort priority = %d, want -1", c.Fleet.BestEffortPriority)
	}
	if c.RedisURL != "" || c.RedisAllowReset {
		t.Error("usage monitoring is on without a Redis URL")
	}
	if c.Fleet.PeerSNI != "" {
		t.Errorf("peer SNI = %q without FLEET_DOMAIN or FLEET_PEER_SNI", c.Fleet.PeerSNI)
	}
	if got := strings.Join(c.Fleet.SessionHeaders, ","); got != "x-claude-code-session-id,x-openwebui-chat-id" {
		t.Errorf("session headers = %q", got)
	}
}

func TestLoadFleet(t *testing.T) {
	valid(t)
	t.Setenv("FLEET_DOMAIN", "example.com")
	t.Setenv("FLEET_SESSION_HEADER", " X-Session-ID , ,x-chat ")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Fleet.PeerSNI != "peers.llm.example.com" {
		t.Errorf("peer SNI = %q", c.Fleet.PeerSNI)
	}
	if got := strings.Join(c.Fleet.SessionHeaders, ","); got != "x-session-id,x-chat" {
		t.Errorf("session headers = %q, want them lower case and without the empty one", got)
	}

	// A name given outright wins over the one made from the domain.
	t.Setenv("FLEET_PEER_SNI", "peers.internal")
	if c, _ = Load(); c.Fleet.PeerSNI != "peers.internal" {
		t.Errorf("peer SNI = %q", c.Fleet.PeerSNI)
	}
}

func TestLoadRefuses(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{"DATABASE_URL", "", "DATABASE_URL"},
		{"ADMIN_TOKEN", "short", "ADMIN_TOKEN"},
		{"ENCRYPTION_KEY", "not base64!", "ENCRYPTION_KEY"},
		{"ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)), "ENCRYPTION_KEY"},
		{"DISCOVERY_INTERVAL", "1s", "DISCOVERY_INTERVAL"},
		{"DISCOVERY_INTERVAL", "soon", "DISCOVERY_INTERVAL"},
		{"SYNC_INTERVAL", "10s", "SYNC_INTERVAL"},
		{"OVERAGE_INTERVAL", "1s", "OVERAGE_INTERVAL"},
		{"OVERAGE_INTERVAL", "0", "OVERAGE_INTERVAL"},
		{"OVERAGE_THRESHOLD", "0.2", "OVERAGE_THRESHOLD"},
		{"OVERAGE_THRESHOLD", "1.5", "OVERAGE_THRESHOLD"},
		{"BEST_EFFORT_PRIORITY", "0", "BEST_EFFORT_PRIORITY"},
		{"BEST_EFFORT_PRIORITY", "3", "BEST_EFFORT_PRIORITY"},
		{"BEST_EFFORT_PRIORITY", "low", "BEST_EFFORT_PRIORITY"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			valid(t)
			t.Setenv(tc.name, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one that names %s", err, tc.want)
			}
		})
	}
}

// 0 turns polling and the periodic sync off. It is not "too short".
func TestLoadZeroTurnsIntervalsOff(t *testing.T) {
	valid(t)
	t.Setenv("DISCOVERY_INTERVAL", "0")
	t.Setenv("SYNC_INTERVAL", "0")
	c, err := Load()
	if err != nil || c.DiscoverEvery != 0 || c.SyncEvery != 0 {
		t.Errorf("intervals = %v, %v, %v", c.DiscoverEvery, c.SyncEvery, err)
	}
}
