package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"aigw-ui/internal/render"
)

type usageCounter struct {
	// Backend is "<namespace>/<AIServiceBackend>". Clusters that use the same
	// name share the counter, which is what makes one budget across sites.
	Backend  string   `json:"backend"`
	Clusters []string `json:"clusters"`
	Used     int64    `json:"used"`
}

type usageRow struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	ModelID    string `json:"model_id"`
	ModelName  string `json:"model_name"`
	Limit      int64  `json:"limit"`
	Window     string `json:"window"`
	Shadow     bool   `json:"shadow"`
	// Used is the highest counter: each counter is held to the limit on its own.
	Used     int64          `json:"used"`
	ResetsAt time.Time      `json:"resets_at"`
	Counters []usageCounter `json:"counters"`
}

// usagePool is the default bucket of a model: every request to the model is
// charged to it, and a tenant whose own quota is used up can still go on
// while the pool has tokens left.
type usagePool struct {
	ModelID   string         `json:"model_id"`
	ModelName string         `json:"model_name"`
	Limit     int64          `json:"limit"`
	Window    string         `json:"window"`
	Used      int64          `json:"used"`
	ResetsAt  time.Time      `json:"resets_at"`
	Counters  []usageCounter `json:"counters"`
}

type usageResponse struct {
	// Enabled is false when the server has no Redis to read from.
	Enabled bool       `json:"enabled"`
	At      time.Time  `json:"at"`
	Quotas  []usageRow `json:"quotas"`
	// Pools is left out when the request asks for one tenant.
	Pools []usagePool `json:"pools"`
	// CanReset reports whether the server is allowed to reset a quota's usage.
	CanReset bool `json:"can_reset"`
	// Hint explains an empty result when it can.
	Hint string `json:"hint,omitempty"`
}

// quotaCounter is one Redis counter of a tenant quota and the clusters that
// count in it.
type quotaCounter struct {
	tenant, model, backend, redis string
	clusters                      []string
}

// quotaCounters works out, from the desired state of every cluster, which
// Redis keys hold the usage of the current window. It returns one row per
// tenant quota without usage yet, and the counters in a stable order.
// tenantID limits it to one tenant when not empty.
func (s *Server) quotaCounters(ctx context.Context, now time.Time, tenantID string) (map[[2]string]*usageRow, []*quotaCounter, error) {
	clusters, err := s.st.ListClusters(ctx)
	if err != nil {
		return nil, nil, err
	}
	models, err := s.st.ListModels(ctx)
	if err != nil {
		return nil, nil, err
	}
	tenants, err := s.st.ListTenants(ctx)
	if err != nil {
		return nil, nil, err
	}
	modelBySlug := map[string][2]string{}
	for _, m := range models {
		modelBySlug[m.Slug] = [2]string{m.ID, m.Name}
	}
	tenantBySlug := map[string]string{}
	for _, t := range tenants {
		tenantBySlug[t.Slug] = t.ID
	}

	rows := map[[2]string]*usageRow{}
	byKey := map[string]*quotaCounter{}
	var counters []*quotaCounter
	for _, c := range clusters {
		state, err := s.st.RenderState(ctx, c.ID, false)
		if err != nil {
			return nil, nil, err
		}
		for _, ct := range render.Counters(state) {
			id, model := tenantBySlug[ct.TenantSlug], modelBySlug[ct.ModelSlug]
			if ct.Pool {
				// The pool has no tenant; it is keyed by an empty slug.
				id = "pool"
				if tenantID != "" {
					continue
				}
			}
			if id == "" || model[0] == "" || (tenantID != "" && tenantID != id) {
				continue
			}
			row := [2]string{ct.TenantSlug, ct.ModelSlug}
			if rows[row] == nil {
				rows[row] = &usageRow{
					TenantID: id, TenantSlug: ct.TenantSlug, ModelID: model[0], ModelName: model[1],
					Limit: ct.Limit, Window: ct.Window, Shadow: ct.Shadow,
					ResetsAt: render.WindowEnd(ct.Window, now), Counters: []usageCounter{},
				}
			}
			key := ct.RedisKey(s.usageReader.Prefix, now)
			qc := byKey[key]
			if qc == nil {
				qc = &quotaCounter{tenant: ct.TenantSlug, model: ct.ModelSlug, backend: ct.Backend, redis: key}
				byKey[key] = qc
				counters = append(counters, qc)
			}
			qc.clusters = append(qc.clusters, c.Name)
		}
	}
	return rows, counters, nil
}

