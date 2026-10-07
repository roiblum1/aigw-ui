package render

import (
	"regexp"
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
			Quotas: []TenantQuota{{TenantSlug: "team-b", Limit: 500, Window: "1h"}, {TenantSlug: "team-a", Limit: 1000, Window: "1d"}},
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
	if v, _, _ := unstructured.NestedString(red[0].Object, "stringData", "team-a.0a1b2c3d"); v != "<redacted>" {
		t.Errorf("redacted value = %q", v)
	}
	if v, _, _ := unstructured.NestedString(objs[0].Object, "stringData", "team-a.0a1b2c3d"); v != "sk-secret" {
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
