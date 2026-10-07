package usage

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

var (
	// ErrResetDisabled is returned when the server may only read from Redis.
	ErrResetDisabled = errors.New("resetting usage is turned off")
	// ErrNoQuota is returned when the tenant has no quota on the model that
	// is applied to a cluster, so there is no counter to reset.
	ErrNoQuota = errors.New("no applied quota for that tenant and model")
)

// RedisError marks a failure to talk to Redis, as opposed to a bad request or
// a database error.
type RedisError struct{ Err error }

func (e RedisError) Error() string { return e.Err.Error() }
func (e RedisError) Unwrap() error { return e.Err }

// Counter is one key in Redis. Clusters that name the backend the same way
// share it, which is what makes one budget across sites.
type Counter struct {
	// Backend is "<namespace>/<AIServiceBackend>".
	Backend  string   `json:"backend"`
	Clusters []string `json:"clusters"`
	Used     int64    `json:"used"`
}

// Quota is the usage of one tenant on one model in the current window.
type Quota struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	ModelID    string `json:"model_id"`
	ModelName  string `json:"model_name"`
	Limit      int64  `json:"limit"`
	Window     string `json:"window"`
	Shadow     bool   `json:"shadow"`
	// Used is the highest counter: each counter is held to the limit on its own.
	Used     int64     `json:"used"`
	ResetsAt time.Time `json:"resets_at"`
	Counters []Counter `json:"counters"`
}

// Pool is the default bucket of a model: every request to the model is
// charged to it, and a tenant whose own quota is used up can still go on
// while the pool has tokens left.
type Pool struct {
	ModelID   string    `json:"model_id"`
	ModelName string    `json:"model_name"`
	Limit     int64     `json:"limit"`
	Window    string    `json:"window"`
	Used      int64     `json:"used"`
	ResetsAt  time.Time `json:"resets_at"`
	Counters  []Counter `json:"counters"`
}

type Report struct {
	At     time.Time `json:"at"`
	Quotas []Quota   `json:"quotas"`
	// Pools is empty when the report is for one tenant.
	Pools []Pool `json:"pools"`
	// Hint explains an empty result when it can.
	Hint string `json:"hint,omitempty"`
}

// ResetResult says how many counters a reset covered and how many of them
// existed, that is, held any usage.
type ResetResult struct {
	TenantSlug string `json:"-"`
	ModelName  string `json:"-"`
	Counters   int    `json:"counters"`
	Deleted    int64  `json:"deleted"`
}

// Service turns the quotas stored in Postgres and the counters stored in
// Redis into a usage report.
type Service struct {
	st *store.Store
	rd *Reader
}

func NewService(st *store.Store, rd *Reader) *Service { return &Service{st: st, rd: rd} }

// CanReset reports whether Reset is allowed.
func (s *Service) CanReset() bool { return s.rd.AllowReset }

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
				if tenantID != "" {
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
				k = &key{backend: ct.Backend, redis: redisKey}
				byRedisKey[redisKey] = k
				b.keys = append(b.keys, k)
			}
			k.clusters = append(k.clusters, c.Name)
		}
	}
	return out, nil
}

// Report returns, for every tenant quota and model pool, the tokens used in
// the current window as the gateways' rate limit services count them.
// tenantID limits it to the quotas of one tenant.
func (s *Service) Report(ctx context.Context, tenantID string) (Report, error) {
	now := time.Now()
	rep := Report{At: now.UTC(), Quotas: []Quota{}, Pools: []Pool{}}
	buckets, err := s.buckets(ctx, now, tenantID)
	if err != nil {
		return rep, err
	}
	var keys []string
	for _, b := range buckets {
		for _, k := range b.keys {
			keys = append(keys, k.redis)
		}
	}
	values, err := s.rd.Values(ctx, keys)
	if err != nil {
		return rep, RedisError{err}
	}

	for _, b := range buckets {
		var used int64
		counters := make([]Counter, 0, len(b.keys))
		for _, k := range b.keys {
			counters = append(counters, Counter{Backend: k.backend, Clusters: k.clusters, Used: values[k.redis]})
			used = max(used, values[k.redis])
		}
		resets := render.WindowEnd(b.window, now)
		if b.tenantSlug == "" {
			rep.Pools = append(rep.Pools, Pool{
				ModelID: b.modelID, ModelName: b.modelName, Limit: b.limit, Window: b.window,
				Used: used, ResetsAt: resets, Counters: counters,
			})
			continue
		}
		rep.Quotas = append(rep.Quotas, Quota{
			TenantID: b.tenantID, TenantSlug: b.tenantSlug, ModelID: b.modelID, ModelName: b.modelName,
			Limit: b.limit, Window: b.window, Shadow: b.shadow,
			Used: used, ResetsAt: resets, Counters: counters,
		})
	}
	sort.Slice(rep.Pools, func(i, j int) bool { return rep.Pools[i].ModelName < rep.Pools[j].ModelName })
	sort.Slice(rep.Quotas, func(i, j int) bool {
		a, b := rep.Quotas[i], rep.Quotas[j]
		if a.TenantSlug != b.TenantSlug {
			return a.TenantSlug < b.TenantSlug
		}
		return a.ModelName < b.ModelName
	})

	if len(keys) > 0 && len(values) == 0 {
		rep.Hint = s.emptyHint(ctx, keys[0])
	}
	return rep, nil
}

// emptyHint explains why no counter was found. That is either "no traffic in
// this window" or a sign that the rate limit service names its keys
// differently; a look at what is in Redis tells the two apart.
func (s *Service) emptyHint(ctx context.Context, expected string) string {
	seen, sample, err := s.rd.Sample(ctx)
	switch {
	case err != nil:
		return ""
	case seen == 0:
		return "Redis holds no quota counters at all. Either no request was counted in the current windows, or the gateways' quota rate limit services write to a different Redis."
	}
	return "Redis holds quota counters, but none under the expected names. Expected for example " + expected +
		" and found " + sample + ". If only the start differs, set REDIS_KEY_PREFIX."
}

// Reset sets the usage of one tenant on one model back to zero for the
// current window, on every cluster that shares the counter. The limit and
// the window stay as they are. A model's pool is never touched.
func (s *Service) Reset(ctx context.Context, tenantID, modelID string) (ResetResult, error) {
	if !s.rd.AllowReset {
		return ResetResult{}, ErrResetDisabled
	}
	buckets, err := s.buckets(ctx, time.Now(), tenantID)
	if err != nil {
		return ResetResult{}, err
	}
	for _, b := range buckets {
		if b.modelID != modelID || b.tenantSlug == "" {
			continue
		}
		res := ResetResult{TenantSlug: b.tenantSlug, ModelName: b.modelName, Counters: len(b.keys)}
		keys := make([]string, 0, len(b.keys))
		for _, k := range b.keys {
			keys = append(keys, k.redis)
		}
		res.Deleted, err = s.rd.Delete(ctx, keys)
		if err != nil {
			return res, RedisError{fmt.Errorf("reset usage: %w", err)}
		}
		return res, nil
	}
	return ResetResult{}, ErrNoQuota
}
