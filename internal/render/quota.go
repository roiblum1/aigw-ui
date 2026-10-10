package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// QuotaRevisionAnnotation is set on every AIGatewayRoute this tool renders.
//
// The name has to begin with gateway.envoyproxy.io/. Envoy Gateway builds a
// route again only when something it reads has changed, and of a route's
// annotations it reads the ones with that prefix and no others. Seen on
// Envoy Gateway 1.9.1: an annotation with another prefix reached the
// HTTPRoute and changed nothing in the proxy.
const QuotaRevisionAnnotation = "gateway.envoyproxy.io/aigw-ui-quota-revision"

// quotaRevision identifies the quota rules of a model. It goes on the
// model's routes as an annotation, to work around this in AI Gateway 1.1.0:
// a tenant's rule is enforced through an entry in the proxy's route, and
// that entry is only written when Envoy Gateway builds the route again. A
// changed QuotaPolicy alone does not make it do so: the controller looks at
// the route, finds nothing to change, and the new tenant is not counted or
// limited until something else changes, such as a key.
//
// The controller copies a route's annotations to the HTTPRoute it generates,
// so an annotation that changes with the rules makes the HTTPRoute change,
// and Envoy Gateway builds the route with the new rules.
func quotaRevision(m Model) string {
	limit, window := m.defaultBucket(false)
	data, err := json.Marshal(struct {
		Rules  []TenantQuota
		Limit  int64
		Window string
		Cost   string
		// Left out when unset, so a model keeps the revision it had.
		DryRun bool `json:",omitempty"`
	}{tenantRules(m), limit, window, m.CostExpression, m.DryRun})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:6])
}

// quotaPolicies returns one QuotaPolicy per namespace that holds a backend of
// the model, because a policy can only target backends in its own namespace.
// A model without backends gets none.
func quotaPolicies(s State, m Model) []*unstructured.Unstructured {
	byNamespace := map[string][]Target{}
	var namespaces []string
	for _, t := range m.targets() {
		ns := t.Namespace
		if ns == "" {
			ns = s.Namespace
		}
		if _, ok := byNamespace[ns]; !ok {
			namespaces = append(namespaces, ns)
		}
		byNamespace[ns] = append(byNamespace[ns], t)
	}
	sort.Strings(namespaces)
	out := make([]*unstructured.Unstructured, 0, len(namespaces))
	for _, ns := range namespaces {
		out = append(out, quotaPolicy(ns, m, byNamespace[ns], false))
	}
	return out
}

// quotaPolicy renders the policy of a model for the given backends. With
// overage set it is the policy of the best-effort route: the same tenant
// rules at the same positions, all of them counted and none refused, and a
// default bucket that never runs out.
func quotaPolicy(namespace string, m Model, targets []Target, overage bool) *unstructured.Unstructured {
	quotas := tenantRules(m)
	rules := make([]any, 0, len(quotas))
	for _, q := range quotas {
		if q.TenantSlug == "" {
			rules = append(rules, placeholderRule())
			continue
		}
		rule := map[string]any{
			"clientSelectors": []any{
				map[string]any{"headers": []any{
					map[string]any{"name": ClientIDHeader, "type": "RegularExpression", "value": TenantClientIDPattern(q.TenantSlug)},
				}},
			},
			"quota": map[string]any{"limit": q.Limit, "duration": q.Window},
		}
		if q.Shadow || overage || m.DryRun {
			rule["shadowMode"] = true
		}
		rules = append(rules, rule)
	}
	defaultLimit, defaultWindow := m.defaultBucket(overage)

	// One target per backend and one quota entry per distinct model name.
	var targetRefs, perModel []any
	seenBackend, seenModel := map[string]bool{}, map[string]bool{}
	for _, t := range targets {
		if !seenBackend[t.Backend] {
			seenBackend[t.Backend] = true
			targetRefs = append(targetRefs, map[string]any{"group": "aigateway.envoyproxy.io", "kind": "AIServiceBackend", "name": t.Backend})
		}
		if !seenModel[t.Model] {
			seenModel[t.Model] = true
			quota := map[string]any{
				"mode": "Shared",
				// In Shared mode every request is also charged to the default
				// bucket and passes if any matching bucket has quota left, so
				// this pool is what tenants without their own rule can use.
				"defaultBucket": map[string]any{"limit": defaultLimit, "duration": defaultWindow},
				"bucketRules":   rules,
			}
			if m.CostExpression != "" {
				quota["costExpression"] = m.CostExpression
			}
			perModel = append(perModel, map[string]any{"modelName": t.Model, "quota": quota})
		}
	}

	name := m.Slug
	if overage {
		name = BestEffortName(m.Slug)
	}
	u := object(aigwAPI, "QuotaPolicy", namespace, name)
	u.Object["spec"] = map[string]any{
		"targetRefs":     targetRefs,
		"perModelQuotas": perModel,
		"serviceQuota": map[string]any{
			"quota": map[string]any{"limit": serviceQuotaLimit, "duration": serviceQuotaWindow},
		},
	}
	return u
}

// defaultBucket returns the limit and window of the model's default bucket.
// On the best-effort route it is the largest limit there is: a tenant
// without a rule of its own is counted there and must not be refused. The
// same goes for a model in dry-run.
func (m Model) defaultBucket(overage bool) (int64, string) {
	limit, window := m.DefaultLimit, m.DefaultWindow
	if !validQuota(limit, window) {
		limit, window = fallbackLimit, fallbackWindow
	}
	if overage || m.DryRun {
		limit = serviceQuotaLimit
	}
	return limit, window
}

// tenantRules returns the bucket rules of a model in order: the rule at
// index i belongs to the tenant quota with Slot i. The position of a rule is
// part of its counter's name in Redis, so Counters relies on the same order.
//
// A position no quota holds is returned with an empty TenantSlug and is
// rendered as a placeholder, so the rules after it keep their position and
// with it their counters. Positions after the last quota are left out.
//
// The CRD rejects a quota without a valid limit and duration, which would
// fail the whole sync. Such a tenant gets a placeholder too and falls back to
// the default bucket.
func tenantRules(m Model) []TenantQuota {
	var rules []TenantQuota
	for _, q := range m.Quotas {
		if q.Slot < 0 || !validQuota(q.Limit, q.Window) {
			continue
		}
		for len(rules) <= q.Slot {
			rules = append(rules, TenantQuota{})
		}
		rules[q.Slot] = q
	}
	return rules
}

// placeholderRule fills a position that no tenant quota holds. It matches no
// request.
func placeholderRule() map[string]any {
	return map[string]any{
		"clientSelectors": []any{
			map[string]any{"headers": []any{
				map[string]any{"name": ClientIDHeader, "type": "Exact", "value": placeholderSelector},
			}},
		},
		"quota": map[string]any{"limit": fallbackLimit, "duration": fallbackWindow},
	}
}

// fallbackLimit and fallbackWindow replace a default bucket that has no valid
// value. They match the defaults a new model gets: the tightest bucket.
const (
	fallbackLimit  int64 = 1
	fallbackWindow       = "1d"
)

// validQuota reports whether the QuotaPolicy CRD accepts the limit and duration.
func validQuota(limit int64, window string) bool {
	switch window {
	case "1s", "1m", "1h", "1d":
		return limit > 0
	}
	return false
}

// placeholderSelector is a client ID no key can have: every real one is
// "<tenant>.<hex>".
const placeholderSelector = "aigw-ui-unused-position"
