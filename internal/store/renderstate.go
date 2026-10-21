package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/render"
	"aigw-ui/internal/weights"
)

// RenderState collects everything one cluster should be running.
func (s *Store) RenderState(ctx context.Context, clusterID string) (render.State, error) {
	c, err := s.GetCluster(ctx, clusterID)
	if err != nil {
		return render.State{}, err
	}
	return s.renderState(ctx, c, &fleetReads{s: s}, true)
}

// RenderStateWithoutKeys is RenderState without the tenants' API keys, for
// callers that only need the models and quotas. It decrypts nothing.
func (s *Store) RenderStateWithoutKeys(ctx context.Context, clusterID string) (render.State, error) {
	c, err := s.GetCluster(ctx, clusterID)
	if err != nil {
		return render.State{}, err
	}
	return s.renderState(ctx, c, &fleetReads{s: s}, false)
}

// RenderStatesWithoutKeys is RenderStateWithoutKeys of every cluster, in the
// order of ListClusters. What all clusters' states have in common is read
// once: a cluster adds one query, not fifteen.
func (s *Store) RenderStatesWithoutKeys(ctx context.Context) ([]Cluster, []render.State, error) {
	clusters, err := s.ListClusters(ctx)
	if err != nil {
		return nil, nil, err
	}
	f := &fleetReads{s: s}
	states := make([]render.State, 0, len(clusters))
	for _, c := range clusters {
		st, err := s.renderState(ctx, c, f, false)
		if err != nil {
			return nil, nil, err
		}
		states = append(states, st)
	}
	return clusters, states, nil
}

// lazy is a value read the first time it is asked for.
type lazy[T any] struct {
	done bool
	v    T
}

func (l *lazy[T]) get(read func() (T, error)) (T, error) {
	if l.done {
		return l.v, nil
	}
	v, err := read()
	if err != nil {
		return v, err
	}
	l.v, l.done = v, true
	return v, nil
}

// fleetReads holds what a cluster's state is built from beside the cluster
// itself and its endpoints, each part read when a state first needs it. The
// states built from one fleetReads share these reads, and render changes
// none of them.
type fleetReads struct {
	s           *Store
	identity    lazy[render.Identity]
	peers       lazy[map[string]peer]
	bestEffort  lazy[map[string][]string]
	fleetModels lazy[[]fleetModel]
	prices      lazy[modelPrices]
	dryRun      lazy[[]string]
	limits      lazy[tenantLimits]
	quotas      lazy[[]modelQuota]
	amounts     lazy[[]modelAmount]
	keys        lazy[[]render.Key]
}

