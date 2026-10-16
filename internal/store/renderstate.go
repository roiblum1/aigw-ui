package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/render"
	"aigw-ui/internal/weights"
)

// RenderState collects everything one cluster should be running.
func (s *Store) RenderState(ctx context.Context, clusterID string) (render.State, error) {
	return s.renderState(ctx, clusterID, true)
}

// RenderStateWithoutKeys is RenderState without the tenants' API keys, for
// callers that only need the models and quotas. It decrypts nothing.
func (s *Store) RenderStateWithoutKeys(ctx context.Context, clusterID string) (render.State, error) {
	return s.renderState(ctx, clusterID, false)
}

func (s *Store) renderState(ctx context.Context, clusterID string, withKeys bool) (render.State, error) {
	c, err := s.GetCluster(ctx, clusterID)
	if err != nil {
		return render.State{}, err
	}
	st := render.State{Namespace: c.Namespace, GatewayName: c.GatewayName, ClientListener: c.ClientListener, AuthEnabled: c.AuthEnabled, Fleet: s.fleet}
	if st.Identity, err = s.Identity(ctx); err != nil {
		return st, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT m.id, m.name, m.slug, e.host, e.port, e.upstream_model, m.default_limit, m.default_window, m.cost_expression, e.source, e.backends,
		        e.pools, m.fleet AND m.spent_mode = 'best-effort', m.standard_limit, m.standard_window
		 FROM model_endpoints e JOIN models m ON m.id = e.model_id WHERE e.cluster_id = $1`, clusterID)
	if err != nil {
		return st, err
	}
	index := map[string]int{}
	for rows.Next() {
		var id string
		var m render.Model
		var source string
		var backends []BackendRef
		var pools []Pool
		var bestEffort bool
		var standardLimit *int64
		var standardWindow *string
		if err := rows.Scan(&id, &m.Name, &m.Slug, &m.Host, &m.Port, &m.UpstreamModel, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &source, &backends,
			&pools, &bestEffort, &standardLimit, &standardWindow); err != nil {
			rows.Close()
			return st, err
		}
		m.Standard = renderedStandard(st, standardOf(standardLimit, standardWindow))
		// The class is needed where the model runs, which may be a cluster
		// that has no entry route itself.
		if bestEffort {
			for _, p := range pools {
				m.BestEffortPools = append(m.BestEffortPools, render.Pool{Namespace: p.Namespace, Name: p.Name, Group: p.Group})
			}
		}
		if source == SourceDiscovered {
			m.Existing = make([]render.Target, 0, len(backends))
			for _, b := range backends {
				m.Existing = append(m.Existing, render.Target{Namespace: b.Namespace, Backend: b.Name, Model: b.Model, PeerOnly: b.PeerOnly})
			}
		}
		index[id] = len(st.Models)
		st.Models = append(st.Models, m)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if c.FleetEnabled {
		if err := s.addFleetModels(ctx, &st, index); err != nil {
			return st, err
		}
	}

	if err := s.addPrices(ctx, &st, index); err != nil {
		return st, err
	}
	if st.Tenants, err = list(ctx, s, func(r scanner) (string, error) {
		var slug string
		return slug, r.Scan(&slug)
	}, `SELECT slug FROM tenants WHERE enabled ORDER BY slug`); err != nil {
		return st, err
	}
	if err := s.addTenantBudgets(ctx, &st); err != nil {
		return st, err
	}

	rows, err = s.db.Query(ctx,
		`SELECT q.model_id, t.slug, q.slot, q.token_limit, q.window_size, q.shadow
		 FROM quotas q JOIN tenants t ON t.id = q.tenant_id WHERE t.enabled`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var modelID string
		var q render.TenantQuota
		if err := rows.Scan(&modelID, &q.TenantSlug, &q.Slot, &q.Limit, &q.Window, &q.Shadow); err != nil {
			rows.Close()
			return st, err
		}
		if i, ok := index[modelID]; ok {
			st.Models[i].Quotas = append(st.Models[i].Quotas, q)
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if !withKeys {
		return st, nil
	}

	rows, err = s.db.Query(ctx,
		`SELECT k.client_id, k.key_enc FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now()) AND t.enabled ORDER BY k.client_id`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var clientID string
		var enc []byte
		if err := rows.Scan(&clientID, &enc); err != nil {
			return st, err
		}
		plain, err := s.box.Open(enc)
		if err != nil {
			return st, fmt.Errorf("decrypt key %s: %w", clientID, err)
		}
		st.Keys = append(st.Keys, render.Key{ClientID: clientID, Value: string(plain)})
	}
	return st, rows.Err()
}

// renderedStandard is a model's standard as the gateway gets it: only while
// it takes tokens only, since the standard's rule reads the tenant header a
// token sets and a request with an API key has none. Until then the model
// keeps its rules per tenant (StandardHeld).
func renderedStandard(st render.State, std *Standard) *render.Budget {
	if std == nil || !tokensOnly(st) {
		return nil
	}
	return &render.Budget{Limit: std.Limit, Window: std.Window}
}

// tokensOnly reports whether the gateway reads the tenant header a token
// sets, which the budgets across models, the models a tenant may call and
// a model's standard are enforced through.
func tokensOnly(st render.State) bool {
	return st.AuthEnabled && st.Identity.Mode == render.IdentityTokens
}

// addTenantBudgets gives the state the enabled tenants' budgets across
// models, each at its position, and the models each restricted tenant may
// call, by model name. A disabled tenant's position is left empty.
// Neither is given until the gateway takes tokens only (BudgetHeld).
func (s *Store) addTenantBudgets(ctx context.Context, st *render.State) error {
	if !tokensOnly(*st) {
		return nil
	}
	rows, err := s.db.Query(ctx,
		`SELECT t.slug, t.budget_credits, t.budget_window, t.restricted,
		        (SELECT coalesce(array_agg(m.name ORDER BY m.name), '{}') FROM tenant_models tm JOIN models m ON m.id = tm.model_id WHERE tm.tenant_id = t.id)
		 FROM tenants t WHERE t.enabled AND (t.budget_credits IS NOT NULL OR t.restricted) ORDER BY t.slug`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var slug string
		var credits *int64
		var window *string
		var restricted bool
		var models []string
		if err := rows.Scan(&slug, &credits, &window, &restricted, &models); err != nil {
			return err
		}
		if credits != nil && window != nil {
			st.Budgets = append(st.Budgets, render.TenantBudget{TenantSlug: slug, Limit: *credits, Window: *window})
		}
		if restricted {
			st.Restricted = append(st.Restricted, render.TenantModels{TenantSlug: slug, Models: models})
		}
	}
	return rows.Err()
}

// Reasons an entry route is left as it is on the clusters.
const (
	heldNoConfig = "the server has no FLEET_DOMAIN or FLEET_PEER_SNI set"
	heldNoSite   = "none of its sites is a fleet cluster with a peer host"
)

// addFleetModels gives every model with an entry route its sites, adding the
// models this cluster does not serve itself: the entry route is on every
// fleet cluster. index maps a model's ID to its position in st.Models.
//
// A model whose entry route cannot be rendered is held: the cluster keeps
// what it has for it, and everything else is still synced. A missing setting
// or a site that left the fleet must never read as "the route was turned
// off", and one such model must not keep keys and quotas from being applied.
func (s *Store) addFleetModels(ctx context.Context, st *render.State, index map[string]int) error {
	rows, err := s.db.Query(ctx, `SELECT name, peer_host, peer_port FROM clusters WHERE fleet_enabled AND peer_host <> ''`)
	if err != nil {
		return err
	}
	type peer struct {
		host string
		port int
	}
	peers := map[string]peer{}
	for rows.Next() {
		var name string
		var p peer
		if err := rows.Scan(&name, &p.host, &p.port); err != nil {
			rows.Close()
			return err
		}
		peers[name] = p
	}
	if err := rows.Err(); err != nil {
		return err
	}

	bestEffort, err := s.BestEffortTenants(ctx)
	if err != nil {
		return err
	}

	rows, err = s.db.Query(ctx,
		`SELECT id, name, slug, default_limit, default_window, cost_expression, fleet_zones, spent_mode, standard_limit, standard_window FROM models WHERE fleet ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, mode string
		var m render.Model
		var zones []weights.Zone
		var standardLimit *int64
		var standardWindow *string
		if err := rows.Scan(&id, &m.Name, &m.Slug, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &zones, &mode, &standardLimit, &standardWindow); err != nil {
			return err
		}
		m.Standard = renderedStandard(*st, standardOf(standardLimit, standardWindow))
		i, ok := index[id]
		if !ok {
			i = len(st.Models)
			index[id] = i
			st.Models = append(st.Models, m)
		}
		if mode == SpentBestEffort {
			st.Models[i].BestEffort = true
			st.Models[i].Overage = bestEffort[id]
		}
		if !s.FleetConfigured() {
			st.Models[i].HeldReason = heldNoConfig
			continue
		}
		var sites []render.FleetSite
		for _, z := range zones {
			if p, ok := peers[z.Zone]; ok {
				sites = append(sites, render.FleetSite{Name: z.Zone, Host: p.host, Port: p.port, Weight: z.Weight})
			}
		}
		if len(sites) == 0 {
			st.Models[i].HeldReason = heldNoSite
			continue
		}
		st.Models[i].Fleet = sites
	}
	return rows.Err()
}

