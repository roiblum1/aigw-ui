package store

import (
	"context"
	"fmt"

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
		`SELECT m.id, m.name, m.slug, e.host, e.port, e.upstream_model, m.default_limit, m.default_window, m.cost_expression, e.source, e.backends
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
		if err := rows.Scan(&id, &m.Name, &m.Slug, &m.Host, &m.Port, &m.UpstreamModel, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &source, &backends); err != nil {
			rows.Close()
			return st, err
		}
		if source == SourceDiscovered {
			m.Existing = make([]render.Target, 0, len(backends))
			for _, b := range backends {
				m.Existing = append(m.Existing, render.Target{Namespace: b.Namespace, Backend: b.Name, Model: b.Model})
			}
		}
		index[id] = len(st.Models)
		st.Models = append(st.Models, m)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if c.FleetEnabled && s.FleetConfigured() {
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

// addFleetModels gives every model with an entry route its sites, adding the
// models this cluster does not serve itself: the entry route is on every
// fleet cluster. index maps a model's ID to its position in st.Models.
//
// It fails when a model would be left without a site, so the sync stops and
// the cluster keeps the objects it has.
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

	rows, err = s.db.Query(ctx,
		`SELECT id, name, slug, default_limit, default_window, cost_expression, fleet_zones FROM models WHERE fleet ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var m render.Model
		var zones []weights.Zone
		if err := rows.Scan(&id, &m.Name, &m.Slug, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &zones); err != nil {
			return err
		}
		var sites []render.FleetSite
		for _, z := range zones {
			if p, ok := peers[z.Zone]; ok {
				sites = append(sites, render.FleetSite{Name: z.Zone, Host: p.host, Port: p.port, Weight: z.Weight})
			}
		}
		if len(sites) == 0 {
			return fmt.Errorf("model %s has an entry route and no site to send to; nothing was changed on the cluster", m.Name)
		}
		if i, ok := index[id]; ok {
			st.Models[i].Fleet = sites
			continue
		}
		m.Fleet = sites
		index[id] = len(st.Models)
		st.Models = append(st.Models, m)
	}
	return rows.Err()
}
