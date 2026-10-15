package render

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A tenant's budget across every priced model is enforced by the gateway,
// not by the hub: one BackendTrafficPolicy on the client listener with one
// global rate limit rule per tenant, charged after each response with the
// credits the model's prices make of it, which the routes write into the
// request's metadata. Every cluster renders the same policy, and the rate
// limit services count in the hub's Redis, so a tenant has one counter
// wherever its requests land (lab budget check, 2026-10-16).
const (
	// BudgetPolicyName is the one policy that holds every tenant's budget.
	BudgetPolicyName = "aigw-ui-tenant-budgets"
	// CreditsKey is the metadata key under which a route of a priced model
	// writes what a request cost in credits. The rate limit rule reads it.
	CreditsKey = "aigw_ui_credits"
	// CreditsAnnotation carries a priced model's credits expression on its
	// QuotaPolicy, so that the sync can put the cost on a route somebody
	// else renders for the model, the way it marks such a route with the
	// quota revision.
	CreditsAnnotation = "aigw-ui.io/credits-expression"
	// costMetadataNamespace is where the AI gateway writes every request
	// cost.
	costMetadataNamespace = "io.envoy.ai_gateway"
)

// TenantBudget is one tenant's budget across models: credits for each window.
type TenantBudget struct {
	TenantSlug string
	Limit      int64
	Window     string
}

// TenantModels is a tenant that may call only the named models.
type TenantModels struct {
	TenantSlug string
	// Models are the model names, as clients send them. Empty: none.
	Models []string
}

// budgetUnits is the rate limit unit of each window.
var budgetUnits = map[string]string{"1s": "Second", "1m": "Minute", "1h": "Hour", "1d": "Day"}

// budgets returns the budgets the state renders, in the order of their
// rules: by tenant. The position of a rule is part of its counter's name,
// so a budget that moves starts its count over; the owner accepts that.
func (s State) budgets() []TenantBudget {
	var out []TenantBudget
	for _, b := range s.Budgets {
		if b.Limit > 0 && budgetUnits[b.Window] != "" {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TenantSlug < out[j].TenantSlug })
	return out
}

// budgetPolicy renders the tenants' budgets. The policy is only rendered
// while the gateway takes tokens only: its rules read the tenant header a
// token sets. It attaches where the API-key policy does.
func budgetPolicy(s State) *unstructured.Unstructured {
	target := map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": s.GatewayName}
	if s.ClientListener != "" {
		target["sectionName"] = s.ClientListener
	}
	var rules []any
	for _, b := range s.budgets() {
		rules = append(rules, map[string]any{
			"clientSelectors": []any{
				map[string]any{"headers": []any{map[string]any{"name": TenantHeader, "type": "Exact", "value": b.TenantSlug}}},
			},
			// One bucket across every route of the listener. Without it the
			// gateway keys the bucket by route, which is by model.
			"shared": true,
			"limit":  map[string]any{"requests": b.Limit, "unit": budgetUnits[b.Window]},
			"cost": map[string]any{
				// A request costs nothing by itself; what its answer cost is
				// charged once the stream ends.
				"request": map[string]any{"from": "Number", "number": int64(0)},
				"response": map[string]any{
					"from":     "Metadata",
					"metadata": map[string]any{"namespace": costMetadataNamespace, "key": CreditsKey},
				},
			},
		})
	}
	u := object(egAPI, "BackendTrafficPolicy", s.Namespace, BudgetPolicyName)
	u.Object["spec"] = map[string]any{
		"targetRefs": []any{target},
		"rateLimit":  map[string]any{"type": "Global", "global": map[string]any{"rules": rules}},
	}
	return u
}

// hasBudgets reports whether the budget policy is rendered.
func (s State) hasBudgets() bool {
	return s.AuthEnabled && s.Identity.Mode == IdentityTokens && len(s.budgets()) > 0
}

// creditsCost is the llmRequestCosts entry of a priced model's route: what a
// request cost in credits, for the budget rule to charge.
func creditsCost(m Model) []any {
	return []any{map[string]any{"metadataKey": CreditsKey, "type": "CEL", "cel": m.CostExpression}}
}

// BudgetCounter is where the gateway's rate limit service keeps what a
// tenant used of its budget. The name follows what Envoy Gateway makes of a
// shared rule with an Exact header match: the policy's domain, its generic
// key (the policy and the rule's position) twice, the rule's descriptor
// twice, and the window start (seen in the hub's Redis, 2026-10-16).
type BudgetCounter struct {
	TenantSlug string
	Limit      int64
	Window     string
	stem       string
}

// BudgetCounters lists the counter of every budget the state renders.
func BudgetCounters(s State) []BudgetCounter {
	if !s.hasBudgets() {
		return nil
	}
	var out []BudgetCounter
	policy := s.Namespace + "/" + BudgetPolicyName
	for i, b := range s.budgets() {
		rule := policy + "/rule/" + strconv.Itoa(i)
		match := "rule-" + strconv.Itoa(i) + "-match-0"
		out = append(out, BudgetCounter{TenantSlug: b.TenantSlug, Limit: b.Limit, Window: b.Window,
			stem: policy + "_" + rule + "_" + rule + "_" + match + "_" + match + "_"})
	}
	return out
}

// RedisKey returns the key that holds the credits used in the window that
// contains now, as Counter.RedisKey does.
func (c BudgetCounter) RedisKey(prefix string, now time.Time) string {
	return prefix + c.stem + strconv.FormatInt(WindowStart(c.Window, now).Unix(), 10)
}

// modelRules returns the authorization rules that hold a restricted tenant
// to its models: one Deny rule per tenant, which matches its requests for
// any other model. The model is read from the header the AI gateway sets
// from the request body, after it has checked the token; a client cannot
// set either. A model not in the list, such as one added since, is refused.
func modelRules(restricted []TenantModels) []any {
	sorted := append([]TenantModels(nil), restricted...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TenantSlug < sorted[j].TenantSlug })
	var rules []any
	for _, t := range sorted {
		models := make([]string, 0, len(t.Models))
		for _, m := range t.Models {
			models = append(models, celString(m))
		}
		sort.Strings(models)
		cel := "'" + TenantHeader + "' in request.headers && request.headers['" + TenantHeader + "'] == " + celString(t.TenantSlug)
		if len(models) > 0 {
			cel += " && !('" + ModelHeader + "' in request.headers && request.headers['" + ModelHeader + "'] in [" + strings.Join(models, ", ") + "])"
		}
		rules = append(rules, map[string]any{"name": "models-" + t.TenantSlug, "action": "Deny", "cel": cel})
	}
	return rules
}

// celString quotes s as a CEL string literal.
func celString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