func (s *Store) renderState(ctx context.Context, c Cluster, f *fleetReads, withKeys bool) (render.State, error) {
	st := render.State{Namespace: c.Namespace, GatewayName: c.GatewayName, ClientListener: c.ClientListener, AuthEnabled: c.AuthEnabled, Fleet: s.fleet,
		TokenCounts: s.tokenCounts}
	var err error
	if st.Identity, err = f.identity.get(func() (render.Identity, error) { return s.Identity(ctx) }); err != nil {
		return st, err
	}

	rows, err := s.db.Query(ctx,
		`SELECT m.id, m.name, m.slug, e.host, e.port, e.upstream_model, m.default_limit, m.default_window, m.cost_expression, e.source, e.backends,
		        e.pools, m.fleet AND m.spent_mode = 'best-effort', m.budget_pass_slot
		 FROM model_endpoints e JOIN models m ON m.id = e.model_id WHERE e.cluster_id = $1`, c.ID)
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
		if err := rows.Scan(&id, &m.Name, &m.Slug, &m.Host, &m.Port, &m.UpstreamModel, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &source, &backends,
			&pools, &bestEffort, &m.BudgetPassSlot); err != nil {
			rows.Close()
			return st, err
		}
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
		if err := s.addFleetModels(ctx, f, &st, index); err != nil {
			return st, err
		}
	}

	if err := s.addPrices(ctx, f, &st, index); err != nil {
		return st, err
	}
	if err := s.addTenantBudgets(ctx, f, &st); err != nil {
		return st, err
	}

	quotas, err := f.quotas.get(func() ([]modelQuota, error) {
		return list(ctx, s, func(r scanner) (modelQuota, error) {
			var q modelQuota
			return q, r.Scan(&q.modelID, &q.TenantSlug, &q.Slot, &q.Limit, &q.Window, &q.Shadow)
		}, `SELECT q.model_id, t.slug, q.slot, q.token_limit, q.window_size, q.shadow
		    FROM quotas q JOIN tenants t ON t.id = q.tenant_id WHERE t.enabled`)
	})
	if err != nil {
		return st, err
	}
	for _, q := range quotas {
		if i, ok := index[q.modelID]; ok {
			st.Models[i].Quotas = append(st.Models[i].Quotas, q.TenantQuota)
		}
	}
	if err := s.addQuotaAmounts(ctx, f, &st, index); err != nil {
		return st, err
	}
	if !withKeys {
		return st, nil
	}

	keys, err := f.keys.get(func() ([]render.Key, error) {
		return list(ctx, s, func(r scanner) (render.Key, error) {
			var k render.Key
			var enc []byte
			if err := r.Scan(&k.ClientID, &enc); err != nil {
				return k, err
			}
			plain, err := s.box.Open(enc)
			if err != nil {
				return k, fmt.Errorf("decrypt key %s: %w", k.ClientID, err)
			}
			k.Value = string(plain)
			return k, nil
		}, `SELECT k.client_id, k.key_enc FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		    WHERE k.kind <> 'personal' AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now()) AND t.enabled
		    UNION ALL
		    -- The tokens personal keys got while the gateways take API keys
		    -- too (listedToken), each under the tenant's slug and its jti.
		    SELECT t.slug || '.' || p.jti, p.token_enc FROM personal_tokens p
		    JOIN api_keys k ON k.id = p.key_id JOIN tenants t ON t.id = k.tenant_id
		    WHERE k.revoked_at IS NULL AND p.expires_at > now() AND t.enabled
		    ORDER BY 1`)
	})
	st.Keys = slices.Clone(keys)
	return st, err
}

// modelQuota is a tenant's own quota on a model, and modelAmount an amount
// of the tenants' own quotas on a model, as RenderState reads them.
type modelQuota struct {
	modelID string
	render.TenantQuota
}

type modelAmount struct {
	modelID string
	render.QuotaAmount
}

// addQuotaAmounts renders the tenants' own quotas by amount while the
// gateway takes tokens only: the token names the tenant's amount, through a
// header only a token sets. Until then each tenant keeps a rule of its own.
func (s *Store) addQuotaAmounts(ctx context.Context, f *fleetReads, st *render.State, index map[string]int) error {
	if !tokensOnly(*st) {
		return nil
	}
	for i := range st.Models {
		st.Models[i].ByAmount = true
	}
	amounts, err := f.amounts.get(func() ([]modelAmount, error) {
		return list(ctx, s, func(r scanner) (modelAmount, error) {
			var a modelAmount
			return a, r.Scan(&a.modelID, &a.Slot, &a.Limit, &a.Window, &a.Shadow)
		}, `SELECT model_id, slot, token_limit, window_size, shadow FROM quota_amounts ORDER BY model_id, slot`)
	})
	if err != nil {
		return err
	}
	for _, a := range amounts {
		if i, ok := index[a.modelID]; ok {
			st.Models[i].Amounts = append(st.Models[i].Amounts, a.QuotaAmount)
		}
	}
	return nil
}

// tokensOnly reports whether the gateway reads the tenant header a token
// sets, which the budgets across models, the models a tenant may call and
// the quotas by amount are enforced through.
func tokensOnly(st render.State) bool {
	return st.AuthEnabled && st.Identity.Mode == render.IdentityTokens
}

// tenantLimits are the enabled tenants' budgets across models and the
// models each restricted tenant may call.
type tenantLimits struct {
	budgets    []render.TenantBudget
	restricted []render.TenantModels
}

