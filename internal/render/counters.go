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
	// Budget marks a counter of a tenant with a budget across models, on a
	// model the budget is charged on: the budget holds the tenant there, so
	// the counter only counts (Shadow).
	Budget bool
	// stem is the Redis key without the key prefix and the window start;
	// byTenant is the stem without the tenant, for a rule that counts each
	// tenant by the tenant header.
	stem, byTenant string
}

// For returns the counter the same rule keeps for another tenant slug, such
// as one a client made up: ok is false when the rule does not count by the
// tenant header.
func (c Counter) For(slug string) (Counter, bool) {
	if c.byTenant == "" {
		return Counter{}, false
	}
	c.TenantSlug, c.stem = slug, c.byTenant+slug+"_"
	return c, true
}

// Rule names the rule that keeps the counter, for a rule that counts each
// tenant by the tenant header: every tenant's counter under the rule has
// the same name here, and For gives the same counter from any of them.
// Empty for a rule of one tenant.
func (c Counter) Rule() string { return c.byTenant }

// Counters lists every counter the given state creates, in the order of
// s.Models and their targets. A model with its best-effort route has the
// counters of that route too, marked Overage.
func Counters(s State) []Counter {
	// What each model's counters are made from is worked out once per model,
	// not once per target or quota: at 1,000 tenants this was most of the
	// time and memory the usage loops spent here.
	budgets := s.budgets()
	budgeted := map[string]bool{}
	for _, b := range budgets {
		budgeted[b.TenantSlug] = true
	}
	// The models each tenant held to some may call, by name.
	allowed := map[string]map[string]bool{}
	for _, r := range s.Restricted {
		allowed[r.TenantSlug] = map[string]bool{}
		for _, name := range r.Models {
			allowed[r.TenantSlug][name] = true
		}
	}
	size := 0
	for _, m := range s.Models {
		size += (len(m.Existing) + 2) * (1 + len(budgets) + len(m.Quotas))
	}
	out := make([]Counter, 0, size)
	for _, m := range s.Models {
		// held reports whether the tenant's budget across models holds it on
		// the model instead of the model's rules.
		passes := m.passesBudgets()
		held := func(slug string) bool { return passes && budgeted[slug] }
		var rules []TenantQuota
		amounts := map[QuotaAmount]int{}
		if m.ByAmount {
			for slot, a := range amountRules(m) {
				a.Slot = 0
				amounts[a] = slot
			}
		} else {
			rules = tenantRules(m)
		}
		ruleCount, inUse := m.ruleCount(), m.usedPositions() > 0
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
			if inUse || overage {
				// The default bucket is named after the number of rules
				// before it, placeholders included (rulePositions).
				limit, window := m.defaultBucket(overage)
				pool := "rule-" + strconv.Itoa(ruleCount) + "-match--1"
				out = append(out, Counter{
					ModelSlug: m.Slug, Backend: backend, Limit: limit, Window: window, Pool: true, Overage: overage,
					stem: prefix + pool + "_" + pool + "_",
				})
			}
			if passes {
				// Each tenant with a budget across models is counted on its own
				// under the last rule, which never refuses: what it used of
				// the model inside its budget. A tenant with a quota of its own
				// from before is read there too.
				own := map[string]bool{}
				for _, q := range m.Quotas {
					own[q.TenantSlug] = true
				}
				stem := budgetPassStem(prefix, m.budgetPassAt())
				for _, b := range budgets {
					// A tenant held to other models is refused this one by
					// the gateway, so it never uses it.
					if names, held := allowed[b.TenantSlug]; own[b.TenantSlug] || (held && !names[m.Name]) {
						continue
					}
					out = append(out, Counter{
						ModelSlug: m.Slug, TenantSlug: b.TenantSlug, Backend: backend,
						Limit: budgetPassLimit, Window: budgetPassWindow, Shadow: true, Overage: overage, Budget: true,
						stem: stem + b.TenantSlug + "_", byTenant: stem,
					})
				}
			}
			if m.ByAmount {
				// Each tenant is counted on its own under its amount's rule.
				stems := map[int]string{}
				for _, q := range m.Quotas {
					slot, ok := amounts[QuotaAmount{Limit: q.Limit, Window: q.Window, Shadow: q.Shadow}]
					if q.TenantSlug == "" || !ok {
						continue
					}
					stem, ok := stems[slot]
					if !ok {
						stem = amountStem(prefix, slot, m, QuotaAmount{Slot: slot, Limit: q.Limit, Window: q.Window, Shadow: q.Shadow})
						stems[slot] = stem
					}
					out = append(out, Counter{
						ModelSlug: m.Slug, TenantSlug: q.TenantSlug, Backend: backend,
						Limit: q.Limit, Window: q.Window, Shadow: q.Shadow || overage || m.DryRun || held(q.TenantSlug), Overage: overage,
						Budget: held(q.TenantSlug), stem: stem + q.TenantSlug + "_", byTenant: stem,
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
				rule := "rule-" + strconv.Itoa(i) + "-" + ClientIDHeader + "|" + TenantClientIDPattern(q.TenantSlug) + "-match-0"
				out = append(out, Counter{
					ModelSlug: m.Slug, TenantSlug: q.TenantSlug, Backend: backend,
					Limit: q.Limit, Window: q.Window, Shadow: q.Shadow || overage || m.DryRun || held(q.TenantSlug), Overage: overage,
					Budget: held(q.TenantSlug), stem: prefix + rule + "_" + rule + "_",
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
