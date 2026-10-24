package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/gitops"
	"aigw-ui/internal/render"
)

type Config struct {
	DatabaseURL   string
	ListenAddr    string
	AdminToken    string
	EncryptionKey []byte
	UIDir         string
	// UpstreamDocsDir holds the documentation of the projects the platform
	// is built on, which the image carries (cmd/upstream-docs). Empty or
	// missing: the Docs page has none.
	UpstreamDocsDir string
	AutoSync        bool
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
	// RedisTokenCounts has the gateways count each tenant's requests and
	// tokens by model, for the tenant's own page. It needs RedisURL.
	RedisTokenCounts bool

	// OverageEvery is how often the counters are read to find the tenants
	// whose budget is nearly spent, for the models in best-effort mode.
	OverageEvery time.Duration
	// OverageThreshold is the share of its budget a tenant has to have used
	// to be moved to best-effort. It is below 1 so the move comes before
	// the gateway starts refusing.
	OverageThreshold float64

	// HistoryEvery is how often the usage counters are read to keep the
	// usage history. 0 keeps none.
	HistoryEvery time.Duration
	// TenantPage lets a tenant see its own usage by signing in with one of
	// its API keys.
	TenantPage bool
	// GatewayURL is the address the people who use the models send their
	// requests to, without the API's path: the setup the tenants' page shows
	// adds /anthropic for Claude Code and /v1 for OpenAI clients. Empty
	// leaves it out.
	GatewayURL string
	// ClaudeCodeModel and ClaudeCodeSmallModel are the models the setup
	// gives Claude Code, for its work and for its small background tasks,
	// to a tenant that may call them. Empty, or a model the tenant may not
	// call: its priciest model and its cheapest.
	ClaudeCodeModel, ClaudeCodeSmallModel string

	// CapacityImage is the AIPerf image a capacity measurement runs on the
	// clusters. Empty turns measurements off.
	CapacityImage string
	// CapacityEvery is how often a running measurement's Job is read.
	CapacityEvery time.Duration
	// CapacityWindow is when the hub measures what is not measured or
	// stale by itself; nil never.
	CapacityWindow *capacity.Window
	// CapacityPeer turns on measuring prefill/decode LLMIs: their Jobs run
	// in the gateway's namespace with the peer client certificate.
	CapacityPeer bool

	// TokenIssuer and TokenAudience are the iss and aud of every token the
	// hub signs, and what the gateways require of one. TokenLifetime is how
	// long a new token lasts.
	TokenIssuer, TokenAudience string
	TokenLifetime              time.Duration
	// TokenRenewedLifetime is how long a token a personal key gets lives.
	TokenRenewedLifetime time.Duration

	// MemoryLimit is the container's memory limit in bytes, 0 when not
	// known. The server keeps its heap to 90% of it.
	MemoryLimit int64

	// Fleet is what every site's entry route has in common. PeerSNI is
	// empty when neither FLEET_PEER_SNI nor FLEET_DOMAIN is set, and no
	// entry route can be turned on then.
	Fleet render.FleetConfig

	// GitOps is the git repository of the models the hub deploys, which
	// Argo CD reads. An empty Repo turns the Deployments page off.
	GitOps gitops.Config
	// ArgoCDNamespace is where Argo CD keeps the deployments' Applications,
	// read for their sync and health; "" shows none. ArgoCDKubeconfig is a
	// kubeconfig file for that cluster; "" uses the pod's own account.
	// ArgoCDAppName is how the platform names a model's Application on a
	// cluster, with {cluster} and {model}.
	ArgoCDNamespace, ArgoCDKubeconfig, ArgoCDAppName string
}