// FleetModels returns the names of the models that have an entry route.
func (s *Store) FleetModels(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT name FROM models WHERE fleet ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// CurrentFleetRevision returns the revision a fleet cluster has once it is
// synced: render.FleetRevision of the fleet as it is now. It is the same for
// every fleet cluster, and empty when no model has an entry route.
func (s *Store) CurrentFleetRevision(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRow(ctx, `SELECT id FROM clusters WHERE fleet_enabled ORDER BY name LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	st, err := s.renderState(ctx, id, false)
	if err != nil {
		return "", err
	}
	return render.FleetRevision(st), nil
}

// addPrices gives every priced model the cost expression of its prices, in
// place of one that was typed in, and its dry-run setting.
func (s *Store) addPrices(ctx context.Context, st *render.State, index map[string]int) error {
	prices, err := s.modelPrices(ctx)
	if err != nil || len(prices.current) == 0 {
		return err
	}
	rows, err := s.db.Query(ctx, `SELECT id FROM models WHERE price_dry_run`)
	if err != nil {
		return err
	}
	dryRun, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for id, p := range prices.current {
		if i, ok := index[id]; ok {
			st.Models[i].CostExpression = p.Render().Expression()
			st.Models[i].Priced = true
		}
	}
	for _, id := range dryRun {
		if i, ok := index[id]; ok {
			st.Models[i].DryRun = prices.current[id].AppliedAt != nil
		}
	}
	return nil
}
