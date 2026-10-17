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
	// budget is set when the tenant's budget across models holds it on the
	// model, not the limit (render.Counter.Budget).
	budget bool
	// unit is what limit and the counters are in. dryRun is set for a model
	// whose prices are being tried out: nobody is refused there.
	unit   string
	dryRun bool
	// bestEffortLimit is the model's limit on best-effort use, if any.
	bestEffortLimit *int64
	keys            []*key
}

// key is one Redis key and the clusters that count in it.
type key struct {
	backend, redis string
	clusters       []string
	overage        bool
}

// budgetBucket is a tenant's budget across models being put together: what
// is known from Postgres, and the Redis keys that hold its usage, one per
// gateway namespace since every cluster with the same namespace renders the
// same policy and counts in the same key.
type budgetBucket struct {
	tenantID, tenantSlug string
	limit                int64
	window               string
	keys                 []*key
}

// fleet is what the counters are worked out from: the clusters' desired
// states, the models and the tenants.
type fleet struct {
	clusters []store.Cluster
	states   []render.State
	models   []store.Model
	tenants  []store.Tenant
}

func (s *Service) load(ctx context.Context) (fleet, error) {
	var f fleet
	var err error
	if f.clusters, err = s.st.ListClusters(ctx); err != nil {
		return f, err
	}
	if f.models, err = s.st.ListModels(ctx); err != nil {
		return f, err
	}
	if f.tenants, err = s.st.ListTenants(ctx); err != nil {
		return f, err
	}
	for _, c := range f.clusters {
		state, err := s.st.RenderStateWithoutKeys(ctx, c.ID)
		if err != nil {
			return f, err
		}
		f.states = append(f.states, state)
	}
	return f, nil
}

// buckets works out, from the desired state of every cluster, which quotas,
// pools and budgets across models exist and which Redis keys hold their
// usage in the window that contains now. tenantID limits it to one tenant.
func (s *Service) buckets(ctx context.Context, now time.Time, tenantID string) ([]*bucket, []*budgetBucket, error) {
	f, err := s.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	out, budgets := s.bucketsOf(f, now, tenantID)
	return out, budgets, nil
}

// bucketsOf is buckets for a fleet already read.
func (s *Service) bucketsOf(f fleet, now time.Time, tenantID string) ([]*bucket, []*budgetBucket) {
	modelBySlug := make(map[string]store.Model, len(f.models))
	for _, m := range f.models {
		modelBySlug[m.Slug] = m
	}
	tenantIDBySlug := make(map[string]string, len(f.tenants))
	for _, t := range f.tenants {
		tenantIDBySlug[t.Slug] = t.ID
	}

	type bucketID struct{ tenantSlug, modelSlug string }
	byID := map[bucketID]*bucket{}
	byRedisKey := map[string]*key{}
	var out []*bucket
	budgetBySlug := map[string]*budgetBucket{}
	var budgets []*budgetBucket
	for i, c := range f.clusters {
		state := f.states[i]
		for _, ct := range render.BudgetCounters(state) {
			id := tenantIDBySlug[ct.TenantSlug]
			if id == "" || (tenantID != "" && tenantID != id) {
				continue
			}
			b := budgetBySlug[ct.TenantSlug]
			if b == nil {
				b = &budgetBucket{tenantID: id, tenantSlug: ct.TenantSlug, limit: ct.Limit, window: ct.Window}
				budgetBySlug[ct.TenantSlug] = b
				budgets = append(budgets, b)
			}
			redisKey := ct.RedisKey(s.rd.Prefix, now)
			k := byRedisKey[redisKey]
			if k == nil {
				k = &key{backend: state.Namespace + "/" + render.BudgetPolicyName, redis: redisKey}
				byRedisKey[redisKey] = k
				b.keys = append(b.keys, k)
			}
			k.clusters = append(k.clusters, c.Name)
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
					limit: ct.Limit, window: ct.Window, shadow: ct.Shadow, budget: ct.Budget,
					unit: model.Unit, dryRun: model.PriceDryRun,
				}
				if model.SpentMode == store.SpentBestEffort {
					b.bestEffortLimit = model.BestEffortLimit
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
	return out, budgets
}

// UsedAs is what the rule that counts the tenant own on the model counted
// for the tenant slug in the window that contains now, the most of any
// backend: 0 for a slug no request named. ok is false when no such rule
// counts own by the tenant header on any cluster.
func (s *Service) UsedAs(ctx context.Context, modelSlug, own, slug string, now time.Time) (used int64, ok bool, err error) {
	clusters, err := s.st.ListClusters(ctx)
	if err != nil {
		return 0, false, err
	}
	var keys []string
	for _, c := range clusters {
		state, err := s.st.RenderStateWithoutKeys(ctx, c.ID)
		if err != nil {
			return 0, false, err
		}
		for _, ct := range render.Counters(state) {
			if ct.ModelSlug != modelSlug || ct.TenantSlug != own || ct.Overage {
				continue
			}
			if other, has := ct.For(slug); has {
				keys = append(keys, other.RedisKey(s.rd.Prefix, now))
			}
		}
	}
	if len(keys) == 0 {
		return 0, false, nil
	}
	values, err := s.rd.Values(ctx, keys)
	if err != nil {
		return 0, true, RedisError{err}
	}
	for _, v := range values {
		used = max(used, v)
	}
	return used, true, nil
}
