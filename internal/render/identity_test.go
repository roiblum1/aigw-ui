package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func identity(mode string) Identity {
	return Identity{Mode: mode, Issuer: "aigw-ui", Audience: "aigw-fleet", JWKS: `{"keys":[]}`,
		Denied: []string{"00ff", "00aa"}, Disabled: []string{"team-c"}}
}

func spec(t *testing.T, o *unstructured.Unstructured) string {
	t.Helper()
	data, err := json.Marshal(o.Object["spec"])
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Before any token, the policy and the backends are what they were.
func TestIdentityKeys(t *testing.T) {
	s := testState()
	s.Identity = Identity{Mode: IdentityKeys}
	objs := Objects(s)
	p := spec(t, find(t, objs, "SecurityPolicy", AuthPolicyName))
	if strings.Contains(p, "jwt") || strings.Contains(p, "authorization") || !strings.Contains(p, `"sanitize":true`) {
		t.Fatalf("policy: %s", p)
	}
	// The model server gets no credential to remove yet.
	if b := spec(t, find(t, objs, "AIServiceBackend", "glm5-3")); strings.Contains(b, "headerMutation") {
		t.Fatalf("backend: %s", b)
	}
	if !has(objs, "Secret", KeysSecretName) {
		t.Fatal("no Secret")
	}
}

// During the switch, the gateway takes an API key or a token: the API key
// check lists both, keeps the credential for the JWT check, which lets a
// request without a token on; the rules refuse a revoked token, a disabled
// tenant's, and a token-shaped credential that did not check out, as lab
// check C found. The backends remove the credentials.
func TestIdentityBoth(t *testing.T) {
	s := testState()
	s.Identity = identity(IdentityBoth)
	objs := Objects(s)
	pol := find(t, objs, "SecurityPolicy", AuthPolicyName)
	if sanitize, _, _ := unstructured.NestedBool(pol.Object, "spec", "apiKeyAuth", "sanitize"); sanitize {
		t.Fatal("the API key check takes the token away")
	}
	if open, _, _ := unstructured.NestedBool(pol.Object, "spec", "jwt", "failOpen"); !open {
		t.Fatal("a request with an API key is refused by the JWT check")
	}
	providers, _, _ := unstructured.NestedSlice(pol.Object, "spec", "jwt", "providers")
	p := providers[0].(map[string]any)
	if fmt.Sprint(p["claimToHeaders"]) != "[map[claim:sub header:x-aigw-tenant] map[claim:jti header:x-aigw-key] map[claim:sub header:x-llm-d-inference-fairness-id]]" ||
		fmt.Sprint(p["extractFrom"]) != "map[headers:[map[name:Authorization valuePrefix:Bearer ] map[name:x-api-key]]]" ||
		fmt.Sprint(p["audiences"]) != "[aigw-fleet]" || p["issuer"] != "aigw-ui" {
		t.Fatalf("provider: %v", p)
	}
	rules, _, _ := unstructured.NestedSlice(pol.Object, "spec", "authorization", "rules")
	var names []string
	for _, r := range rules {
		names = append(names, r.(map[string]any)["name"].(string)+":"+r.(map[string]any)["action"].(string))
	}
	if strings.Join(names, " ") != "revoked:Deny disabled-tenant:Deny token:Allow token-not-checked:Deny" {
		t.Fatalf("rules: %v", names)
	}
	if v := fmt.Sprint(rules[0].(map[string]any)["principal"]); v != "map[jwt:map[claims:[map[name:jti values:[00aa 00ff]]] provider:aigw-hub]]" {
		t.Fatalf("revoked: %s", v)
	}
	for _, b := range []string{"glm5-3", "small"} {
		if h := spec(t, find(t, objs, "AIServiceBackend", b)); !strings.Contains(h, `"headerMutation":{"remove":["authorization","x-api-key"]}`) {
			t.Fatalf("backend %s: %s", b, h)
		}
	}
	if !has(objs, "Secret", KeysSecretName) {
		t.Fatal("no Secret during the switch")
	}
}

// With tokens only, nothing is listed: no Secret, no API key check, and the
// token writes the client ID the per-tenant rules match.
func TestIdentityTokens(t *testing.T) {
	s := testState()
	s.Identity = identity(IdentityTokens)
	s.Identity.Denied = nil
	objs := Objects(s)
	if has(objs, "Secret", KeysSecretName) {
		t.Fatal("a Secret with tokens only")
	}
	pol := find(t, objs, "SecurityPolicy", AuthPolicyName)
	p := spec(t, pol)
	if strings.Contains(p, "apiKeyAuth") || strings.Contains(p, "failOpen") || !strings.Contains(p, `{"claim":"cid","header":"x-aigw-client-id"}`) ||
		!strings.Contains(p, `{"claim":"sub","header":"x-llm-d-inference-fairness-id"}`) {
		t.Fatalf("policy: %s", p)
	}
	rules, _, _ := unstructured.NestedSlice(pol.Object, "spec", "authorization", "rules")
	if len(rules) != 1 || rules[0].(map[string]any)["name"] != "disabled-tenant" {
		t.Fatalf("rules: %v", rules)
	}
	s.Identity.Disabled = nil
	if p := spec(t, find(t, Objects(s), "SecurityPolicy", AuthPolicyName)); strings.Contains(p, "authorization") {
		t.Fatalf("no one refused, yet rules: %s", p)
	}
}

// The gateway takes at most 128 values in one claim of a rule.
func TestIdentityDeniedSplit(t *testing.T) {
	i := identity(IdentityTokens)
	i.Denied = nil
	for n := range 300 {
		i.Denied = append(i.Denied, fmt.Sprintf("%016x", n))
	}
	rules := authorization(i, nil)["rules"].([]any)
	if len(rules) != 4 { // 128 + 128 + 44 revoked, 1 disabled
		t.Fatalf("%d rules", len(rules))
	}
	for _, r := range rules[:3] {
		claims := r.(map[string]any)["principal"].(map[string]any)["jwt"].(map[string]any)["claims"].([]any)
		if n := len(claims[0].(map[string]any)["values"].([]any)); n > maxClaimValues {
			t.Fatalf("%d values", n)
		}
	}
}

// A rate limit charges an answer's cost with a rule that reads the request's
// headers again when the answer ends, and a backend removes a header from
// those same headers: a model server's backend that removed the tenant and
// the budget had every answer counted as 1 under a quota and as nothing
// under a budget (the lab, 2026-10-17). No backend removes a header a rule
// reads, in any identity mode.
func TestBackendsKeepCountedHeaders(t *testing.T) {
	counted := []string{TenantHeader, KeyHeader, ClientIDHeader, BudgetHeader, QuotaHeader("")}
	for _, mode := range []string{IdentityKeys, IdentityBoth, IdentityTokens} {
		// Model servers, and the backends of entry routes.
		for _, s := range []State{testState(), fleetState()} {
			s.AuthEnabled = true
			s.Identity = identity(mode)
			for _, o := range Objects(s) {
				if o.GetKind() != "AIServiceBackend" {
					continue
				}
				removed, _, _ := unstructured.NestedStringSlice(o.Object, "spec", "headerMutation", "remove")
				for _, name := range removed {
					for _, c := range counted {
						if strings.HasPrefix(name, c) {
							t.Errorf("%s: backend %s removes %s", mode, o.GetName(), name)
						}
					}
				}
			}
		}
	}
}

// A fleet backend that sets the request class removes the credentials too.
func TestIdentityFleetBackend(t *testing.T) {
	s := fleetState()
	s.AuthEnabled = true
	s.Identity = identity(IdentityTokens)
	b := fleetServiceBackend(s, s.Models[0], "x", ObjectiveBestEffort)
	h := fmt.Sprint(b.Object["spec"].(map[string]any)["headerMutation"])
	if !strings.Contains(h, "remove:[authorization x-api-key]") || !strings.Contains(h, "best-effort") {
		t.Fatalf("headers: %s", h)
	}
}
