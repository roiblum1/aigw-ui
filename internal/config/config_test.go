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
		"LISTEN_ADDR", "UI_DIR", "UPSTREAM_DOCS_DIR", "AUTO_SYNC", "DISCOVERY_INTERVAL", "SYNC_INTERVAL",
		"REDIS_URL", "REDIS_CA_FILE", "REDIS_TLS_INSECURE", "REDIS_KEY_PREFIX", "REDIS_ALLOW_RESET",
		"OVERAGE_INTERVAL", "OVERAGE_THRESHOLD", "BEST_EFFORT_PRIORITY",
		"FLEET_PEER_SNI", "FLEET_DOMAIN", "FLEET_PEER_CA_CONFIGMAP", "FLEET_PEER_CLIENT_SECRET", "FLEET_SESSION_HEADER",
		"CAPACITY_PEER_MEASUREMENTS", "TOKEN_ISSUER", "TOKEN_AUDIENCE", "TOKEN_LIFETIME_DAYS", "TOKEN_RENEWED_LIFETIME_HOURS",
		"MEMORY_LIMIT", "GATEWAY_URL", "CLAUDE_CODE_MODEL", "CLAUDE_CODE_SMALL_MODEL",
		"GITOPS_REPO", "GITOPS_BRANCH", "GITOPS_DIR", "GITOPS_USERNAME", "GITOPS_TOKEN", "GITOPS_SSH_KEY_FILE",
		"GITOPS_SSH_KNOWN_HOSTS_FILE", "GITOPS_AUTHOR_NAME", "GITOPS_AUTHOR_EMAIL", "GITOPS_WORK_DIR", "ARGOCD_NAMESPACE", "ARGOCD_KUBECONFIG", "ARGOCD_APP_NAME",
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
	if c.CapacityPeer {
		t.Error("prefill/decode measurements on by default")
	}
	if c.TokenIssuer != "aigw-ui" || c.TokenAudience != "aigw-fleet" || c.TokenLifetime != 180*24*time.Hour {
		t.Errorf("tokens = %q %q %v", c.TokenIssuer, c.TokenAudience, c.TokenLifetime)
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
	if c.MemoryLimit != 0 {
		t.Errorf("memory limit = %d without MEMORY_LIMIT", c.MemoryLimit)
	}
	if c.GatewayURL != "" {
		t.Errorf("gateway address = %q without GATEWAY_URL", c.GatewayURL)
	}
	if got := strings.Join(c.Fleet.SessionHeaders, ","); got != "x-claude-code-session-id,x-openwebui-chat-id" {
		t.Errorf("session headers = %q", got)
	}
	if c.GitOps.Repo != "" || c.GitOps.Branch != "main" || c.GitOps.Dir != "gitops/app-of-apps" || c.ArgoCDNamespace != "" || c.ArgoCDAppName != "popai-{cluster}-{model}-deploy" {
		t.Errorf("deployments = %+v, Argo CD namespace %q", c.GitOps, c.ArgoCDNamespace)
	}
}

// The gateway's address is kept without a trailing slash and without an
// API's path, which an address copied from Claude Code's or an OpenAI
// client's settings has, so that the setup adds each API's path once. A
// prefix before it is kept.
func TestLoadGatewayURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://llm.example.com/":                  "https://llm.example.com",
		"https://llm.example.com/anthropic":         "https://llm.example.com",
		"http://10.0.0.5/anthropic/":                "http://10.0.0.5",
		"https://llm.example.com/v1":                "https://llm.example.com",
		"https://example.com/llm/v1":                "https://example.com/llm",
		"https://example.com/llm":                   "https://example.com/llm",
		" https://llm.example.com/anthropic-proxy ": "https://llm.example.com/anthropic-proxy",
	} {
		valid(t)
		t.Setenv("GATEWAY_URL", raw)
		if c, err := Load(); err != nil || c.GatewayURL != want {
			t.Errorf("%q: gateway address = %+v, %v; want %q", raw, c, err, want)
		}
	}
	valid(t)
	t.Setenv("CLAUDE_CODE_MODEL", " GLM5.3 ")
	t.Setenv("CLAUDE_CODE_SMALL_MODEL", "qwen-small")
	if c, err := Load(); err != nil || c.ClaudeCodeModel != "GLM5.3" || c.ClaudeCodeSmallModel != "qwen-small" {
		t.Fatalf("Claude Code models = %+v, %v", c, err)
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
		{"TOKEN_LIFETIME_DAYS", "0", "TOKEN_LIFETIME_DAYS"},
		{"TOKEN_LIFETIME_DAYS", "180d", "TOKEN_LIFETIME_DAYS"},
		{"TOKEN_LIFETIME_DAYS", "4000", "TOKEN_LIFETIME_DAYS"},
		{"TOKEN_RENEWED_LIFETIME_HOURS", "0", "TOKEN_RENEWED_LIFETIME_HOURS"},
		{"TOKEN_RENEWED_LIFETIME_HOURS", "7d", "TOKEN_RENEWED_LIFETIME_HOURS"},
		// The chart passes the limit in bytes, as the pod's spec gives it.
		{"MEMORY_LIMIT", "1Gi", "MEMORY_LIMIT"},
		{"MEMORY_LIMIT", "-1", "MEMORY_LIMIT"},
		{"GATEWAY_URL", "llm.example.com", "GATEWAY_URL"},
		{"GATEWAY_URL", "ftp://llm.example.com", "GATEWAY_URL"},
		// A credential without the repository it is for is a mistake.
		{"GITOPS_TOKEN", "t", "GITOPS_REPO"},
		{"ARGOCD_APP_NAME", "aigw-{cluster}", "ARGOCD_APP_NAME"},
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

// The chart sets MEMORY_LIMIT from the container's memory limit, which the
// pod's spec gives in bytes.
func TestLoadMemoryLimit(t *testing.T) {
	valid(t)
	t.Setenv("MEMORY_LIMIT", "1073741824")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.MemoryLimit != 1<<30 {
		t.Errorf("memory limit = %d, want %d", c.MemoryLimit, 1<<30)
	}
}
