package kube

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/render"
)

// The policy of each step of the switch to tokens, as the real Envoy Gateway
// and AI Gateway CRDs take it: their schema and rules check every field, the
// inline key set, the claim rules and the CEL rule among them. Moving to
// tokens only removes the Secret.
func TestRealAPIServerIdentity(t *testing.T) {
	c, ns := realClient(t)
	ctx := context.Background()
	s := fleetTestState(ns, render.FleetSite{Name: "site1-a", Host: "llm.site1-a.example.com", Port: 8443, Weight: 1})
	s.Identity = render.Identity{Mode: render.IdentityBoth, Issuer: "aigw-ui", Audience: "aigw-fleet",
		JWKS:     `{"keys":[{"kty":"EC","crv":"P-256","alg":"ES256","use":"sig","kid":"k1","x":"fd18Ci2gdd5ASrxaWGxQBxw_JoZYsctP7jd89spqbNY","y":"ZJI0p-g0zxmy-HRKAD_uMMi0wc7XY5gDE-Jba_Yu88U"}]}`,
		Disabled: []string{"team-c"}}
	for n := range 130 {
		s.Identity.Denied = append(s.Identity.Denied, fmt.Sprintf("%016x", n))
	}
	// Enough spans of denied tokens for two rules of CEL near its limit.
	for n := range int64(30) {
		s.Identity.DeniedSpans = append(s.Identity.DeniedSpans, render.ExpirySpan{From: 1792254713 + 100*n, To: 1792254763 + 100*n, Tokens: 3})
	}
	if _, err := c.Sync(ctx, ns, render.Objects(s), nil); err != nil {
		t.Fatalf("both: the API server refused a rendered object: %v", err)
	}
	pol, _ := gvrFor("SecurityPolicy")
	live, err := c.dyn.Resource(pol).Namespace(ns).Get(ctx, render.AuthPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rules, _, _ := unstructured.NestedSlice(live.Object, "spec", "authorization", "rules"); len(rules) != 7 {
		t.Fatalf("both: %d rules, want 2 revoked, 2 revoked-span, 1 disabled, token, token-not-checked", len(rules))
	}
	backend, _ := gvrFor("AIServiceBackend")
	b, err := c.dyn.Resource(backend).Namespace(ns).Get(ctx, render.FleetName("glm-5-3"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if removed, _, _ := unstructured.NestedStringSlice(b.Object, "spec", "headerMutation", "remove"); len(removed) != 2 {
		t.Fatalf("backend headers: %v", b.Object["spec"])
	}

	// Tokens only: the Secret and the API key check go.
	s.Identity.Mode = render.IdentityTokens
	res, err := c.Sync(ctx, ns, render.Objects(s), nil)
	if err != nil {
		t.Fatalf("tokens: the API server refused a rendered object: %v", err)
	}
	secret, _ := gvrFor("Secret")
	if _, err := c.dyn.Resource(secret).Namespace(ns).Get(ctx, render.KeysSecretName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("tokens only, yet the Secret is there: %v (changes %+v)", err, res.Changes)
	}
	if live, err = c.dyn.Resource(pol).Namespace(ns).Get(ctx, render.AuthPolicyName, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, has, _ := unstructured.NestedMap(live.Object, "spec", "apiKeyAuth"); has {
		t.Fatal("tokens only, yet the API key check is there")
	}
	qp, _ := gvrFor("QuotaPolicy")
	q, err := c.dyn.Resource(qp).Namespace(ns).Get(ctx, "glm-5-3", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	quotas, _, _ := unstructured.NestedSlice(q.Object, "spec", "perModelQuotas")
	rules := quotas[0].(map[string]any)["quota"].(map[string]any)["bucketRules"].([]any)
	if len(rules) == 0 || strings.Contains(fmt.Sprint(rules), render.TenantHeader) {
		t.Fatalf("rules per tenant: %v", rules)
	}

	// Budgets across models and a tenant held to one model: the budget
	// policy and the route's cost as the Envoy Gateway and AI Gateway CRDs
	// take them, and the CEL rule in the policy.
	s.Models[0].Priced, s.Models[0].CostExpression = true, "input_tokens * 15u"
	s.Budgets = []render.TenantBudget{{TenantSlug: "team-b", Limit: 300_000, Window: "1d"}, {TenantSlug: "team-a", Limit: 100_000, Window: "1h"}}
	s.Restricted = []render.TenantModels{{TenantSlug: "team-a", Models: []string{"GLM5.3"}}}
	s.Models[0].BudgetsHold = true
	if _, err := c.Sync(ctx, ns, render.Objects(s), nil); err != nil {
		t.Fatalf("budgets: the API server refused a rendered object: %v", err)
	}
	// The model's QuotaPolicy lets a tenant with a budget through: its last
	// rule, as the CRD takes it.
	if q, err = c.dyn.Resource(qp).Namespace(ns).Get(ctx, "glm-5-3", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	quotas, _, _ = unstructured.NestedSlice(q.Object, "spec", "perModelQuotas")
	rules = quotas[0].(map[string]any)["quota"].(map[string]any)["bucketRules"].([]any)
	if last := fmt.Sprint(rules[len(rules)-1]); last != "map[clientSelectors:[map[headers:[map[invert:false name:x-aigw-budget type:RegularExpression value:.+] map[invert:false name:x-aigw-tenant type:Distinct]]]] quota:map[duration:1d limit:4294967295]]" {
		t.Fatalf("the rule for budgets: %v", rules)
	}
	btp, _ := gvrFor("BackendTrafficPolicy")
	bp, err := c.dyn.Resource(btp).Namespace(ns).Get(ctx, render.BudgetPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// One rule for every tenant, its limit from the token: the CRD keeps
	// fromMetadata, which one without the field would have pruned.
	rl, _, _ := unstructured.NestedSlice(bp.Object, "spec", "rateLimit", "global", "rules")
	if len(rl) != 1 || fmt.Sprint(rl[0].(map[string]any)["limit"]) != "map[fromMetadata:map[key:aigw-hub namespace:envoy.filters.http.jwt_authn] requests:4294967295 unit:Day]" ||
		!strings.Contains(fmt.Sprint(rl[0].(map[string]any)["clientSelectors"]), "type:Distinct") {
		t.Fatalf("budget rules: %v", rl)
	}
	// The entry route's own policy merges with it: the field is kept, which
	// a CRD without it would have pruned.
	fp, err := c.dyn.Resource(btp).Namespace(ns).Get(ctx, render.FleetName("glm-5-3"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if merge, _, _ := unstructured.NestedString(fp.Object, "spec", "mergeType"); merge != "StrategicMerge" {
		t.Fatalf("the entry route's policy has mergeType %q", merge)
	}
	route, _ := gvrFor("AIGatewayRoute")
	r, err := c.dyn.Resource(route).Namespace(ns).Get(ctx, render.FleetName("glm-5-3"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if costs, _, _ := unstructured.NestedSlice(r.Object, "spec", "llmRequestCosts"); len(costs) != 1 || costs[0].(map[string]any)["metadataKey"] != render.CreditsKey {
		t.Fatalf("route costs: %v", costs)
	}
	if live, err = c.dyn.Resource(pol).Namespace(ns).Get(ctx, render.AuthPolicyName, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if rules, _, _ := unstructured.NestedSlice(live.Object, "spec", "authorization", "rules"); len(rules) != 6 || rules[5].(map[string]any)["name"] != "models" {
		t.Fatalf("tokens: %d rules, want 2 revoked, 2 revoked-span, 1 disabled, 1 models: %v", len(rules), rules)
	}
	// Without budgets the policy goes.
	s.Budgets = nil
	s.Models[0].BudgetsHold = false
	if _, err := c.Sync(ctx, ns, render.Objects(s), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.dyn.Resource(btp).Namespace(ns).Get(ctx, render.BudgetPolicyName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("no budgets, yet the policy is there: %v", err)
	}

	// Token counts bring it back for their four rules, after the budget
	// rule, and the four that count each key by its client ID (0.37.0): the
	// CRD keeps shadowMode, the two Distinct headers and the cost from
	// metadata, and the route's token cost types are ones the AI gateway's
	// CRD takes.
	s.TokenCounts = true
	if _, err := c.Sync(ctx, ns, render.Objects(s), nil); err != nil {
		t.Fatalf("token counts: the API server refused a rendered object: %v", err)
	}
	if bp, err = c.dyn.Resource(btp).Namespace(ns).Get(ctx, render.BudgetPolicyName, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	rl, _, _ = unstructured.NestedSlice(bp.Object, "spec", "rateLimit", "global", "rules")
	if len(rl) != 9 || rl[2].(map[string]any)["shadowMode"] != true || !strings.Contains(fmt.Sprint(rl[6].(map[string]any)["clientSelectors"]), "name:x-aigw-client-id type:Distinct") ||
		fmt.Sprint(rl[2].(map[string]any)["cost"]) != "map[request:map[from:Number number:0] response:map[from:Metadata metadata:map[key:aigw_ui_input namespace:io.envoy.ai_gateway]]]" ||
		strings.Count(fmt.Sprint(rl[2].(map[string]any)["clientSelectors"]), "type:Distinct") != 2 {
		t.Fatalf("token rules as stored: %v", rl)
	}
	if r, err = c.dyn.Resource(route).Namespace(ns).Get(ctx, render.FleetName("glm-5-3"), metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if costs, _, _ := unstructured.NestedSlice(r.Object, "spec", "llmRequestCosts"); len(costs) != 4 || !reflect.DeepEqual(costs[1:], render.TokenCosts()) {
		t.Fatalf("route costs with token counts: %v", costs)
	}
	s.TokenCounts = false
	if _, err := c.Sync(ctx, ns, render.Objects(s), nil); err != nil {
		t.Fatal(err)
	}

	// Own quotas by amount: the rule matches the amount's header exactly and
	// counts each tenant on its own, and the token provider copies the
	// amount from a nested claim, both as the CRDs take them.
	s.Models[0].ByAmount = true
	s.Models[0].Amounts = []render.QuotaAmount{{Slot: 0, Limit: 500, Window: "1d"}}
	if _, err := c.Sync(ctx, ns, render.Objects(s), nil); err != nil {
		t.Fatalf("by amount: the API server refused a rendered object: %v", err)
	}
	if q, err = c.dyn.Resource(qp).Namespace(ns).Get(ctx, "glm-5-3", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	quotas, _, _ = unstructured.NestedSlice(q.Object, "spec", "perModelQuotas")
	rules = quotas[0].(map[string]any)["quota"].(map[string]any)["bucketRules"].([]any)
	if len(rules) != 1 || fmt.Sprint(rules[0]) != "map[clientSelectors:[map[headers:[map[invert:false name:x-aigw-quota-glm-5-3 type:Exact value:500-1d] map[invert:false name:x-aigw-tenant type:Distinct]]]] quota:map[duration:1d limit:500]]" {
		t.Fatalf("rules by amount: %v", rules)
	}
	if live, err = c.dyn.Resource(pol).Namespace(ns).Get(ctx, render.AuthPolicyName, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(live.Object["spec"]), "map[claim:quotas.glm-5-3 header:x-aigw-quota-glm-5-3]") {
		t.Fatalf("the token provider does not copy the amount: %v", live.Object["spec"])
	}
}
