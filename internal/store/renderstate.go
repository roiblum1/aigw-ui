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

	rows, err := s.db.Query(ctx,
		`SELECT m.id, m.name, m.slug, e.host, e.port, e.upstream_model, m.default_limit, m.default_window, m.cost_expression, e.source, e.backends,
		        e.pools, m.fleet AND m.spent_mode = 'best-effort'
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
		if err := rows.Scan(&id, &m.Name, &m.Slug, &m.Host, &m.Port, &m.UpstreamModel, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &source, &backends,
			&pools, &bestEffort); err != nil {
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
		if err := s.addFleetModels(ctx, &st, index); err != nil {
			return st, err
		}
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
		 WHERE k.revoked_at IS NULL AND t.enabled ORDER BY k.client_id`)
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
		`SELECT id, name, slug, default_limit, default_window, cost_expression, fleet_zones, spent_mode FROM models WHERE fleet ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, mode string
		var m render.Model
		var zones []weights.Zone
		if err := rows.Scan(&id, &m.Name, &m.Slug, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &zones, &mode); err != nil {
			return err
		}
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
