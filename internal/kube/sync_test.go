package kube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"

	"aigw-ui/internal/render"
)

// serial answers one request at a time: a sync sends several at once, and
// the stand-ins keep what they saw in plain variables.
func serial(h http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		h(w, r)
	}
}

// A server that ignores label selectors returns objects this tool does not
// own. Sync must delete only the ones carrying its label.
func TestSyncPrunesOnlyManagedObjects(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(serial(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
		case strings.HasSuffix(r.URL.Path, "/aigatewayroutes"):
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[
				{"apiVersion":"aigateway.envoyproxy.io/v1beta1","kind":"AIGatewayRoute","metadata":{"name":"theirs","namespace":"ns"}},
				{"apiVersion":"aigateway.envoyproxy.io/v1beta1","kind":"AIGatewayRoute","metadata":{"name":"ours-stale","namespace":"ns","labels":{"app.kubernetes.io/managed-by":"aigw-ui"}}}
			]}`))
		case strings.Contains(r.URL.Path, "/secrets"):
			// The role on the cluster grants no list on Secrets, only get on this one name.
			if !strings.HasSuffix(r.URL.Path, "/secrets/aigw-ui-api-keys") {
				t.Errorf("unexpected Secret request %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		default:
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`))
		}
	}))
	defer srv.Close()

	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Sync(context.Background(), "ns", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/aigatewayroutes/ours-stale") || res.Pruned != 1 {
		t.Errorf("deleted %v (pruned %d), want only ours-stale", deleted, res.Pruned)
	}

	// A held name stays, although nothing desired has it.
	deleted = nil
	res, err = client.Sync(context.Background(), "ns", nil, map[string]bool{"ours-stale": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 || res.Pruned != 0 {
		t.Errorf("deleted %v (pruned %d), want nothing", deleted, res.Pruned)
	}
}

// A QuotaPolicy left in a namespace that no longer holds a backend is removed,
// and objects are applied to their own namespace.
func TestSyncQuotaPoliciesAcrossNamespaces(t *testing.T) {
	var deleted, applied []string
	srv := httptest.NewServer(serial(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch:
			applied = append(applied, r.URL.Path)
			w.Write([]byte(`{"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"QuotaPolicy","metadata":{"name":"glm"}}`))
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
		case r.URL.Path == "/apis/aigateway.envoyproxy.io/v1alpha1/quotapolicies":
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[
				{"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"QuotaPolicy","metadata":{"name":"glm","namespace":"team-b","labels":{"app.kubernetes.io/managed-by":"aigw-ui"}}},
				{"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"QuotaPolicy","metadata":{"name":"glm","namespace":"old-team","labels":{"app.kubernetes.io/managed-by":"aigw-ui"}}}
			]}`))
		case strings.Contains(r.URL.Path, "/secrets/"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		default:
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`))
		}
	}))
	defer srv.Close()

	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	client.settle = 0
	policy := &unstructured.Unstructured{}
	policy.SetAPIVersion("aigateway.envoyproxy.io/v1alpha1")
	policy.SetKind("QuotaPolicy")
	policy.SetNamespace("team-b")
	policy.SetName("glm")

	if _, err := client.Sync(context.Background(), "ai-gateway", []*unstructured.Unstructured{policy}, nil); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || !strings.Contains(applied[0], "/namespaces/team-b/quotapolicies/glm") {
		t.Errorf("applied %v, want the policy in team-b", applied)
	}
	if len(deleted) != 1 || !strings.Contains(deleted[0], "/namespaces/old-team/quotapolicies/glm") {
		t.Errorf("deleted %v, want only the stale policy in old-team", deleted)
	}
}

