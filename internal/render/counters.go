package render

import (
	"strconv"
	"time"
)

// quotaDomain is the rate limit domain the gateway uses for every QuotaPolicy.
const quotaDomain = "ai-gateway-quota"

// Counter is where the gateway's rate limit service keeps the usage of one
// tenant quota on one backend. The names below follow what the gateway
// generates from a QuotaPolicy (internal/ratelimit/translator in its source)
// and how the rate limit service turns a descriptor into a Redis key.
type Counter struct {
	ModelSlug  string
	TenantSlug string
	// Backend is "<namespace>/<AIServiceBackend name>".
	Backend string
	Limit   int64
	Window  string
	Shadow  bool
	// Pool marks the counter of the model's default bucket, which every
	// request is charged to. TenantSlug is empty for it.
	Pool bool
	// Overage marks a counter of the model's best-effort route: what was
	// used after the budget was spent. It never refuses a request.
	Overage bool
	// Standard marks a tenant's counter under the model's standard, for a
	// tenant without a quota of its own on the model.
	Standard bool
	// Budget marks a counter of a tenant with a budget across models, on a
	// model the budget is charged on: the budget holds the tenant there, so
	// the counter only counts (Shadow).
	Budget bool
	// stem is the Redis key without the key prefix and the window start;
	// prefix is the part of it every counter of the model's QuotaPolicy on
	// the backend shares, and std the model's standard, if any.
	stem, prefix string
	std          *Budget
}

// StandardFor returns the counter the model's standard keeps for the tenant
// slug on the same backend, from any counter of the model there: for a slug
// that has no quota, such as one a client made up. ok is false when the
// model has no standard.
func (c Counter) StandardFor(slug string) (Counter, bool) {
	if c.std == nil {
		return Counter{}, false
	}
	return Counter{ModelSlug: c.ModelSlug, TenantSlug: slug, Backend: c.Backend, Limit: c.std.Limit, Window: c.std.Window,
		Overage: c.Overage, Standard: true, stem: c.prefix + "rule-0-" + TenantHeader + "-match-0_" + slug + "_", prefix: c.prefix, std: c.std}, true
}

// Counters lists the counter of every tenant quota the state renders.
func Counters(s State) []Counter {
	var out []Counter
	budgeted := map[string]bool{}
	for _, b := range s.budgets() {
		budgeted[b.TenantSlug] = true
	}
	for _, m := range s.Models {
		// held reports whether the tenant's budget across models holds it on
		// the model instead of the model's rules.
		held := func(slug string) bool { return m.passesBudgets() && budgeted[slug] }
		rules := tenantRules(m)
		seen := map[string]bool{}
		targets := m.targets()
		overageAt := -1
		if t, ok := m.overageTarget(); ok {
			overageAt = len(targets)
			targets = append(targets[:len(targets):len(targets)], t)
		}
		for n, t := range targets {
			overage := n == overageAt
			ns := t.Namespace
			if ns == "" {
				ns = s.Namespace
			}
			backend := ns + "/" + t.Backend
			if seen[backend+"\x00"+t.Model] {
				continue
			}
			seen[backend+"\x00"+t.Model] = true
			prefix := quotaDomain + "_backend_name_" + backend + "_model_name_override_" + t.Model + "_"
			st := m.standard()
			if m.ruleCount() > 0 || overage {
				// The default bucket is named after the number of rules before it.
				limit, window := m.defaultBucket(overage)
				pool := "rule-" + strconv.Itoa(m.ruleCount()) + "-match--1"
				out = append(out, Counter{
					ModelSlug: m.Slug, Backend: backend, Limit: limit, Window: window, Pool: true, Overage: overage,
					stem: prefix + pool + "_" + pool + "_", prefix: prefix, std: st,
				})
			}
			first := 0
			if st != nil {
				// The standard's rule is the first. The gateway names a
				// bucket of it after the rule and the tenant the header
				// held: one counter per tenant. A tenant with a quota is
				// counted there too, and is read where its quota counts.
				first = 1
				own := map[string]bool{}
				for _, q := range rules {
					own[q.TenantSlug] = true
				}
				for _, slug := range s.Tenants {
					if own[slug] {
						continue
					}
					out = append(out, Counter{
						ModelSlug: m.Slug, TenantSlug: slug, Backend: backend,
						Limit: st.Limit, Window: st.Window, Shadow: overage || m.DryRun || held(slug), Overage: overage, Standard: true,
						Budget: held(slug), stem: prefix + "rule-0-" + TenantHeader + "-match-0_" + slug + "_", prefix: prefix, std: st,
					})
				}
			}
			if m.ByAmount {
				// Each tenant is counted on its own under its amount's rule.
				for _, q := range m.Quotas {
					a, slot, ok := amountOf(m, q)
					if q.TenantSlug == "" || !ok {
						continue
					}
					out = append(out, Counter{
						ModelSlug: m.Slug, TenantSlug: q.TenantSlug, Backend: backend,
						Limit: q.Limit, Window: q.Window, Shadow: q.Shadow || overage || m.DryRun || held(q.TenantSlug), Overage: overage,
						Budget: held(q.TenantSlug), stem: amountStem(prefix, first+slot, m, a, q.TenantSlug), prefix: prefix, std: st,
					})
				}
				continue
			}
			for i, q := range rules {
				if q.TenantSlug == "" {
					continue // a placeholder, which counts nothing
				}
				// The gateway names the descriptor of a rule after its position,
				// header and pattern or value, and sends the same string as its
				// value.
				rule := "rule-" + strconv.Itoa(first+i) + "-" + ClientIDHeader + "|" + TenantClientIDPattern(q.TenantSlug) + "-match-0"
				if st != nil {
					rule = "rule-" + strconv.Itoa(first+i) + "-" + TenantHeader + "|" + q.TenantSlug + "-match-0"
				}
				out = append(out, Counter{
					ModelSlug: m.Slug, TenantSlug: q.TenantSlug, Backend: backend,
					Limit: q.Limit, Window: q.Window, Shadow: q.Shadow || overage || m.DryRun || held(q.TenantSlug), Overage: overage,
					Budget: held(q.TenantSlug), stem: prefix + rule + "_" + rule + "_", prefix: prefix, std: st,
				})
			}
		}
	}
	return out
}

func windowSeconds(window string) int64 {
	switch window {
	case "1s":
		return 1
	case "1m":
		return 60
	case "1h":
		return 3600
	}
	return 86400
}

// WindowStart returns when the window that contains now began. Windows are
// aligned to the Unix epoch, so a day starts at 00:00 UTC.
func WindowStart(window string, now time.Time) time.Time {
	d := windowSeconds(window)
	return time.Unix(now.Unix()/d*d, 0).UTC()
}

// WindowEnd returns when the counter of the window that contains now resets.
func WindowEnd(window string, now time.Time) time.Time {
	return WindowStart(window, now).Add(time.Duration(windowSeconds(window)) * time.Second)
}

// RedisKey returns the key that holds the tokens used in the window that
// contains now. prefix is the rate limit service's CACHE_KEY_PREFIX.
func (c Counter) RedisKey(prefix string, now time.Time) string {
	return prefix + c.stem + strconv.FormatInt(WindowStart(c.Window, now).Unix(), 10)
}
