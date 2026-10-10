package render

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func testState() State {
	return State{
		Namespace:   "ai-gateway",
		GatewayName: "llm",
		AuthEnabled: true,
		Models: []Model{{
			Name: "GLM5.3", Slug: "glm5-3", Host: "glm.models.svc.cluster.local", Port: 8000,
			UpstreamModel: "glm-5.3", DefaultLimit: 1, DefaultWindow: "1d",
			Quotas: []TenantQuota{{TenantSlug: "team-b", Slot: 1, Limit: 500, Window: "1h"}, {TenantSlug: "team-a", Slot: 0, Limit: 1000, Window: "1d"}},
		}, {
			Name: "small", Slug: "small", Host: "10.0.0.5", Port: 80, UpstreamModel: "small", DefaultLimit: 1, DefaultWindow: "1d",
		}},
		Keys: []Key{{ClientID: "team-a.0a1b2c3d", Value: "sk-secret"}},
	}
}

func kinds(objs []*unstructured.Unstructured) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.GetKind()+"/"+o.GetName())
	}
	return out
}

func TestObjects(t *testing.T) {
	objs := Objects(testState())
	want := []string{
		"Secret/aigw-ui-api-keys",
		"Backend/glm5-3", "AIServiceBackend/glm5-3", "AIGatewayRoute/glm5-3", "QuotaPolicy/glm5-3",
		"Backend/small", "AIServiceBackend/small", "AIGatewayRoute/small",
		"SecurityPolicy/aigw-ui-api-key-auth",
	}
	got := kinds(objs)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("object %d: got %s, want %s", i, got[i], want[i])
		}
	}
	for _, o := range objs {
		if o.GetNamespace() != "ai-gateway" || o.GetLabels()[ManagedLabel] != ManagedValue {
			t.Errorf("%s: wrong namespace or missing managed label", o.GetName())
		}
		// Server-side apply sends the object as JSON; DeepCopy panics on types JSON cannot carry.
		o.DeepCopy()
	}

	quota := objs[4]
	perModel, _, _ := unstructured.NestedSlice(quota.Object, "spec", "perModelQuotas")
	entry := perModel[0].(map[string]any)
	if entry["modelName"] != "glm-5.3" {
		t.Errorf("quota modelName = %v, want the upstream model name", entry["modelName"])
	}
	rules, _, _ := unstructured.NestedSlice(entry, "quota", "bucketRules")
	if len(rules) != 2 {
		t.Fatalf("got %d bucket rules, want 2", len(rules))
	}
	// Rules are sorted by tenant so every cluster renders the same policy.
	first, _, _ := unstructured.NestedSlice(rules[0].(map[string]any), "clientSelectors")
	headers, _, _ := unstructured.NestedSlice(first[0].(map[string]any), "headers")
	if got := headers[0].(map[string]any)["value"]; got != TenantClientIDPattern("team-a") {
		t.Errorf("first rule matches %v, want team-a", got)
	}
}

func TestObjectsWithoutAuth(t *testing.T) {
	s := testState()
	s.AuthEnabled = false
	for _, k := range kinds(Objects(s)) {
		if k == "Secret/"+KeysSecretName || k == "SecurityPolicy/"+AuthPolicyName {
			t.Errorf("%s rendered with auth disabled", k)
		}
	}
}

func TestTenantClientIDPattern(t *testing.T) {
	re := regexp.MustCompile(TenantClientIDPattern("team-a"))
	for id, want := range map[string]bool{
		"team-a.0a1b2c3d":  true,
		"team-a2.0a1b2c3d": false,
		"team-ab0a1b2c3d":  false,
		"xteam-a.0a1b2c3d": false,
	} {
		if re.MatchString(id) != want {
			t.Errorf("match(%q) = %v, want %v", id, !want, want)
		}
	}
}