// Sync reports what the cluster confirmed: an object that did not exist is
// created, one whose generation moved is updated, and one that stayed the
// same is not listed. The gateway's verdict is read back afterwards.
func TestSyncReportsChangesAndGatewayStatus(t *testing.T) {
	generation := map[string]int{"old": 3, "same": 5}
	srv := httptest.NewServer(serial(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		object := func(gen int, condition string) string {
			return `{"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"QuotaPolicy","metadata":{"name":"` + name +
				`","namespace":"ns","generation":` + string(rune('0'+gen)) + `},"status":{"conditions":[{"type":"` + condition + `","message":"why"}]}}`
		}
		switch {
		case r.Method == http.MethodPatch:
			if name != "same" {
				generation[name]++
			}
			w.Write([]byte(object(generation[name], "Accepted")))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/quotapolicies/"):
			if generation[name] == 0 {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
				return
			}
			condition := "Accepted"
			if name == "old" {
				condition = "NotAccepted"
			}
			w.Write([]byte(object(generation[name], condition)))
		default:
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`))
		}
	}))
	defer srv.Close()

	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	client.settle = 0
	var desired []*unstructured.Unstructured
	for _, name := range []string{"new", "old", "same"} {
		p := &unstructured.Unstructured{}
		p.SetAPIVersion("aigateway.envoyproxy.io/v1alpha1")
		p.SetKind("QuotaPolicy")
		p.SetName(name)
		desired = append(desired, p)
	}
	res, err := client.Sync(context.Background(), "ns", desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 3 || len(res.Changes) != 2 {
		t.Fatalf("applied %d, changes %+v; want 3 applied and 2 changes", res.Applied, res.Changes)
	}
	if c := res.Changes[0]; c.Name != "new" || c.Action != "created" || c.Gateway != "Accepted" {
		t.Errorf("first change = %+v, want new created and accepted", c)
	}
	if c := res.Changes[1]; c.Name != "old" || c.Action != "updated" || c.Gateway != "NotAccepted" {
		t.Errorf("second change = %+v, want old updated and not accepted", c)
	}
	if len(res.Rejected) != 1 || res.Rejected[0].Name != "old" || res.Rejected[0].GatewayMessage != "why" {
		t.Errorf("rejected = %+v, want old with its message", res.Rejected)
	}
}

// A route of the cluster's own gets the quota revision of the policy that
// targets its backend, before the policy is written. This tool's routes,
// routes to other backends and routes that have the revision are left alone.
func TestSyncMarksTheClustersOwnRoutes(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(serial(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch:
			calls = append(calls, r.Header.Get("Content-Type")+" "+r.URL.Path)
			w.Write([]byte(`{"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"QuotaPolicy","metadata":{"name":"glm","namespace":"models"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/namespaces/models/aigatewayroutes"):
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[
				{"apiVersion":"aigateway.envoyproxy.io/v1beta1","kind":"AIGatewayRoute","metadata":{"name":"theirs","namespace":"models"},"spec":{"rules":[{"backendRefs":[{"name":"glm-a"}]}]}},
				{"apiVersion":"aigateway.envoyproxy.io/v1beta1","kind":"AIGatewayRoute","metadata":{"name":"current","namespace":"models","annotations":{"gateway.envoyproxy.io/aigw-ui-quota-revision":"r2"}},"spec":{"rules":[{"backendRefs":[{"name":"glm-a"}]}]}},
				{"apiVersion":"aigateway.envoyproxy.io/v1beta1","kind":"AIGatewayRoute","metadata":{"name":"other-model","namespace":"models"},"spec":{"rules":[{"backendRefs":[{"name":"judge"}]}]}},
				{"apiVersion":"aigateway.envoyproxy.io/v1beta1","kind":"AIGatewayRoute","metadata":{"name":"ours","namespace":"models","labels":{"app.kubernetes.io/managed-by":"aigw-ui"}},"spec":{"rules":[{"backendRefs":[{"name":"glm-a"}]}]}}
			]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/quotapolicies/glm"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		default:
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`))
		}
	}))
	defer srv.Close()

	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	policy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "aigateway.envoyproxy.io/v1alpha1", "kind": "QuotaPolicy",
		"metadata": map[string]any{"name": "glm", "namespace": "models", "annotations": map[string]any{"gateway.envoyproxy.io/aigw-ui-quota-revision": "r2"}},
		"spec":     map[string]any{"targetRefs": []any{map[string]any{"kind": "AIServiceBackend", "name": "glm-a"}}},
	}}
	res, err := client.Sync(context.Background(), "ns", []*unstructured.Unstructured{policy}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "application/merge-patch+json ") || !strings.HasSuffix(calls[0], "/aigatewayroutes/theirs") ||
		!strings.HasSuffix(calls[1], "/quotapolicies/glm") {
		t.Errorf("patches %v, want the route theirs marked first, then the policy applied", calls)
	}
	if len(res.Changes) == 0 || res.Changes[0].Kind != "AIGatewayRoute" || res.Changes[0].Name != "theirs" {
		t.Errorf("changes %+v, want the marked route listed", res.Changes)
	}

	// A policy without the revision is one for this tool's own backends.
	calls = nil
	policy.SetAnnotations(nil)
	if _, err := client.Sync(context.Background(), "ns", []*unstructured.Unstructured{policy}, nil); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "/quotapolicies/glm") {
		t.Errorf("patches %v, want only the policy", calls)
	}
}

// A route of the cluster's own that sends two priced models to one backend
// charges each its own prices. Before, each model's policy wrote its own
// expression in turn, and every request on the route was charged the
// prices of the model synced last.
func TestSyncPricesEachModelOfASharedRoute(t *testing.T) {
	var bodies []string
	lists := 0
	srv := httptest.NewServer(serial(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/aigatewayroutes/shared"):
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(b))
			w.Write([]byte(`{"apiVersion":"aigateway.envoyproxy.io/v1beta1","kind":"AIGatewayRoute","metadata":{"name":"shared","namespace":"models"}}`))
		case r.Method == http.MethodPatch:
			w.Write([]byte(`{"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"QuotaPolicy","metadata":{"name":"x","namespace":"models"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/namespaces/models/aigatewayroutes"):
			lists++
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[
				{"apiVersion":"aigateway.envoyproxy.io/v1beta1","kind":"AIGatewayRoute","metadata":{"name":"shared","namespace":"models"},"spec":{"rules":[
					{"matches":[{"headers":[{"name":"x-ai-eg-model","value":"opus"}]}],"backendRefs":[{"name":"mock"}]},
					{"matches":[{"headers":[{"name":"x-ai-eg-model","value":"sonnet"}]}],"backendRefs":[{"name":"mock"}]}]}}
			]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		}
	}))
	defer srv.Close()
	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	policy := func(model, credits string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "aigateway.envoyproxy.io/v1alpha1", "kind": "QuotaPolicy",
			"metadata": map[string]any{"name": model, "namespace": "models", "annotations": map[string]any{
				render.QuotaRevisionAnnotation: "r-" + model, render.CreditsAnnotation: credits, render.CreditsModelAnnotation: model}},
			"spec": map[string]any{"targetRefs": []any{map[string]any{"kind": "AIServiceBackend", "name": "mock"}}},
		}}
	}
	if _, err := client.Sync(context.Background(), "ns", []*unstructured.Unstructured{
		policy("opus", "input_tokens * 15u"), policy("sonnet", "input_tokens * 3u")}, nil); err != nil {
		t.Fatal(err)
	}
	want := `model == 'opus' ? (input_tokens * 15u) : model == 'sonnet' ? (input_tokens * 3u) : 0u`
	if len(bodies) != 2 || lists != 1 {
		t.Fatalf("route patches %v after %d lists; want 2 after 1", bodies, lists)
	}
	if !strings.Contains(bodies[0], `"cel":"`+want+`"`) {
		t.Errorf("first patch %s, want the cost %s", bodies[0], want)
	}
	// The route is listed once a sync. The second policy finds the cost the
	// first one wrote and only moves the revision.
	if strings.Contains(bodies[1], "llmRequestCosts") || !strings.Contains(bodies[1], `"r-sonnet"`) {
		t.Errorf("second patch %s, want only the revision r-sonnet", bodies[1])
	}
}

