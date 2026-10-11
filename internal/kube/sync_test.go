package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

// A server that ignores label selectors returns objects this tool does not
// own. Sync must delete only the ones carrying its label.
func TestSyncPrunesOnlyManagedObjects(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
