package render

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	celgo "github.com/google/cel-go/cel"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
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

// Runs of denied tokens are refused by their expiry, a span to an entry,
// in rules of at most 4,096 characters of CEL, after the jtis.
func TestIdentityDeniedSpans(t *testing.T) {
	i := identity(IdentityTokens)
	i.DeniedSpans = []ExpirySpan{{From: 1792254713, To: 1792254800, Tokens: 40}}
	rules := authorization(i, nil)["rules"].([]any)
	if len(rules) != 3 || rules[0].(map[string]any)["name"] != "revoked" || rules[1].(map[string]any)["name"] != "revoked-span" {
		t.Fatalf("rules: %v", rules)
	}
	c := "metadata.filter_metadata['envoy.filters.http.jwt_authn']['aigw-hub']"
	want := "'envoy.filters.http.jwt_authn' in metadata.filter_metadata && 'aigw-hub' in metadata.filter_metadata['envoy.filters.http.jwt_authn']" +
		" && 'exp' in " + c + " && 'cid' in " + c + " && 'jti' in " + c +
		" && " + c + "['cid'].endsWith(" + c + "['jti'])" +
		" && ((" + c + "['exp'] >= 1792254713.0 && " + c + "['exp'] <= 1792254800.0))"
	if got := rules[1].(map[string]any)["cel"]; got != want {
		t.Fatalf("cel\n%s\nwant\n%s", got, want)
	}

	i.DeniedSpans = nil
	for n := range int64(300) {
		i.DeniedSpans = append(i.DeniedSpans, ExpirySpan{From: 1792254713 + 100*n, To: 1792254713 + 100*n + 50, Tokens: 3})
	}
	var spans int
	for _, r := range authorization(i, nil)["rules"].([]any) {
		cel, _ := r.(map[string]any)["cel"].(string)
		if cel == "" {
			continue
		}
		if len(cel) > maxCEL {
			t.Fatalf("%d characters of CEL", len(cel))
		}
		spans += strings.Count(cel, "<= ")
	}
	if spans != 300 {
		t.Fatalf("%d of 300 spans rendered", spans)
	}
}

// The span rule refuses a stored token that expires within a span and lets
// every other token through: one that expires outside it, and one a
// personal key got, whose cid names the key. Evaluated with cel-go on the
// claims as the gateway's JWT filter keeps them (numbers as doubles); a
// request without a checked token matches nothing.
func TestIdentityDeniedSpanMatches(t *testing.T) {
	cel := spanRules([]ExpirySpan{{From: 1000, To: 2000, Tokens: 3}, {From: 5000, To: 5000, Tokens: 3}})[0].(map[string]any)["cel"].(string)
	env, err := celgo.NewEnv(celgo.Variable("metadata", celgo.DynType))
	if err != nil {
		t.Fatal(err)
	}
	// Envoy Gateway only parses the expression and Envoy evaluates it
	// without a type check; a checker must take it as well.
	checked, iss := env.Compile(cel)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	parsed, iss := env.Parse(cel)
	if iss.Err() != nil {
		t.Fatal(iss.Err())
	}
	var programs []celgo.Program
	for _, ast := range []*celgo.Ast{checked, parsed} {
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		programs = append(programs, prg)
	}
	refused := func(claims map[string]any) bool {
		t.Helper()
		md := map[string]any{"filter_metadata": map[string]any{}}
		if claims != nil {
			md["filter_metadata"] = map[string]any{"envoy.filters.http.jwt_authn": map[string]any{"aigw-hub": claims}}
		}
		var got []bool
		for _, prg := range programs {
			out, _, err := prg.Eval(map[string]any{"metadata": md})
			if err != nil {
				t.Fatalf("%v: %v", claims, err)
			}
			got = append(got, out.Value() == true)
		}
		if got[0] != got[1] {
			t.Fatalf("%v: checked %v, parsed %v", claims, got[0], got[1])
		}
		return got[0]
	}
	stored := func(exp float64) map[string]any {
		return map[string]any{"sub": "e2e-q-001", "jti": "00aa11bb22cc33dd", "cid": "e2e-q-001.00aa11bb22cc33dd", "exp": exp, "iat": exp - 100}
	}
	for _, c := range []struct {
		claims map[string]any
		want   bool
	}{
		{stored(1000), true}, {stored(1500), true}, {stored(2000), true}, {stored(5000), true},
		{stored(999), false}, {stored(2001), false}, {stored(4999), false},
		// A personal key's token: its cid is the key's.
		{map[string]any{"sub": "e2e-q-001", "jti": "99ee", "cid": "e2e-q-001.00aa11bb22cc33dd", "exp": 1500.0}, false},
		{nil, false},
	} {
		if got := refused(c.claims); got != c.want {
			t.Errorf("%v: refused %v, want %v", c.claims, got, c.want)
		}
	}
}

