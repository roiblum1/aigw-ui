package kube

import (
	"context"
	"fmt"
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
	if _, err := c.Sync(ctx, ns, render.Objects(s), nil); err != nil {
		t.Fatalf("both: the API server refused a rendered object: %v", err)
	}
	pol, _ := gvrFor("SecurityPolicy")
	live, err := c.dyn.Resource(pol).Namespace(ns).Get(ctx, render.AuthPolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rules, _, _ := unstructured.NestedSlice(live.Object, "spec", "authorization", "rules"); len(rules) != 5 {
		t.Fatalf("both: %d rules, want 2 revoked, 1 disabled, token, token-not-checked", len(rules))
	}
	backend, _ := gvrFor("AIServiceBackend")
	b, err := c.dyn.Resource(backend).Namespace(ns).Get(ctx, render.FleetName("glm-5-3"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if removed, _, _ := unstructured.NestedStringSlice(b.Object, "spec", "headerMutation", "remove"); len(removed) != 2 {
		t.Fatalf("backend headers: %v", b.Object["spec"])
	}

	// Tokens only, with a standard: one Distinct rule on the tenant header
	// and team-a's quota as an Exact exception on it.
	s.Identity.Mode = render.IdentityTokens
	s.Models[0].Standard = &render.Budget{Limit: 100, Window: "1h"}
	s.Tenants = []string{"team-a", "team-b"}
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
	if len(rules) != 2 || fmt.Sprint(rules[0]) != "map[clientSelectors:[map[headers:[map[invert:false name:x-aigw-tenant type:Distinct]]]] quota:map[duration:1h limit:100]]" {
		t.Fatalf("standard rules: %v", rules)
	}
}
