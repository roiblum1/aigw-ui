package usage

import (
	"context"
	"slices"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// HistoryCounter is one counter the usage history reads, and what it holds.
type HistoryCounter struct {
	// Key is the counter's Redis key.
	Key string
	store.UsageKey
	// Kind is store.KindUsed, store.KindBestEffort or a render.TokenKind.
	Kind string
	// Unit is that of a quota's counter, empty for a token counter.
	Unit   string
	Window string
	Value  int64
}

// HistoryCounters returns every counter the history reads in the windows
// that contain now, each with what it holds:
//
//   - each Redis key of every tenant quota, on its own: one per gateway
//     namespace, and one on the best-effort route;
//   - those of a tenant under a model's other amounts, while a token signed
//     before its last change of limits may still name one: the gateway
//     counts that token's requests under the amount it names;
//   - the token counters of each tenant on each model it may call, where the
//     gateway counts tokens. Their requests counter is read first, and the
//     others only where it shows a request or busy says the history already
//     follows the tenant on the model, which keeps the read small;
//   - the same counters of each live token and personal key, by its client
//     ID (0.37.0), read the same way; their UsageKey names the key.
func (s *Service) HistoryCounters(ctx context.Context, now time.Time, busy func(store.UsageKey) bool) ([]HistoryCounter, error) {
	f, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	var out []HistoryCounter
	seen := map[string]bool{}
	add := func(c HistoryCounter) {
		if !seen[c.Key] {
			seen[c.Key] = true
			out = append(out, c)
		}
	}
	buckets, _ := s.bucketsOf(f, now, "")
	for _, b := range buckets {
		if b.tenantSlug == "" {
			continue // a pool is nobody's use
		}
		for _, k := range b.keys {
			add(HistoryCounter{Key: k.redis, UsageKey: store.UsageKey{TenantID: b.tenantID, ModelID: b.modelID},
				Kind: kindOf(k.overage), Unit: b.unit, Window: b.window})
		}
	}
	for _, c := range s.formerAmounts(f, now) {
		add(c)
	}
	tokens, err := s.tokenCounters(ctx, f, now, busy)
	if err != nil {
		return nil, err
	}
	keyTokens, err := s.keyTokenCounters(ctx, f, now, busy)
	if err != nil {
		return nil, err
	}
	tokens = append(tokens, keyTokens...)
	var keys []string
	for _, c := range out {
		keys = append(keys, c.Key)
	}
	values, err := s.rd.Values(ctx, keys)
	if err != nil {
		return nil, RedisError{err}
	}
	for i := range out {
		out[i].Value = values[out[i].Key]
	}
	for _, c := range tokens {
		add(c)
	}
	return out, nil
}

// Values reads counters by key. A key that does not exist holds 0.
func (s *Service) Values(ctx context.Context, keys []string) (map[string]int64, error) {
	values, err := s.rd.Values(ctx, keys)
	if err != nil {
		return nil, RedisError{err}
	}
	return values, nil
}

func kindOf(overage bool) string {
	if overage {
		return store.KindBestEffort
	}
	return store.KindUsed
}

// formerAmounts returns the counters of the tenants whose limits changed
// within a token's lifetime under every amount rule of a model other than
// their own. An amount's rule is there while some tenant holds it, so its
// counters are those of the tenants who do, for another slug.
func (s *Service) formerAmounts(f fleet, now time.Time) []HistoryCounter {
	tokens := s.st.Tokens()
	lifetime := max(tokens.Lifetime, tokens.RenewedLifetime)
	var recent []store.Tenant
	for _, t := range f.tenants {
		if t.Enabled && t.LimitsChangedAt != nil && now.Sub(*t.LimitsChangedAt) < lifetime {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		return nil
	}
	modelBySlug := make(map[string]store.Model, len(f.models))
	for _, m := range f.models {
		modelBySlug[m.Slug] = m
	}
	var out []HistoryCounter
	// Each rule is taken once: every tenant's counter under it gives the
	// same counter for another tenant, and so does every cluster that
	// renders it. Taking each counter in turn made up to 850,000 counters
	// a minute, of which 8,500 differed, at 1,000 tenants, and the hub ran
	// out of memory.
	rules := map[string]bool{}
	for _, state := range f.states {
		for _, ct := range render.Counters(state) {
			m, ok := modelBySlug[ct.ModelSlug]
			if !ok || ct.Pool || ct.Budget || ct.TenantSlug == "" || ct.Rule() == "" || rules[ct.Rule()] {
				continue
			}
			rules[ct.Rule()] = true
			for _, t := range recent {
				if t.Models != nil && !slices.Contains(t.Models, m.ID) {
					continue
				}
				other, _ := ct.For(t.Slug)
				out = append(out, HistoryCounter{Key: other.RedisKey(s.rd.Prefix, now), UsageKey: store.UsageKey{TenantID: t.ID, ModelID: m.ID},
					Kind: kindOf(ct.Overage), Unit: m.Unit, Window: ct.Window})
			}
		}
	}
	return out
}

// counted is a tenant, or one of its keys, whose token counters the
// history reads: by the tenant's slug, or by the key's client ID.
type counted struct {
	tenant store.Tenant
	keyID  string
	value  string
}

// counterKey names a counter of a tenant (render.TokenCounterKey) or of a
// key (render.KeyCounterKey).
type counterKey func(prefix, namespace string, kind render.TokenKind, value, model string, now time.Time) string

// tokenCounters returns the token counters of each enabled tenant on each
// model it may call, on every gateway namespace whose clusters count them,
// with what they hold.
func (s *Service) tokenCounters(ctx context.Context, f fleet, now time.Time, busy func(store.UsageKey) bool) ([]HistoryCounter, error) {
	var who []counted
	for _, t := range f.tenants {
		if t.Enabled {
			who = append(who, counted{tenant: t, value: t.Slug})
		}
	}
	return s.countersOf(ctx, f, now, busy, who, render.TokenCounterKey)
}

// keyTokenCounters returns the same counters of each live token and
// personal key of an enabled tenant, by its client ID, on each model the
// tenant may call. The keys are read only where some cluster counts tokens.
func (s *Service) keyTokenCounters(ctx context.Context, f fleet, now time.Time, busy func(store.UsageKey) bool) ([]HistoryCounter, error) {
	if len(countingNamespaces(f)) == 0 {
		return nil, nil
	}
	keys, err := s.st.CountedKeys(ctx)
	if err != nil {
		return nil, err
	}
	tenants := make(map[string]store.Tenant, len(f.tenants))
	for _, t := range f.tenants {
		tenants[t.ID] = t
	}
	var who []counted
	for _, k := range keys {
		if t, ok := tenants[k.TenantID]; ok && t.Enabled {
			who = append(who, counted{tenant: t, keyID: k.ID, value: k.ClientID})
		}
	}
	return s.countersOf(ctx, f, now, busy, who, render.KeyCounterKey)
}

// countingNamespaces are the gateway namespaces whose clusters count tokens.
func countingNamespaces(f fleet) []string {
	namespaces := map[string]bool{}
	for _, state := range f.states {
		if state.CountsTokens() {
			namespaces[state.Namespace] = true
		}
	}
	return sortedKeys(namespaces)
}

// countersOf returns the token counters of each of who on each model its
// tenant may call, named by keyOf, with what they hold.
func (s *Service) countersOf(ctx context.Context, f fleet, now time.Time, busy func(store.UsageKey) bool, who []counted, keyOf counterKey) ([]HistoryCounter, error) {
	type pair struct {
		key       store.UsageKey
		ns, value string
		modelName string
	}
	var pairs []pair
	var requests []string
	for _, ns := range countingNamespaces(f) {
		for _, c := range who {
			for _, m := range f.models {
				if c.tenant.Models != nil && !slices.Contains(c.tenant.Models, m.ID) {
					continue
				}
				pairs = append(pairs, pair{store.UsageKey{TenantID: c.tenant.ID, ModelID: m.ID, KeyID: c.keyID}, ns, c.value, m.Name})
				requests = append(requests, keyOf(s.rd.Prefix, ns, render.TokenRequests, c.value, m.Name, now))
			}
		}
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	sent, err := s.rd.Values(ctx, requests)
	if err != nil {
		return nil, RedisError{err}
	}
	var out []HistoryCounter
	var keys []string
	for i, p := range pairs {
		if sent[requests[i]] == 0 && !busy(p.key) {
			continue
		}
		for _, kind := range render.TokenKinds {
			key := keyOf(s.rd.Prefix, p.ns, kind, p.value, p.modelName, now)
			out = append(out, HistoryCounter{Key: key, UsageKey: p.key, Kind: string(kind), Window: render.TokenWindow()})
			if kind != render.TokenRequests {
				keys = append(keys, key)
			}
		}
	}
	values, err := s.rd.Values(ctx, keys)
	if err != nil {
		return nil, RedisError{err}
	}
	for i := range out {
		if out[i].Kind == string(render.TokenRequests) {
			out[i].Value = sent[out[i].Key]
		} else {
			out[i].Value = values[out[i].Key]
		}
	}
	return out, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TokenUse returns what a tenant's token counters on a model under a
// gateway namespace hold in the hour that contains now and the one before,
// added up, and the key of its requests counter in this hour.
func (s *Service) TokenUse(ctx context.Context, namespace, tenantSlug, model string, now time.Time) (map[render.TokenKind]int64, string, error) {
	keys := map[string]render.TokenKind{}
	var list []string
	for _, at := range []time.Time{now, now.Add(-time.Hour)} {
		for _, kind := range render.TokenKinds {
			k := render.TokenCounterKey(s.rd.Prefix, namespace, kind, tenantSlug, model, at)
			keys[k] = kind
			list = append(list, k)
		}
	}
	values, err := s.rd.Values(ctx, list)
	if err != nil {
		return nil, list[0], RedisError{err}
	}
	out := map[render.TokenKind]int64{}
	for k, v := range values {
		out[keys[k]] += v
	}
	return out, list[0], nil
}