func TestRedacted(t *testing.T) {
	objs := Objects(testState())
	red := Redacted(objs)
	if v, _, _ := unstructured.NestedString(red[0].Object, "data", "team-a.0a1b2c3d"); v != "<redacted>" {
		t.Errorf("redacted value = %q", v)
	}
	if v, _, _ := unstructured.NestedString(objs[0].Object, "data", "team-a.0a1b2c3d"); v != "c2stc2VjcmV0" {
		t.Errorf("Redacted changed the original: %q", v)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"GLM5.3": "glm5-3", "org/Model_v2": "org-model-v2", "--x--": "x", "...": ""} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiscoveredModel(t *testing.T) {
	s := State{Namespace: "ai-gateway", GatewayName: "llm", Models: []Model{
		{
			Name: "GLM5.3", Slug: "glm5-3", DefaultLimit: 1, DefaultWindow: "1d",
			Existing: []Target{{Backend: "glm-a", Model: "glm-5.3"}, {Backend: "glm-b", Model: "glm-5.3"}},
			Quotas:   []TenantQuota{{TenantSlug: "team-a", Limit: 10, Window: "1h"}},
		},
		{Name: "no-quota", Slug: "no-quota", Existing: []Target{{Backend: "x", Model: "no-quota"}}},
	}}
	objs := Objects(s)
	// The route and backends belong to the cluster; only the quota is ours.
	if got := kinds(objs); len(got) != 1 || got[0] != "QuotaPolicy/glm5-3" {
		t.Fatalf("got %v, want only QuotaPolicy/glm5-3", got)
	}
	targets, _, _ := unstructured.NestedSlice(objs[0].Object, "spec", "targetRefs")
	perModel, _, _ := unstructured.NestedSlice(objs[0].Object, "spec", "perModelQuotas")
	if len(targets) != 2 || len(perModel) != 1 {
		t.Errorf("got %d targets and %d model quotas, want 2 and 1", len(targets), len(perModel))
	}
	objs[0].DeepCopy()
}

func TestQuotaPolicyPerBackendNamespace(t *testing.T) {
	quota := []TenantQuota{{TenantSlug: "team-a", Limit: 10, Window: "1h"}}
	s := State{Namespace: "ai-gateway", GatewayName: "llm", Models: []Model{
		{Name: "glm", Slug: "glm", DefaultLimit: 1, DefaultWindow: "1d", Quotas: quota, Existing: []Target{
			{Namespace: "team-b", Backend: "glm-b", Model: "glm"},
			{Backend: "glm-a", Model: "glm"}, // empty namespace is the gateway namespace
			{Namespace: "team-b", Backend: "glm-b2", Model: "glm"},
		}},
		// Listed by the gateway but served from something a quota cannot target.
		{Name: "judge", Slug: "judge", DefaultLimit: 1, DefaultWindow: "1d", Quotas: quota, Existing: []Target{}},
	}}
	objs := Objects(s)
	if len(objs) != 2 {
		t.Fatalf("got %v, want one QuotaPolicy in each of two namespaces", kinds(objs))
	}
	for i, want := range []struct {
		ns      string
		targets int
	}{{"ai-gateway", 1}, {"team-b", 2}} {
		targets, _, _ := unstructured.NestedSlice(objs[i].Object, "spec", "targetRefs")
		if objs[i].GetNamespace() != want.ns || len(targets) != want.targets || objs[i].GetName() != "glm" {
			t.Errorf("policy %d: %s/%s with %d targets, want %s/glm with %d",
				i, objs[i].GetNamespace(), objs[i].GetName(), len(targets), want.ns, want.targets)
		}
	}
}

// The gateway's controller writes a QuotaPolicy back with whatever
// serviceQuota it holds, and the CRD only accepts these four windows. Without
// a valid value here its finalizer update is rejected on every reconcile.
func TestQuotaPolicySetsValidServiceQuota(t *testing.T) {
	for _, o := range Objects(testState()) {
		if o.GetKind() != "QuotaPolicy" {
			continue
		}
		duration, _, _ := unstructured.NestedString(o.Object, "spec", "serviceQuota", "quota", "duration")
		limit, _, _ := unstructured.NestedInt64(o.Object, "spec", "serviceQuota", "quota", "limit")
		if !map[string]bool{"1s": true, "1m": true, "1h": true, "1d": true}[duration] || limit <= 0 {
			t.Errorf("serviceQuota = %d per %q, want a positive limit and a window the CRD accepts", limit, duration)
		}
		return
	}
	t.Fatal("no QuotaPolicy rendered")
}

// durations collects every "duration" value in an object, at any depth.
func durations(v any, out *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if s, ok := child.(string); ok && k == "duration" {
				*out = append(*out, s)
			}
			durations(child, out)
		}
	case []any:
		for _, child := range x {
			durations(child, out)
		}
	}
}

// A quota without a window must never reach the cluster: the CRD rejects the
// empty duration and the sync, or the controller's finalizer update, fails.
func TestQuotaPolicyNeverRendersInvalidQuota(t *testing.T) {
	s := State{Namespace: "ai-gateway", GatewayName: "llm", Models: []Model{{
		Name: "glm", Slug: "glm", UpstreamModel: "glm",
		Quotas: []TenantQuota{
			{TenantSlug: "team-a", Slot: 0, Limit: 10, Window: ""},
			{TenantSlug: "team-b", Slot: 1, Limit: 0, Window: "1h"},
			{TenantSlug: "team-c", Slot: 2, Limit: 10, Window: "30s"},
			{TenantSlug: "team-d", Slot: 3, Limit: 10, Window: "1h"},
		},
	}}}
	var policy *unstructured.Unstructured
	for _, o := range Objects(s) {
		if o.GetKind() == "QuotaPolicy" {
			policy = o
		}
	}
	if policy == nil {
		t.Fatal("no QuotaPolicy rendered")
	}
	var got []string
	durations(policy.Object, &got)
	if len(got) != 6 { // serviceQuota, defaultBucket, three placeholders and team-d
		t.Errorf("got durations %v, want six", got)
	}
	for _, d := range got {
		if !validQuota(1, d) {
			t.Errorf("rendered duration %q, which the CRD rejects", d)
		}
	}
	perModel, _, _ := unstructured.NestedSlice(policy.Object, "spec", "perModelQuotas")
	limit, _, _ := unstructured.NestedInt64(perModel[0].(map[string]any), "quota", "defaultBucket", "limit")
	if limit != fallbackLimit {
		t.Errorf("default bucket limit = %d, want the fallback %d", limit, fallbackLimit)
	}
}

