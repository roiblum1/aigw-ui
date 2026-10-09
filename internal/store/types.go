package store

import (
	"time"

	"aigw-ui/internal/weights"
)

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

	// GatewayURL is the address of the cluster's gateway. When set, models are
	// listed from its /v1/models endpoint.
	GatewayURL        string `json:"gateway_url"`
	HasDiscoveryToken bool   `json:"has_discovery_token"`

	// FleetEnabled makes the cluster one of the sites that share traffic: it
	// is a zone in every model's site weights and gets them written.
	FleetEnabled bool `json:"fleet_enabled"`
	// ClientListener is the Gateway listener clients come in on. The API-key
	// policy attaches to it alone; empty attaches it to the whole Gateway.
	ClientListener string `json:"client_listener"`
	// PeerHost and PeerPort are where the other sites reach this site's
	// gateway. A fleet cluster needs them.
	PeerHost string `json:"peer_host"`
	PeerPort int    `json:"peer_port"`
	// FleetRevision identifies the entry routes last applied to the
	// cluster. FleetOutdated is true when the fleet has moved on since.
	FleetRevision string `json:"fleet_revision"`
	FleetOutdated bool   `json:"fleet_outdated"`
}

const (
	SourceManual     = "manual"
	SourceDiscovered = "discovered"
)

// BackendRef is an AIServiceBackend that already exists on a cluster and the
// model name the route sends to it.
type BackendRef struct {
	Name string `json:"name"`
	// Namespace is empty for the cluster's gateway namespace.
	Namespace string `json:"namespace,omitempty"`
	Model     string `json:"model"`
	// Override reports whether the route sets modelNameOverride for this
	// backend. The gateway documents quota matching only for that case.
	Override bool `json:"override"`
	// PeerOnly is true when clients cannot reach the backend through the
	// cluster's own routes, only other sites' gateways can.
	PeerOnly bool `json:"peer_only,omitempty"`
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
	// Capacity is what the cluster can serve of the model and the weight
	// applied for it.
	Capacity EndpointCapacity `json:"capacity"`
}

// DiscoveredModel is one model found on a cluster.
type DiscoveredModel struct {
	Name     string
	Backends []BackendRef
	// OnlyIfKnown is set for a model the cluster has a deployment of but
	// does not expose itself. It is recorded for a model that is already
	// in the catalog and never adds one.
	OnlyIfKnown bool
}

type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	DefaultLimit  int64  `json:"default_limit"`
	DefaultWindow string `json:"default_window"`
	// CostExpression is the CEL expression that turns a request's token counts
	// into the amount charged to quotas. Empty charges total_tokens.
	CostExpression string     `json:"cost_expression"`
	CreatedAt      time.Time  `json:"created_at"`
	Endpoints      []Endpoint `json:"endpoints"`
	// QuotaCapable is false when no cluster has anything a quota can attach to.
	QuotaCapable bool `json:"quota_capable"`
	// SiteWeights is the zone weight of every site listed for the model,
	// the same on every cluster. SiteWeightsNote says why they were not
	// replaced on the last discovery round, when they were not.
	SiteWeights     []weights.Zone `json:"site_weights"`
	SiteWeightsNote string         `json:"site_weights_note"`
	// Fleet is true when this tool renders the model's entry route on every
	// fleet cluster.
	Fleet bool `json:"fleet"`
	// SpentMode says what happens to a tenant whose budget for the model is
	// spent: SpentRefuse or SpentBestEffort.
	SpentMode string `json:"spent_mode"`
	// BestEffortUnlimited serves tenants without a quota of their own on the
	// model as best-effort, in place of sharing its default pool.
	BestEffortUnlimited bool `json:"best_effort_unlimited"`
	// Warnings are differences between the sites that serve the model which
	// an operator should look at.
	Warnings []string `json:"warnings"`
}

// What happens to a tenant whose budget for a model is spent.
const (
	SpentRefuse     = "refuse"
	SpentBestEffort = "best-effort"
)

// Overage is one period in which a tenant was served as best-effort on a
// model: from when its budget was nearly spent until its window ended.
type Overage struct {
	ModelID    string    `json:"model_id"`
	ModelName  string    `json:"model_name"`
	TenantID   string    `json:"tenant_id"`
	TenantSlug string    `json:"tenant_slug"`
	Since      time.Time `json:"since"`
	Until      time.Time `json:"until"`
	// Active is true while the period has not ended.
	Active bool `json:"active"`
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
	// Shadow quotas are counted but never reject a request.
	Shadow bool `json:"shadow"`
}

type Overview struct {
	Clusters      int `json:"clusters"`
	ClustersError int `json:"clusters_error"`
	Models        int `json:"models"`
	Tenants       int `json:"tenants"`
	ActiveKeys    int `json:"active_keys"`
	Quotas        int `json:"quotas"`
}
