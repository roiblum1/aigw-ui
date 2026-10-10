package usage

import (
	"context"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// bucket is a quota or a pool being put together: what is known from
// Postgres, and the Redis keys that hold its usage. tenantSlug is empty for a
// model's pool.
type bucket struct {
	tenantID, tenantSlug string
	modelID, modelName   string
	limit                int64
	window               string
	shadow               bool
	keys                 []*key
}

// key is one Redis key and the clusters that count in it.
type key struct {
	backend, redis string
	clusters       []string
	overage        bool
}

// buckets works out, from the desired state of every cluster, which quotas
// and pools exist and which Redis keys hold their usage in the window that
// contains now. tenantID limits it to the quotas of one tenant.
func (s *Service) buckets(ctx context.Context, now time.Time, tenantID string) ([]*bucket, error) {
	clusters, err := s.st.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	models, err := s.st.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	tenants, err := s.st.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	modelBySlug := make(map[string]store.Model, len(models))
	for _, m := range models {
		modelBySlug[m.Slug] = m
	}
	tenantIDBySlug := make(map[string]string, len(tenants))
	for _, t := range tenants {
		tenantIDBySlug[t.Slug] = t.ID
	}

	type bucketID struct{ tenantSlug, modelSlug string }
	byID := map[bucketID]*bucket{}
	byRedisKey := map[string]*key{}
	var out []*bucket
	for _, c := range clusters {
		state, err := s.st.RenderStateWithoutKeys(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, ct := range render.Counters(state) {
			model, ok := modelBySlug[ct.ModelSlug]
			if !ok {
				continue
			}
			id := ""
			if ct.Pool {
				// The default bucket of a best-effort route only makes
				// sure nobody is refused there. It is not the model's pool.
				if tenantID != "" || ct.Overage {
					continue
				}
			} else if id = tenantIDBySlug[ct.TenantSlug]; id == "" || (tenantID != "" && tenantID != id) {
				continue
			}
			b := byID[bucketID{ct.TenantSlug, ct.ModelSlug}]
			if b == nil {
				b = &bucket{
					tenantID: id, tenantSlug: ct.TenantSlug, modelID: model.ID, modelName: model.Name,
					limit: ct.Limit, window: ct.Window, shadow: ct.Shadow,
				}
				byID[bucketID{ct.TenantSlug, ct.ModelSlug}] = b
				out = append(out, b)
			}
			redisKey := ct.RedisKey(s.rd.Prefix, now)
			k := byRedisKey[redisKey]
			if k == nil {
				k = &key{backend: ct.Backend, redis: redisKey, overage: ct.Overage}
				byRedisKey[redisKey] = k
				b.keys = append(b.keys, k)
			}
			k.clusters = append(k.clusters, c.Name)
		}
	}
	return out, nil
}