func TestQuotaPolicyCostExpressionAndShadow(t *testing.T) {
	s := testState()
	s.Models[0].CostExpression = "input_tokens + output_tokens * 4u"
	s.Models[0].Quotas[0].Shadow = true // team-b
	quota := Objects(s)[4]
	perModel, _, _ := unstructured.NestedSlice(quota.Object, "spec", "perModelQuotas")
	q := perModel[0].(map[string]any)["quota"].(map[string]any)
	if q["costExpression"] != "input_tokens + output_tokens * 4u" {
		t.Errorf("costExpression = %v", q["costExpression"])
	}
	rules := q["bucketRules"].([]any)
	if _, set := rules[0].(map[string]any)["shadowMode"]; set {
		t.Error("team-a is enforced and must not carry shadowMode")
	}
	if rules[1].(map[string]any)["shadowMode"] != true {
		t.Error("team-b should be in shadow mode")
	}
	quota.DeepCopy()

	// Without an expression the field is left out so the gateway default applies.
	plain := Objects(testState())[4]
	perModel, _, _ = unstructured.NestedSlice(plain.Object, "spec", "perModelQuotas")
	if _, set := perModel[0].(map[string]any)["quota"].(map[string]any)["costExpression"]; set {
		t.Error("costExpression set although the model has none")
	}
}

// A position without a quota is filled with a rule that matches nothing, so
// the rules after it stay where they are.
func TestQuotaPolicyKeepsRulePositions(t *testing.T) {
	s := State{Namespace: "ai-gateway", GatewayName: "llm", Models: []Model{{
		Name: "glm", Slug: "glm", UpstreamModel: "glm",
		Quotas: []TenantQuota{{TenantSlug: "team-c", Slot: 2, Limit: 10, Window: "1h"}, {TenantSlug: "team-a", Slot: 0, Limit: 10, Window: "1h"}},
	}}}
	policy := Objects(s)[3]
	perModel, _, _ := unstructured.NestedSlice(policy.Object, "spec", "perModelQuotas")
	rules := perModel[0].(map[string]any)["quota"].(map[string]any)["bucketRules"].([]any)
	var selectors []string
	for _, r := range rules {
		header := r.(map[string]any)["clientSelectors"].([]any)[0].(map[string]any)["headers"].([]any)[0].(map[string]any)
		selectors = append(selectors, header["type"].(string)+" "+header["value"].(string))
	}
	want := []string{`RegularExpression ^team-a\.[a-f0-9]+$`, "Exact " + placeholderSelector, `RegularExpression ^team-c\.[a-f0-9]+$`}
	if strings.Join(selectors, ", ") != strings.Join(want, ", ") {
		t.Errorf("rules = %v\nwant %v", selectors, want)
	}
}

// On the whole Gateway the key policy would also guard the listener other
// sites forward to, and they strip the key first.
func TestAuthPolicyTargetsClientListener(t *testing.T) {
	target := func(s State) map[string]any {
		refs, _, _ := unstructured.NestedSlice(authPolicy(s).Object, "spec", "targetRefs")
		return refs[0].(map[string]any)
	}
	whole := target(State{Namespace: "ai-gateway", GatewayName: "ai-gateway"})
	if _, set := whole["sectionName"]; set || whole["name"] != "ai-gateway" {
		t.Errorf("no listener set: %v", whole)
	}
	one := target(State{Namespace: "ai-gateway", GatewayName: "ai-gateway", ClientListener: "https"})
	if one["sectionName"] != "https" || one["kind"] != "Gateway" {
		t.Errorf("listener set: %v", one)
	}
}

// Every backend the hub renders takes kv_transfer_params out of a client's
// request: with it a client sets the cached token count of its own answer.
func TestServiceBackendsRemoveClientOnlyFields(t *testing.T) {
	var seen int
	for _, s := range []State{testState(), fleetState(), bestEffortState("team-a")} {
		for _, o := range Objects(s) {
			if o.GetKind() != "AIServiceBackend" {
				continue
			}
			seen++
			remove, _, _ := unstructured.NestedSlice(o.Object, "spec", "bodyMutation", "remove")
			if !reflect.DeepEqual(remove, []any{"kv_transfer_params"}) {
				t.Errorf("%s removes %v from the request body", o.GetName(), remove)
			}
		}
	}
	if seen < 4 {
		t.Errorf("only %d backends rendered", seen)
	}
}