// envoyCEL fails when Envoy could not build the expression. Envoy builds CEL
// without comprehensions (exists, all, map, filter) and without joining
// strings, and refuses the whole route configuration when an expression
// needs them: the proxy then takes no change at all (0.32.0 on the lab,
// 2026-10-17; filters/common/expr/evaluator.cc in Envoy 1.39). cel-go takes
// both, so the tests check the parsed expression for what Envoy has: the
// operators below and the built-in string tests.
func envoyCEL(t *testing.T, expr string) {
	t.Helper()
	if err := envoyCELError(expr); err != nil {
		t.Errorf("%v in %q", err, expr)
	}
}

func envoyCELError(expr string) error {
	env, err := celgo.NewEnv()
	if err != nil {
		return err
	}
	ast, iss := env.Parse(expr)
	if iss.Err() != nil {
		return iss.Err()
	}
	parsed, err := celgo.AstToParsedExpr(ast)
	if err != nil {
		return err
	}
	allowed := map[string]bool{"_&&_": true, "_||_": true, "!_": true, "_==_": true, "_!=_": true, "_<_": true, "_<=_": true,
		"_>_": true, "_>=_": true, "@in": true, "_[_]": true, "startsWith": true, "endsWith": true, "contains": true}
	var walk func(e *exprpb.Expr) error
	walk = func(e *exprpb.Expr) error {
		switch k := e.GetExprKind().(type) {
		case *exprpb.Expr_ComprehensionExpr:
			return errors.New("a comprehension, which Envoy does not build")
		case *exprpb.Expr_CallExpr:
			if !allowed[k.CallExpr.GetFunction()] {
				return fmt.Errorf("function %s, which Envoy may not have", k.CallExpr.GetFunction())
			}
			if t := k.CallExpr.GetTarget(); t != nil {
				if err := walk(t); err != nil {
					return err
				}
			}
			for _, a := range k.CallExpr.GetArgs() {
				if err := walk(a); err != nil {
					return err
				}
			}
		case *exprpb.Expr_SelectExpr:
			return walk(k.SelectExpr.GetOperand())
		case *exprpb.Expr_ListExpr:
			for _, el := range k.ListExpr.GetElements() {
				if err := walk(el); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(parsed.GetExpr())
}

// Every authorization rule the hub renders in CEL is one Envoy builds: the
// spans of denied tokens, the models rule and the rule against unchecked
// tokens. What 0.32.0 wrote for the spans is refused.
func TestAuthorizationCELForEnvoy(t *testing.T) {
	i := identity(IdentityBoth)
	for n := range int64(50) {
		i.DeniedSpans = append(i.DeniedSpans, ExpirySpan{From: 1792254713 + 100*n, To: 1792254763 + 100*n, Tokens: 3})
	}
	var n int
	for _, r := range authorization(i, []TenantModels{{TenantSlug: "a", Models: []string{"m"}}})["rules"].([]any) {
		if cel, ok := r.(map[string]any)["cel"].(string); ok {
			envoyCEL(t, cel)
			n++
		}
	}
	if n < 5 { // 3 span rules, models, token-not-checked
		t.Fatalf("%d CEL rules", n)
	}
	for _, old := range []string{
		"[metadata].exists(c, c['exp'] >= 1.0)",
		"metadata['cid'] == metadata['sub'] + '.' + metadata['jti']",
	} {
		if envoyCELError(old) == nil {
			t.Errorf("taken: %q", old)
		}
	}
}
