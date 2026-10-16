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
	// EntryBackend names the backend of the model's entry route on this
	// cluster, which is what a quota attaches to there. Empty unless the
	// model is a fleet model and the cluster is in the fleet. It is not in
	// Backends, which lists what the cluster's own routes have.
	EntryBackend string `json:"entry_backend,omitempty"`
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
	// QuotaCapable is false when no cluster has anything a quota can attach to
	// and the model is not a fleet model.
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
	// BestEffortLimit is the most a tenant may use of the model as
	// best-effort in one period of its quota, in the model's unit. Nil is
	// no limit.
	BestEffortLimit *int64 `json:"best_effort_limit"`
	// Warnings are differences between the sites that serve the model which
	// an operator should look at.
	Warnings []string `json:"warnings"`
	// Unit is what the model's limits and usage are counted in: UnitTokens,
	// or UnitCredits once it has prices.
	Unit string `json:"unit"`
	// Prices are the prices in use, PendingPrices the ones that wait for
	// their day. Both are nil when there are none.
	Prices        *Price `json:"prices"`
	PendingPrices *Price `json:"pending_prices"`
	// PriceDryRun counts every tenant of a priced model and refuses nobody.
	PriceDryRun bool `json:"price_dry_run"`
	// MarketIDSet is the market entry an operator chose for the model: nil
	// follows the annotation and the automatic match, "" is no market price.
	MarketIDSet *string `json:"market_id_set"`
	// MarketAnnotations are the values of aigw-ui.io/market-id on the
	// model's deployments, each once.
	MarketAnnotations []string `json:"market_annotations"`
	// Standard is the budget every tenant has on the model, nil when there
	// is none; a tenant's own quota adds to it. StandardNext is a change
	// that waits for its day.
	Standard     *Standard       `json:"standard"`
	StandardNext *StandardChange `json:"standard_next"`
	// QuotaRules is how many rules the tenants' quotas on the model take in
	// its QuotaPolicy, placeholders of quotas since removed included.
	QuotaRules int `json:"quota_rules"`
}

// Standard is a model's standard per-tenant budget: a limit in the model's
// unit for each window.
type Standard struct {
	Limit  int64  `json:"limit"`
	Window string `json:"window"`
}

// StandardChange is a change of a model's standard that starts At, the next
// 00:00 UTC when it was made, so that no tenant's count moves mid-window.
// Standard nil removes it.
type StandardChange struct {
	Standard *Standard `json:"standard"`
	At       time.Time `json:"at"`
}

// Cost returns the cost expression that is rendered for the model: the one
// of its prices, or the one that was typed in.
func (m Model) Cost() string {
	if m.Prices != nil {
		return m.Prices.Render().Expression()
	}
	return m.CostExpression
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
	// CappedAt is set when the tenant reached the model's best-effort limit
	// in the period. From then on it is refused until the period ends.
	CappedAt *time.Time `json:"capped_at"`
}

type Tenant struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	DisplayName string    `json:"display_name"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	KeyCount    int       `json:"key_count"`
	// APIKeyCount is how many of those keys are API keys rather than
	// tokens: while any tenant has one, the gateways list every key.
	APIKeyCount int `json:"api_key_count"`
	QuotaCount  int `json:"quota_count"`
	// Budget is the tenant's budget across every priced model, in credits
	// for each window, enforced by the gateways; nil when it has none. Its
	// quotas per model hold alongside it.
	Budget *TenantBudget `json:"budget"`
	// Models is the IDs of the models the tenant may call, or nil when it
	// may call every model. An empty list closes every model to it.
	Models []string `json:"models"`
	// LimitsChangedAt is when the budget or the list of models last
	// changed: both travel in the tenant's tokens, so a token signed before
	// carries the ones from before.
	LimitsChangedAt *time.Time `json:"limits_changed_at"`
}

// TenantBudget is a tenant's budget across models: credits for each window.
type TenantBudget struct {
	Credits int64  `json:"credits"`
	Window  string `json:"window"`
}

type APIKey struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	ClientID string `json:"client_id"`
	// KeyPrefix is the start of an API key, or the end of a token, whose
	// start is the same for every token: enough to tell keys apart.
	KeyPrefix string     `json:"key_prefix"`
	RevokedAt *time.Time `json:"revoked_at"`
	CreatedAt time.Time  `json:"created_at"`
	// Kind is KindKey, KindToken or KindPersonal; a token expires at
	// ExpiresAt. A personal key does not expire: ExpiresAt is when the last
	// token it got does, and RenewedAt when it got that token.
	Kind      string     `json:"kind"`
	ExpiresAt *time.Time `json:"expires_at"`
	RenewedAt *time.Time `json:"renewed_at"`
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
	// Unit is what TokenLimit is counted in: UnitTokens, or UnitCredits for
	// a model with prices. The field keeps its name for callers that
	// already use it.
	Unit string `json:"unit"`
}

type Overview struct {
	Clusters      int `json:"clusters"`
	ClustersError int `json:"clusters_error"`
	Models        int `json:"models"`
	Tenants       int `json:"tenants"`
	ActiveKeys    int `json:"active_keys"`
	Quotas        int `json:"quotas"`
}
