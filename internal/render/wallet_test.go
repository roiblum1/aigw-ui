package render

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A spent wallet is a rule that refuses the tenant's client ID on the models
// it covers until the window ends, first among the rules, with API keys as
// with tokens.
func TestWalletRules(t *testing.T) {
	until := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	s := testState()
	s.Identity = Identity{Mode: IdentityKeys, Closed: []Closure{{Slug: "team-a", Models: []string{"GLM5.3", "it's"}, Until: until}}}
	pol := authPolicy(s)
	rules, _, _ := unstructured.NestedSlice(pol.Object, "spec", "authorization", "rules")
	if len(rules) != 1 {
		t.Fatalf("rules %v", rules)
	}
	got := rules[0].(map[string]any)["cel"].(string)
	want := `request.time < timestamp('2026-10-16T00:00:00Z') && 'x-aigw-client-id' in request.headers && ` +
		`request.headers['x-aigw-client-id'].startsWith('team-a.') && 'x-ai-eg-model' in request.headers && ` +
		`request.headers['x-ai-eg-model'] in ['GLM5.3', 'it\'s']`
	if got != want {
		t.Fatalf("cel:\n%s\nwant\n%s", got, want)
	}
	if def, _, _ := unstructured.NestedString(pol.Object, "spec", "authorization", "defaultAction"); def != "Allow" {
		t.Fatalf("default %q", def)
	}
	// With tokens, the wallet rules come before the token rules.
	s.Identity = identity(IdentityBoth)
	s.Identity.Closed = []Closure{{Slug: "team-a", Models: []string{"GLM5.3"}, Until: until}}
	rules, _, _ = unstructured.NestedSlice(authPolicy(s).Object, "spec", "authorization", "rules")
	if rules[0].(map[string]any)["name"] != "wallet-spent" || len(rules) != 5 {
		t.Fatalf("order: %v", rules)
	}
}

// A tenant with many models gets several rules, each short enough.
func TestWalletRulesSplit(t *testing.T) {
	var models []string
	for n := range 200 {
		models = append(models, fmt.Sprintf("model-with-a-long-name-%03d", n))
	}
	rules := walletRules([]Closure{{Slug: "team-a", Models: models, Until: time.Now()}})
	if len(rules) < 2 {
		t.Fatalf("%d rules", len(rules))
	}
	seen := 0
	for _, r := range rules {
		cel := r.(map[string]any)["cel"].(string)
		if len(cel) > maxCEL {
			t.Fatalf("%d characters", len(cel))
		}
		seen += strings.Count(cel, "model-with-a-long-name-")
	}
	if seen != 200 {
		t.Fatalf("%d models in the rules", seen)
	}
}
