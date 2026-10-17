package render

import (
	"strconv"
	"time"
)

// Token counts. While the gateway takes tokens only, it counts for each
// tenant and model the requests and the input, cached and output tokens of
// their answers, in Redis, the way it counts budgets: four more rules of the
// budget policy on the client listener, one counter per tenant and model an
// hour, which never refuse anything (shadow mode, and the most the gateway
// counts as their limit). The AI gateway writes each answer's token counts
// into the request's metadata under the keys below (llmRequestCosts of type
// InputToken, CachedInputToken and OutputToken); the rules add them up when
// the answer ends. The hub reads the counters for the tenant's own page.
// They are numbers: no prompt and no answer reaches the hub.
//
// The policy keeps the budget rule first, also without any budget, so that
// the token rules after it, and the counters named after their positions,
// never move.
const (
	// InputTokensKey is where a route writes an answer's input tokens, the
	// cached ones included.
	InputTokensKey = "aigw_ui_input"
	// CachedTokensKey is where it writes the input tokens served from the
	// model server's cache.
	CachedTokensKey = "aigw_ui_cached"
	// OutputTokensKey is where it writes the output tokens.
	OutputTokensKey = "aigw_ui_output"
	// TokenCountsAnnotation marks the budget policy while it counts tokens,
	// so that the sync also gives the token keys to the routes of the
	// cluster's own it marks (kube.markRoutes).
	TokenCountsAnnotation = "aigw-ui.io/token-counts"
	// tokenWindow is the window of every token counter.
	tokenWindow = "1h"
)

// TokenKind is what a token counter counts.
type TokenKind string

const (
	TokenRequests TokenKind = "requests"
	TokenInput    TokenKind = "input"
	TokenCached   TokenKind = "cached"
	TokenOutput   TokenKind = "output"
)

// TokenKinds are the token counters in the order of their rules: the rule
// of TokenKinds[i] is the policy's rule i+1, after the budget rule.
var TokenKinds = []TokenKind{TokenRequests, TokenInput, TokenCached, TokenOutput}

// tokenCostKeys maps each kind counted from an answer to its metadata key
// and the AI gateway's cost type.
var tokenCostKeys = []struct {
	kind          TokenKind
	key, costType string
}{
	{TokenInput, InputTokensKey, "InputToken"},
	{TokenCached, CachedTokensKey, "CachedInputToken"},
	{TokenOutput, OutputTokensKey, "OutputToken"},
}

// TokenWindow is the window of every token counter.
func TokenWindow() string { return tokenWindow }

// CountsTokens reports whether the cluster's gateway counts tokens by tenant
// and model: the hub reads the counters (TokenCounts) and the gateway takes
// tokens only, which write the tenant header the rules read.
func (s State) CountsTokens() bool {
	return s.TokenCounts && s.AuthEnabled && s.Identity.Mode == IdentityTokens
}

// TokenCosts are the request costs a route carries for the token counters,
// in the order of their rules.
func TokenCosts() []any {
	out := make([]any, 0, len(tokenCostKeys))
	for _, c := range tokenCostKeys {
		out = append(out, map[string]any{"metadataKey": c.key, "type": c.costType})
	}
	return out
}

// tokenRules are the rules of the budget policy that count tokens, one per
// kind: each tenant and model has a counter of its own (Distinct on the
// tenant header and on the model header the AI gateway sets from the
// request body), across every route of the listener (shared), by the hour.
func tokenRules() []any {
	var out []any
	for _, kind := range TokenKinds {
		rule := map[string]any{
			"clientSelectors": []any{
				map[string]any{"headers": []any{
					map[string]any{"name": TenantHeader, "type": "Distinct"},
					map[string]any{"name": ModelHeader, "type": "Distinct"},
				}},
			},
			"shared":     true,
			"shadowMode": true,
			"limit":      map[string]any{"requests": int64(MaxLimit), "unit": "Hour"},
		}
		cost := map[string]any{"request": map[string]any{"from": "Number", "number": int64(1)}}
		for _, c := range tokenCostKeys {
			if c.kind == kind {
				// An answer's tokens are charged once it ends; the request
				// itself adds nothing.
				cost = map[string]any{
					"request": map[string]any{"from": "Number", "number": int64(0)},
					"response": map[string]any{
						"from":     "Metadata",
						"metadata": map[string]any{"namespace": costMetadataNamespace, "key": c.key},
					},
				}
			}
		}
		rule["cost"] = cost
		out = append(out, rule)
	}
	return out
}

// TokenCounterKey is the Redis key of a tenant's counter of one kind on a
// model, in the hour that contains now. model is the model's name as
// clients send it. prefix is the rate limit service's CACHE_KEY_PREFIX.
//
// The rate limit service names a key after the policy (the domain of a
// shared rule), then each entry of the descriptor as key_value: the rule's
// own name twice, then one entry per Distinct header, rule-<i>-match-<j>
// with the request's value, then the window's start (envoyproxy/ratelimit
// src/limiter/cache_key.go; Envoy Gateway 1.9.2
// internal/xds/translator/ratelimit.go). The budget rule's key, seen in the
// shared Redis, has the same form.
func TokenCounterKey(prefix, namespace string, kind TokenKind, tenantSlug, model string, now time.Time) string {
	i := 0
	for n, k := range TokenKinds {
		if k == kind {
			i = n + 1
		}
	}
	policy := namespace + "/" + BudgetPolicyName
	rule := policy + "/rule/" + strconv.Itoa(i)
	match := "rule-" + strconv.Itoa(i) + "-match-"
	return prefix + policy + "_" + rule + "_" + rule + "_" + match + "0_" + tenantSlug + "_" + match + "1_" + model + "_" +
		strconv.FormatInt(WindowStart(tokenWindow, now).Unix(), 10)
}
