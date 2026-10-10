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
	// Overage marks the counter of the model's best-effort route: what was
	// used after the budget was spent.
	Overage bool `json:"overage,omitempty"`
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
	// Unit is what Limit, Used and OverageUsed are counted in: "tokens", or
	// "credits" for a model with prices. A credit is 0.00001 dollars.
	Unit string `json:"unit"`
	// DryRun is set while the model's prices are tried out: the tenant is
	// counted and not refused.
	DryRun bool `json:"dry_run,omitempty"`
	// Used is the highest counter: each counter is held to the limit on its own.
	// It leaves out what was used as best-effort, which is OverageUsed.
	Used        int64     `json:"used"`
	OverageUsed int64     `json:"overage_used"`
	ResetsAt    time.Time `json:"resets_at"`
	Counters    []Counter `json:"counters"`
	// BestEffortUntil is set while the tenant's requests for the model are
	// served as best-effort.
	BestEffortUntil *time.Time `json:"best_effort_until,omitempty"`
	// BestEffortLimit is the most the model lets a tenant use as
	// best-effort in one period, when it sets one. BestEffortCapped is true
	// once the tenant reached it: it is refused until the period ends.
	BestEffortLimit  *int64 `json:"best_effort_limit,omitempty"`
	BestEffortCapped bool   `json:"best_effort_capped,omitempty"`
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
	Unit      string    `json:"unit"`
	// DryRun is set while the model's prices are tried out. Limit is then
	// the largest there is and not the model's pool.
	DryRun bool `json:"dry_run,omitempty"`
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
	// EndedOverage is true when the tenant was being served as best-effort
	// on the model and the reset put it back.
	EndedOverage bool `json:"ended_overage"`
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

// Report returns, for every tenant quota and model pool, the tokens used in
// the current window as the gateways' rate limit services count them.
// tenantID limits it to the quotas of one tenant.
func (s *Service) Report(ctx context.Context, tenantID string) (Report, error) {
	return s.ReportAt(ctx, tenantID, time.Now())
}

// ReportAt is Report for the windows that contain now. The counters of a
// window that ended stay in Redis for a while, which is how the last usage
// of a window is still read after it.
func (s *Service) ReportAt(ctx context.Context, tenantID string, now time.Time) (Report, error) {
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

	periods, err := s.overagePeriods(ctx)
	if err != nil {
		return rep, err
	}
	for _, b := range buckets {
		var used, overage int64
		counters := make([]Counter, 0, len(b.keys))
		for _, k := range b.keys {
			counters = append(counters, Counter{Backend: k.backend, Clusters: k.clusters, Used: values[k.redis], Overage: k.overage})
			if k.overage {
				overage = max(overage, values[k.redis])
			} else {
				used = max(used, values[k.redis])
			}
		}
		resets := render.WindowEnd(b.window, now)
		if b.tenantSlug == "" {
			rep.Pools = append(rep.Pools, Pool{
				ModelID: b.modelID, ModelName: b.modelName, Limit: b.limit, Window: b.window,
				Used: used, ResetsAt: resets, Counters: counters, Unit: b.unit, DryRun: b.dryRun,
			})
			continue
		}
		q := Quota{
			TenantID: b.tenantID, TenantSlug: b.tenantSlug, ModelID: b.modelID, ModelName: b.modelName,
			Limit: b.limit, Window: b.window, Shadow: b.shadow, Unit: b.unit, DryRun: b.dryRun,
			Used: used, OverageUsed: overage, ResetsAt: resets, Counters: counters,
			BestEffortLimit: b.bestEffortLimit,
		}
		if o, ok := periods[[2]string{b.modelID, b.tenantID}]; ok {
			q.BestEffortUntil, q.BestEffortCapped = &o.Until, o.CappedAt != nil
		}
		rep.Quotas = append(rep.Quotas, q)
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

// overagePeriods returns the running overage periods by model and tenant ID.
func (s *Service) overagePeriods(ctx context.Context) (map[[2]string]store.Overage, error) {
	active, err := s.st.ActiveOverage(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[[2]string]store.Overage, len(active))
	for _, o := range active {
		out[[2]string{o.ModelID, o.TenantID}] = o
	}
	return out, nil
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
		// The overage ends first. If Redis then fails, nothing is half
		// done: the tenant is still past its budget and the loop moves it
		// again at its next look.
		res.EndedOverage, err = s.st.EndOverage(ctx, modelID, tenantID)
		if err != nil {
			return res, err
		}
		res.Deleted, err = s.rd.Delete(ctx, keys)
		if err != nil {
			return res, RedisError{fmt.Errorf("reset usage: %w", err)}
		}
		return res, nil
	}
	return ResetResult{}, ErrNoQuota
}
