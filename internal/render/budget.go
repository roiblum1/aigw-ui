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
// global rate limit rule for every tenant, charged after each response with
// the credits the model's prices make of it, which the routes write into the
// request's metadata. Every cluster renders the same policy, and the rate
// limit services count in one shared Redis, so a tenant has one counter
// wherever its requests land (lab budget check, 2026-10-16).
//
// The amount is not in the policy: it is in the tenant's token. The hub
// writes it there when it signs the token (requests_per_unit and unit), the
// gateway's JWT filter puts the token's claims into the request's metadata,
// and the rule takes its limit from there (limit.fromMetadata). It is the
// upstream way to give each tenant its own number without a rule per
// tenant, so the policy is the same size for 1,000 tenants as for one; a
// rule per tenant cost 1.2 KB on every route of the listener, and Envoy
// Gateway takes 256 at most (lab, 2026-10-16). A tenant's list of models
// travels in its token the same way.
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
	// CreditsModelAnnotation names the model of that expression as clients
	// send it, so that a route serving several models charges each one its
	// own prices (ModelCredits).
	CreditsModelAnnotation = "aigw-ui.io/credits-model"
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

const (
	// BudgetClaim and UnitClaim are the claims of a token that carry its
	// tenant's budget across models: the credits and the window, as Envoy
	// names its rate limit units. ModelsClaim lists the models the tenant
	// may call, when it is held to some.
	BudgetClaim = "requests_per_unit"
	UnitClaim   = "unit"
	ModelsClaim = "models"
	// BudgetHeader is where the gateway puts a token's budget, so that the
	// budget rule matches the requests of a tenant that has one and no other.
	BudgetHeader = "x-aigw-budget"
	// jwtMetadata is where the gateway's JWT filter puts a token's claims,
	// under the provider's name.
	jwtMetadata = "envoy.filters.http.jwt_authn"
)

// BudgetUnits is the rate limit unit a token names for each window, as
// Envoy parses it (envoy.type.v3.RateLimitUnit, upper case).
var BudgetUnits = map[string]string{"1s": "SECOND", "1m": "MINUTE", "1h": "HOUR", "1d": "DAY"}

// budgets returns the budgets a gateway can hold a tenant to.
func (s State) budgets() []TenantBudget {
	var out []TenantBudget
	for _, b := range s.Budgets {
		if b.TenantSlug != "" && b.Limit > 0 && BudgetUnits[b.Window] != "" {
			out = append(out, b)
		}
	}
	return out
}

// budgetPolicy renders the rule of every tenant's budget. It matches the
// requests whose token carries a budget, one counter per tenant (Distinct on
// the tenant header), and takes the limit from the token's claims.
//
// When the limit cannot be read from the token the rule's own applies: the
// most the gateway counts in a window, so the budget does not hold, as when
// the shared Redis cannot be reached. That is the case with an Envoy Gateway
// before 1.9.0, whose CRD drops limit.fromMetadata, and with a unit Envoy does
// not know, which the hub never signs. The self-test's budget step then fails;
// a small fallback would have refused every tenant with a budget, the
// self-test's own included, so it could not have told.
//
// The policy is only rendered while the gateway takes tokens only: the
// headers it reads are the ones a token sets. It attaches where the API-key
// policy does.
func budgetPolicy(s State) *unstructured.Unstructured {
	target := map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": s.GatewayName}
	if s.ClientListener != "" {
		target["sectionName"] = s.ClientListener
	}
	rule := map[string]any{
		"clientSelectors": []any{
			map[string]any{"headers": []any{
				map[string]any{"name": BudgetHeader, "type": "RegularExpression", "value": ".+"},
				map[string]any{"name": TenantHeader, "type": "Distinct"},
			}},
		},
		// One bucket across every route of the listener. Without it the
		// gateway keys the bucket by route, which is by model.
		"shared": true,
		"limit": map[string]any{
			"requests":     int64(MaxLimit),
			"unit":         "Day",
			"fromMetadata": map[string]any{"namespace": jwtMetadata, "key": TokenProvider},
		},
		"cost": map[string]any{
			// A request costs nothing by itself; what its answer cost is
			// charged once the stream ends.
			"request": map[string]any{"from": "Number", "number": int64(0)},
			"response": map[string]any{
				"from":     "Metadata",
				"metadata": map[string]any{"namespace": costMetadataNamespace, "key": CreditsKey},
			},
		},
	}
	u := object(egAPI, "BackendTrafficPolicy", s.Namespace, BudgetPolicyName)
	u.Object["spec"] = map[string]any{
		"targetRefs": []any{target},
		"rateLimit":  map[string]any{"type": "Global", "global": map[string]any{"rules": []any{rule}}},
	}
	return u
}