// addTenantBudgets gives the state the enabled tenants' budgets across
// models, each at its position, and the models each restricted tenant may
// call, by model name. A disabled tenant's position is left empty.
// Neither is given until the gateway takes tokens only (BudgetHeld).
func (s *Store) addTenantBudgets(ctx context.Context, f *fleetReads, st *render.State) error {
	if !tokensOnly(*st) {
		return nil
	}
	limits, err := f.limits.get(func() (tenantLimits, error) {
		var out tenantLimits
		rows, err := s.db.Query(ctx,
			`SELECT t.slug, t.budget_credits, t.budget_window, t.restricted,
			        (SELECT coalesce(array_agg(m.name ORDER BY m.name), '{}') FROM tenant_models tm JOIN models m ON m.id = tm.model_id WHERE tm.tenant_id = t.id)
			 FROM tenants t WHERE t.enabled AND (t.budget_credits IS NOT NULL OR t.restricted) ORDER BY t.slug`)
		if err != nil {
			return out, err
		}
		defer rows.Close()
		for rows.Next() {
			var slug string
			var credits *int64
			var window *string
			var restricted bool
			var models []string
			if err := rows.Scan(&slug, &credits, &window, &restricted, &models); err != nil {
				return out, err
			}
			if credits != nil && window != nil {
				out.budgets = append(out.budgets, render.TenantBudget{TenantSlug: slug, Limit: *credits, Window: *window})
			}
			if restricted {
				out.restricted = append(out.restricted, render.TenantModels{TenantSlug: slug, Models: models})
			}
		}
		return out, rows.Err()
	})
	if err != nil {
		return err
	}
	st.Budgets, st.Restricted = slices.Clone(limits.budgets), slices.Clone(limits.restricted)
	// A budget holds its tenant on every model it is charged on, in place
	// of the model's own rules.
	for i := range st.Models {
		st.Models[i].BudgetsHold = len(st.Budgets) > 0
	}
	return nil
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
func (s *Store) addFleetModels(ctx context.Context, f *fleetReads, st *render.State, index map[string]int) error {
	peers, err := f.peers.get(func() (map[string]peer, error) {
		rows, err := s.db.Query(ctx, `SELECT name, peer_host, peer_port FROM clusters WHERE fleet_enabled AND peer_host <> ''`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		peers := map[string]peer{}
		for rows.Next() {
			var name string
			var p peer
			if err := rows.Scan(&name, &p.host, &p.port); err != nil {
				return nil, err
			}
			peers[name] = p
		}
		return peers, rows.Err()
	})
	if err != nil {
		return err
	}
	bestEffort, err := f.bestEffort.get(func() (map[string][]string, error) { return s.BestEffortTenants(ctx) })
	if err != nil {
		return err
	}
	models, err := f.fleetModels.get(func() ([]fleetModel, error) {
		return list(ctx, s, func(r scanner) (fleetModel, error) {
			var m fleetModel
			return m, r.Scan(&m.id, &m.Name, &m.Slug, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &m.zones, &m.mode, &m.BudgetPassSlot)
		}, `SELECT id, name, slug, default_limit, default_window, cost_expression, fleet_zones, spent_mode, budget_pass_slot FROM models WHERE fleet ORDER BY name`)
	})
	if err != nil {
		return err
	}
	for _, fm := range models {
		i, ok := index[fm.id]
		if !ok {
			i = len(st.Models)
			index[fm.id] = i
			st.Models = append(st.Models, fm.Model)
		}
		if fm.mode == SpentBestEffort {
			st.Models[i].BestEffort = true
			st.Models[i].Overage = slices.Clone(bestEffort[fm.id])
		}
		if !s.FleetConfigured() {
			st.Models[i].HeldReason = heldNoConfig
			continue
		}
		var sites []render.FleetSite
		for _, z := range fm.zones {
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
	return nil
}

// peer is how other sites reach a fleet cluster.
type peer struct {
	host string
	port int
}

// fleetModel is a model with an entry route, as addFleetModels reads it.
type fleetModel struct {
	render.Model
	id, mode string
	zones    []weights.Zone
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
	st, err := s.RenderStateWithoutKeys(ctx, id)
	if err != nil {
		return "", err
	}
	return render.FleetRevision(st), nil
}

// addPrices gives every priced model the cost expression of its prices, in
// place of one that was typed in, and its dry-run setting.
func (s *Store) addPrices(ctx context.Context, f *fleetReads, st *render.State, index map[string]int) error {
	prices, err := f.prices.get(func() (modelPrices, error) { return s.modelPrices(ctx) })
	if err != nil || len(prices.current) == 0 {
		return err
	}
	dryRun, err := f.dryRun.get(func() ([]string, error) {
		rows, err := s.db.Query(ctx, `SELECT id FROM models WHERE price_dry_run`)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[string])
	})
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