// usage reports, for every tenant quota, the tokens used in the current
// window as counted by the gateways' rate limit services in Redis.
// ?tenant_id= limits it to one tenant.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	resp := usageResponse{Enabled: s.usageReader != nil, At: now.UTC(), Quotas: []usageRow{}, Pools: []usagePool{}}
	if s.usageReader == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.CanReset = s.usageReader.AllowReset

	rows, counters, err := s.quotaCounters(ctx, now, r.URL.Query().Get("tenant_id"))
	if err != nil {
		fail(w, err)
		return
	}
	keys := make([]string, 0, len(counters))
	for _, c := range counters {
		keys = append(keys, c.redis)
	}

	values, err := s.usageReader.Values(ctx, keys)
	if err != nil {
		writeError(w, http.StatusBadGateway, "read usage from Redis: "+err.Error())
		return
	}
	for _, c := range counters {
		row := rows[[2]string{c.tenant, c.model}]
		used := values[c.redis]
		row.Counters = append(row.Counters, usageCounter{Backend: c.backend, Clusters: c.clusters, Used: used})
		row.Used = max(row.Used, used)
	}
	for id, row := range rows {
		if id[0] == "" {
			resp.Pools = append(resp.Pools, usagePool{
				ModelID: row.ModelID, ModelName: row.ModelName, Limit: row.Limit, Window: row.Window,
				Used: row.Used, ResetsAt: row.ResetsAt, Counters: row.Counters,
			})
			continue
		}
		resp.Quotas = append(resp.Quotas, *row)
	}
	sort.Slice(resp.Pools, func(i, j int) bool { return resp.Pools[i].ModelName < resp.Pools[j].ModelName })
	sort.Slice(resp.Quotas, func(i, j int) bool {
		a, b := resp.Quotas[i], resp.Quotas[j]
		if a.TenantSlug != b.TenantSlug {
			return a.TenantSlug < b.TenantSlug
		}
		return a.ModelName < b.ModelName
	})

	// No counter at all is either "no traffic in this window" or a sign that
	// the rate limit service names its keys differently. A look at what is in
	// Redis tells the two apart.
	if len(keys) > 0 && len(values) == 0 {
		seen, sample, err := s.usageReader.Sample(ctx)
		switch {
		case err != nil:
		case seen == 0:
			resp.Hint = "Redis holds no quota counters at all. Either no request was counted in the current windows, or the gateways' quota rate limit services write to a different Redis."
		default:
			resp.Hint = "Redis holds quota counters, but none under the expected names. Expected for example " + keys[0] + " and found " + sample + ". If only the start differs, set REDIS_KEY_PREFIX."
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// resetUsage sets the usage of one tenant on one model back to zero for the
// current window, on every cluster that shares the counter. The limit and the
// window stay as they are.
func (s *Server) resetUsage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch {
	case s.usageReader == nil:
		writeError(w, http.StatusConflict, "usage monitoring is off: the server has no Redis configured")
		return
	case !s.usageReader.AllowReset:
		writeError(w, http.StatusForbidden, "resetting usage is turned off; set redis.allowReset (REDIS_ALLOW_RESET=true) to allow it")
		return
	}
	tenantID, modelID := r.PathValue("id"), r.PathValue("model_id")
	rows, counters, err := s.quotaCounters(ctx, time.Now(), tenantID)
	if err != nil {
		fail(w, err)
		return
	}
	var keys []string
	var tenant, model string
	for _, c := range counters {
		if row := rows[[2]string{c.tenant, c.model}]; row.ModelID == modelID && c.tenant != "" {
			keys = append(keys, c.redis)
			tenant, model = row.TenantSlug, row.ModelName
		}
	}
	if len(keys) == 0 {
		writeError(w, http.StatusNotFound, "this tenant has no quota on that model that is applied to a cluster")
		return
	}
	summary := "Reset usage of " + tenant + " on " + model
	deleted, err := s.usageReader.Delete(ctx, keys)
	if err != nil {
		s.log(r, "usage.reset", summary, false, "Redis: "+err.Error())
		writeError(w, http.StatusBadGateway, "reset usage in Redis: "+err.Error())
		return
	}
	s.log(r, "usage.reset", summary, true, fmt.Sprintf("%d of %d counters held usage and were cleared.", deleted, len(keys)))
	slog.Info("usage reset", "tenant", tenant, "model", model, "counters", len(keys), "deleted", deleted)
	writeJSON(w, http.StatusOK, map[string]any{"counters": len(keys), "deleted": deleted})
}
