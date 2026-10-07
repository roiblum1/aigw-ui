package store

import "time"

type Cluster struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Site        string     `json:"site"`
	Namespace   string     `json:"namespace"`
	GatewayName string     `json:"gateway_name"`
	AuthEnabled bool       `json:"auth_enabled"`
	SyncStatus  string     `json:"sync_status"`
	SyncMessage string     `json:"sync_message"`
	SyncedAt    *time.Time `json:"synced_at"`
	CreatedAt   time.Time  `json:"created_at"`

	DiscoveryMessage string     `json:"discovery_message"`
	DiscoveredAt     *time.Time `json:"discovered_at"`
}

const (
	SourceManual     = "manual"
	SourceDiscovered = "discovered"
)

// BackendRef is an AIServiceBackend that already exists on a cluster and the
// model name the route sends to it.
type BackendRef struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

type Endpoint struct {
	ClusterID     string `json:"cluster_id"`
	ClusterName   string `json:"cluster_name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	UpstreamModel string `json:"upstream_model"`
	// Source is "manual" for endpoints entered here and "discovered" for
	// models found in a cluster's existing AIGatewayRoutes.
	Source   string       `json:"source"`
	Backends []BackendRef `json:"backends"`
}

// DiscoveredModel is one model found on a cluster.
type DiscoveredModel struct {
	Name     string
	Backends []BackendRef
}

type Model struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Slug          string     `json:"slug"`
	DefaultLimit  int64      `json:"default_limit"`
	DefaultWindow string     `json:"default_window"`
	CreatedAt     time.Time  `json:"created_at"`
	Endpoints     []Endpoint `json:"endpoints"`
}

type Tenant struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"display_name"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	KeyCount    int       `json:"key_count"`
	QuotaCount  int       `json:"quota_count"`
}

type APIKey struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"tenant_id"`
	Name      string     `json:"name"`
	ClientID  string     `json:"client_id"`
	KeyPrefix string     `json:"key_prefix"`
	RevokedAt *time.Time `json:"revoked_at"`
	CreatedAt time.Time  `json:"created_at"`
}

type Quota struct {
	ID         string `json:"id"`
	TenantID   string `json:"tenant_id"`
	ModelID    string `json:"model_id"`
	ModelName  string `json:"model_name"`
	TokenLimit int64  `json:"token_limit"`
	Window     string `json:"window"`
}

type Overview struct {
	Clusters      int `json:"clusters"`
	ClustersError int `json:"clusters_error"`
	Models        int `json:"models"`
	Tenants       int `json:"tenants"`
	ActiveKeys    int `json:"active_keys"`
	Quotas        int `json:"quotas"`
}