func Load() (*Config, error) {
	c := &Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		ListenAddr:  envOr("LISTEN_ADDR", ":8080"),
		AdminToken:  os.Getenv("ADMIN_TOKEN"),
		UIDir:       envOr("UI_DIR", "web/dist"),
		// The image sets it; a checkout has none unless it was fetched there.
		UpstreamDocsDir: envOr("UPSTREAM_DOCS_DIR", "upstream-docs"),
		AutoSync:        envOr("AUTO_SYNC", "true") == "true",

		RedisURL:         os.Getenv("REDIS_URL"),
		RedisCAFile:      os.Getenv("REDIS_CA_FILE"),
		RedisTLSInsecure: os.Getenv("REDIS_TLS_INSECURE") == "true",
		RedisKeyPrefix:   os.Getenv("REDIS_KEY_PREFIX"),
		RedisAllowReset:  os.Getenv("REDIS_ALLOW_RESET") == "true",
		RedisTokenCounts: os.Getenv("REDIS_TOKEN_COUNTS") == "true",
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
	c.HistoryEvery, err = time.ParseDuration(envOr("USAGE_HISTORY_INTERVAL", "1m"))
	if err != nil || c.HistoryEvery < 0 || (c.HistoryEvery > 0 && c.HistoryEvery < 10*time.Second) {
		return nil, errors.New("USAGE_HISTORY_INTERVAL must be a duration of at least 10s such as 1m, or 0 to keep no usage history")
	}
	c.TenantPage = envOr("TENANT_PAGE", "true") == "true"
	c.GatewayURL = gatewayRoot(os.Getenv("GATEWAY_URL"))
	if u, err := url.Parse(c.GatewayURL); c.GatewayURL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
		return nil, errors.New("GATEWAY_URL must be an http or https address such as https://llm.example.com")
	}
	c.ClaudeCodeModel = strings.TrimSpace(os.Getenv("CLAUDE_CODE_MODEL"))
	c.ClaudeCodeSmallModel = strings.TrimSpace(os.Getenv("CLAUDE_CODE_SMALL_MODEL"))
	c.CapacityImage = os.Getenv("CAPACITY_IMAGE")
	c.CapacityEvery, err = time.ParseDuration(envOr("CAPACITY_INTERVAL", "20s"))
	if err != nil || c.CapacityEvery < 5*time.Second {
		return nil, errors.New("CAPACITY_INTERVAL must be a duration of at least 5s such as 20s")
	}
	c.CapacityPeer = os.Getenv("CAPACITY_PEER_MEASUREMENTS") == "true"
	if c.CapacityWindow, err = capacity.ParseWindow(os.Getenv("CAPACITY_WINDOW")); err != nil {
		return nil, fmt.Errorf("CAPACITY_WINDOW: %w", err)
	}
	c.TokenIssuer, c.TokenAudience = envOr("TOKEN_ISSUER", "aigw-ui"), envOr("TOKEN_AUDIENCE", "aigw-fleet")
	days, err := strconv.Atoi(envOr("TOKEN_LIFETIME_DAYS", "180"))
	if err != nil || days < 1 || days > 3650 {
		return nil, errors.New("TOKEN_LIFETIME_DAYS must be a whole number of days from 1 to 3650 such as 180")
	}
	c.TokenLifetime = time.Duration(days) * 24 * time.Hour
	hours, err := strconv.Atoi(envOr("TOKEN_RENEWED_LIFETIME_HOURS", "168"))
	if err != nil || hours < 1 || hours > 24*90 {
		return nil, errors.New("TOKEN_RENEWED_LIFETIME_HOURS must be a whole number of hours from 1 to 2160 such as 168")
	}
	c.TokenRenewedLifetime = time.Duration(hours) * time.Hour
	if v := os.Getenv("MEMORY_LIMIT"); v != "" {
		if c.MemoryLimit, err = strconv.ParseInt(v, 10, 64); err != nil || c.MemoryLimit < 0 {
			return nil, errors.New("MEMORY_LIMIT must be a number of bytes such as 1073741824")
		}
	}
	c.GitOps = gitops.Config{
		Repo:           strings.TrimSpace(os.Getenv("GITOPS_REPO")),
		Branch:         envOr("GITOPS_BRANCH", "main"),
		Dir:            envOr("GITOPS_DIR", "gitops/app-of-apps"),
		Username:       os.Getenv("GITOPS_USERNAME"),
		Token:          os.Getenv("GITOPS_TOKEN"),
		SSHKeyFile:     os.Getenv("GITOPS_SSH_KEY_FILE"),
		KnownHostsFile: os.Getenv("GITOPS_SSH_KNOWN_HOSTS_FILE"),
		AuthorName:     os.Getenv("GITOPS_AUTHOR_NAME"),
		AuthorEmail:    os.Getenv("GITOPS_AUTHOR_EMAIL"),
		WorkDir:        os.Getenv("GITOPS_WORK_DIR"),
	}
	if c.GitOps.Repo == "" && (c.GitOps.Token != "" || c.GitOps.SSHKeyFile != "") {
		return nil, errors.New("GITOPS_TOKEN or GITOPS_SSH_KEY_FILE is set without GITOPS_REPO")
	}
	c.ArgoCDNamespace, c.ArgoCDKubeconfig = os.Getenv("ARGOCD_NAMESPACE"), os.Getenv("ARGOCD_KUBECONFIG")
	c.ArgoCDAppName = envOr("ARGOCD_APP_NAME", gitops.DefaultAppName)
	if !strings.Contains(c.ArgoCDAppName, "{cluster}") || !strings.Contains(c.ArgoCDAppName, "{model}") {
		return nil, errors.New("ARGOCD_APP_NAME must hold {cluster} and {model}, such as popai-{cluster}-{model}-deploy")
	}
	c.Fleet.BestEffortPriority, err = strconv.ParseInt(envOr("BEST_EFFORT_PRIORITY", strconv.Itoa(render.DefaultBestEffortPriority)), 10, 32)
	if err != nil || c.Fleet.BestEffortPriority >= 0 {
		return nil, errors.New("BEST_EFFORT_PRIORITY must be a whole number below 0 such as -1")
	}
	return c, nil
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// gatewayRoot is the gateway's address without a trailing slash and without
// the path of one of its APIs, which an address copied from a client's
// settings ends with: Claude Code's ANTHROPIC_BASE_URL ends with /anthropic,
// an OpenAI client's base URL with /v1. A path before them is kept, for a
// gateway behind a prefix.
func gatewayRoot(raw string) string {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	for _, api := range []string{"/anthropic", "/v1"} {
		if root, ok := strings.CutSuffix(u, api); ok {
			return root
		}
	}
	return u
}