// hasBudgets reports whether the budget policy is rendered: while the
// gateway takes tokens only, and some tenant has a budget. Without one the
// policy is left out, so a cluster without global rate limiting in its
// Envoy Gateway is not asked for it.
func (s State) hasBudgets() bool {
	return s.AuthEnabled && s.Identity.Mode == IdentityTokens && len(s.budgets()) > 0
}

// chargesBudgets reports whether the model's routes write what a request
// cost in credits, which the tenants' budgets across models are charged
// with: a model with prices, unless they are being tried out (DryRun), which
// refuses nobody for them.
func (m Model) chargesBudgets() bool {
	return m.Priced && !m.DryRun && m.CostExpression != ""
}

// creditsCost is the llmRequestCosts entry of a priced model's route: what a
// request cost in credits, for the budget rule to charge.
func creditsCost(m Model) []any {
	return []any{map[string]any{"metadataKey": CreditsKey, "type": "CEL", "cel": m.CostExpression}}
}

// ModelCredits returns one credits expression for a route that serves
// several models, from the expression of each: the CEL variable model is
// the model the request names, which the route's rules match on. Any other
// model costs nothing, as a model without prices does. With one model it is
// that model's expression.
func ModelCredits(byModel map[string]string) string {
	names := make([]string, 0, len(byModel))
	for name := range byModel {
		names = append(names, name)
	}
	if len(names) == 1 {
		return byModel[names[0]]
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString("model == " + celString(name) + " ? (" + byModel[name] + ") : ")
	}
	b.WriteString("0u")
	return b.String()
}

// BudgetCounter is where the gateway's rate limit service keeps what a
// tenant used of its budget. The name follows what Envoy Gateway makes of a
// shared rule with a header match and a Distinct match on the tenant
// header: the policy's domain, its generic key (the policy and the rule's
// position, 0) twice, the first match's descriptor twice, the Distinct one
// and the tenant, and the window start (Envoy Gateway v1.9.2,
// internal/xds/translator/ratelimit.go; seen in the lab's shared Redis,
// 2026-10-16). The amount is not part of it, so a token with a new amount
// counts on in the same key, and the window is the one the hub signs into
// the tenant's tokens.
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
	policy := s.Namespace + "/" + BudgetPolicyName
	rule := policy + "/rule/0"
	stem := policy + "_" + rule + "_" + rule + "_rule-0-match-0_rule-0-match-0_rule-0-match-1_"
	var out []BudgetCounter
	for _, b := range s.budgets() {
		out = append(out, BudgetCounter{TenantSlug: b.TenantSlug, Limit: b.Limit, Window: b.Window, stem: stem + b.TenantSlug + "_"})
	}
	return out
}

// RedisKey returns the key that holds the credits used in the window that
// contains now, as Counter.RedisKey does.
func (c BudgetCounter) RedisKey(prefix string, now time.Time) string {
	return prefix + c.stem + strconv.FormatInt(WindowStart(c.Window, now).Unix(), 10)
}

// modelsRule is the authorization rule that holds a tenant to the models its
// token lists: it refuses a request for any other (HTTP 403). The model is
// read from the header the AI gateway sets from the request body, after the
// token was checked, and the list from the token's claims; a client can set
// neither. A token without the claim may call every model.
//
// The gateway counts an error in the expression as no match, which for a
// Deny rule lets the request through, so every lookup is guarded.
func modelsRule() map[string]any {
	claims := "metadata.filter_metadata['" + jwtMetadata + "']['" + TokenProvider + "']"
	cel := "'" + jwtMetadata + "' in metadata.filter_metadata" +
		" && '" + TokenProvider + "' in metadata.filter_metadata['" + jwtMetadata + "']" +
		" && '" + ModelsClaim + "' in " + claims +
		" && !('" + ModelHeader + "' in request.headers && request.headers['" + ModelHeader + "'] in " + claims + "['" + ModelsClaim + "'])"
	return map[string]any{"name": "models", "action": "Deny", "cel": cel}
}

// celString quotes s as a CEL string literal.
func celString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