// While the gateway counts tokens, a route of the cluster's own that the
// sync marks gets the keys it writes an answer's token counts under, once;
// a key of the wrong type is put right, and they go when tokens are no
// longer counted. A cost of the route's own stays.
func TestWithCostsTokenKeys(t *testing.T) {
	route := func(costs ...any) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"llmRequestCosts": costs}}}
	}
	theirs := map[string]any{"metadataKey": "llm_total", "type": "TotalToken"}
	credits := map[string]any{"metadataKey": render.CreditsKey, "type": "CEL", "cel": "1u"}

	got, changed := withCosts(route(theirs, credits), "1u", true)
	if want := append([]any{theirs, credits}, render.TokenCosts()...); !changed || !reflect.DeepEqual(got, want) {
		t.Fatalf("added: %v %v", changed, got)
	}
	if again, changed := withCosts(route(got...), "1u", true); changed || !reflect.DeepEqual(again, got) {
		t.Fatalf("a second sync changed %v", again)
	}
	wrong := map[string]any{"metadataKey": render.CachedTokensKey, "type": "InputToken"}
	if fixed, changed := withCosts(route(theirs, wrong), "", true); !changed || !reflect.DeepEqual(fixed, append([]any{theirs}, render.TokenCosts()...)) {
		t.Fatalf("wrong type: %v %v", changed, fixed)
	}
	if off, changed := withCosts(route(got...), "1u", false); !changed || !reflect.DeepEqual(off, []any{theirs, credits}) {
		t.Fatalf("not counted: %v %v", changed, off)
	}
}

// A sync writes the tenants' keys, then the key and token policy, then what
// routes and policies name, the routes, and the policies, each stage's
// objects side by side. It reports them in the order they were rendered, and stops after
// a stage that failed.
func TestSyncAppliesInStages(t *testing.T) {
	var order []string
	fail := ""
	srv := httptest.NewServer(serial(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case r.Method == http.MethodPatch && name == fail:
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Invalid","code":422}`))
		case r.Method == http.MethodPatch:
			order = append(order, name)
			w.Write([]byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"` + name + `","generation":1}}`))
		case r.Method == http.MethodGet && strings.Count(r.URL.Path, "/") > 6:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		default:
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`))
		}
	}))
	defer srv.Close()
	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	client.settle = 0
	object := func(apiVersion, kind, name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(apiVersion)
		u.SetKind(kind)
		u.SetName(name)
		return u
	}
	// In the order render.Objects gives: each model's objects, then the
	// token policy.
	desired := []*unstructured.Unstructured{
		object("gateway.envoyproxy.io/v1alpha1", "Backend", "be-a"),
		object("aigateway.envoyproxy.io/v1beta1", "AIServiceBackend", "sb-a"),
		object("aigateway.envoyproxy.io/v1beta1", "AIGatewayRoute", "rt-a"),
		object("gateway.envoyproxy.io/v1alpha1", "EnvoyPatchPolicy", "pp-a"),
		object("aigateway.envoyproxy.io/v1alpha1", "QuotaPolicy", "qp-a"),
		object("gateway.envoyproxy.io/v1alpha1", "Backend", "be-b"),
		object("aigateway.envoyproxy.io/v1beta1", "AIGatewayRoute", "rt-b"),
		object("gateway.envoyproxy.io/v1alpha1", "SecurityPolicy", "sp"),
	}
	res, err := client.Sync(context.Background(), "ns", desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	stage := map[string]int{"sp": 1, "be-a": 2, "sb-a": 2, "be-b": 2, "rt-a": 3, "rt-b": 3, "pp-a": 4, "qp-a": 4}
	for i := 1; i < len(order); i++ {
		if stage[order[i]] < stage[order[i-1]] {
			t.Fatalf("written in the order %v", order)
		}
	}
	var reported []string
	for _, c := range res.Changes {
		reported = append(reported, c.Name)
	}
	if want := []string{"be-a", "sb-a", "rt-a", "pp-a", "qp-a", "be-b", "rt-b", "sp"}; !reflect.DeepEqual(reported, want) {
		t.Errorf("changes %v, want %v", reported, want)
	}

	// A route that is refused stops the sync before the policies.
	order, fail = nil, "rt-b"
	res, err = client.Sync(context.Background(), "ns", desired, nil)
	if err == nil || !strings.Contains(err.Error(), "rt-b") {
		t.Fatalf("err = %v, want the refused route", err)
	}
	for _, name := range order {
		if stage[name] == 4 {
			t.Errorf("policy %s written after a route was refused: %v", name, order)
		}
	}
	if res.Applied != 5 {
		t.Errorf("%d applied, want the 5 of the first three stages that were taken", res.Applied)
	}
}

// A client remembers each object as its last sync left it. The next sync
// writes without reading an object first, reads the status of the objects
// it did not change from the answers to its writes, and still tells an
// object someone deleted in between, which it creates again.
func TestSyncRemembersObjects(t *testing.T) {
	gets := 0
	uid := map[string]string{"qp": "uid-1"}
	srv := httptest.NewServer(serial(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		object := `{"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"QuotaPolicy","metadata":{"name":"` + name +
			`","namespace":"ns","uid":"` + uid[name] + `","generation":1},"status":{"conditions":[{"type":"Accepted"}]}}`
		switch {
		case r.Method == http.MethodPatch:
			w.Write([]byte(object))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/quotapolicies/"+name) && name != "quotapolicies":
			gets++
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		default:
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`))
		}
	}))
	defer srv.Close()
	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	client.settle = 0
	policy := &unstructured.Unstructured{}
	policy.SetAPIVersion("aigateway.envoyproxy.io/v1alpha1")
	policy.SetKind("QuotaPolicy")
	policy.SetName("qp")
	desired := []*unstructured.Unstructured{policy}

	res, err := client.Sync(context.Background(), "ns", desired, nil)
	if err != nil || len(res.Changes) != 1 || res.Changes[0].Action != "created" {
		t.Fatalf("first sync: %+v %v", res.Changes, err)
	}
	first := gets
	res, err = client.Sync(context.Background(), "ns", desired, nil)
	if err != nil || len(res.Changes) != 0 || len(res.Rejected) != 0 {
		t.Fatalf("second sync: %+v %+v %v", res.Changes, res.Rejected, err)
	}
	if gets != first {
		t.Errorf("the second sync read the policy %d times", gets-first)
	}

	// Someone deleted it: the write makes a new object.
	uid["qp"] = "uid-2"
	res, err = client.Sync(context.Background(), "ns", desired, nil)
	if err != nil || len(res.Changes) != 1 || res.Changes[0].Action != "created" {
		t.Errorf("after a delete by hand: %+v %v", res.Changes, err)
	}
}
