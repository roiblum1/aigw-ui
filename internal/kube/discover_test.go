package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"k8s.io/client-go/rest"
)

const routeList = `{
  "apiVersion": "aigateway.envoyproxy.io/v1alpha1", "kind": "AIGatewayRouteList", "metadata": {},
  "items": [
    {"apiVersion": "aigateway.envoyproxy.io/v1alpha1", "kind": "AIGatewayRoute",
     "metadata": {"name": "models", "namespace": "ai-gateway"},
     "spec": {"rules": [
       {"matches": [{"headers": [{"type": "Exact", "name": "x-ai-eg-model", "value": "GLM5.3"}]}],
        "backendRefs": [{"name": "glm-primary", "modelNameOverride": "glm-5.3"}, {"name": "glm-fallback", "priority": 1}]},
       {"matches": [{"headers": [{"name": "X-AI-EG-Model", "value": "embed"}]},
                    {"headers": [{"type": "RegularExpression", "name": "x-ai-eg-model", "value": "gpt-.*"}]}],
        "backendRefs": [{"name": "embed"}, {"name": "other-ns", "namespace": "elsewhere"}]},
       {"matches": [{"headers": [{"type": "Exact", "name": "x-ai-eg-model", "value": "judge"}]}],
        "backendRefs": [{"name": "judge-pool", "group": "inference.networking.k8s.io", "kind": "InferencePool"}]},
       {"matches": [{"headers": [{"type": "Exact", "name": "x-tenant", "value": "not-a-model"}]}],
        "backendRefs": [{"name": "ignored"}]}
     ]}},
    {"apiVersion": "aigateway.envoyproxy.io/v1alpha1", "kind": "AIGatewayRoute",
     "metadata": {"name": "second", "namespace": "ai-gateway"},
     "spec": {"rules": [
       {"matches": [{"headers": [{"type": "Exact", "name": "x-ai-eg-model", "value": "GLM5.3"}]}],
        "backendRefs": [{"name": "glm-primary", "modelNameOverride": "glm-5.3"}]}
     ]}},
    {"apiVersion": "aigateway.envoyproxy.io/v1alpha1", "kind": "AIGatewayRoute",
     "metadata": {"name": "ours", "namespace": "ai-gateway", "labels": {"app.kubernetes.io/managed-by": "aigw-ui"}},
     "spec": {"rules": [
       {"matches": [{"headers": [{"type": "Exact", "name": "x-ai-eg-model", "value": "managed-here"}]}],
        "backendRefs": [{"name": "managed-here"}]}
     ]}}
  ]}`

func TestDiscover(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(routeList))
	}))
	defer srv.Close()

	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Discover(context.Background(), "ai-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/apis/aigateway.envoyproxy.io/v1alpha1/namespaces/ai-gateway/aigatewayroutes"; gotPath != want {
		t.Errorf("listed %s, want %s", gotPath, want)
	}
	want := []DiscoveredModel{
		// Listed by two routes, reported once. The fallback has no override, so it gets the route's model name.
		{Name: "GLM5.3", Backends: []ModelBackend{
			{Name: "glm-fallback", Namespace: "ai-gateway", Model: "GLM5.3"},
			{Name: "glm-primary", Namespace: "ai-gateway", Model: "glm-5.3", Override: true},
		}},
		// Header names are case-insensitive; a backend in another namespace keeps its namespace.
		{Name: "embed", Backends: []ModelBackend{
			{Name: "embed", Namespace: "ai-gateway", Model: "embed"},
			{Name: "other-ns", Namespace: "elsewhere", Model: "embed"},
		}},
		// Served only from an InferencePool: listed, but nothing a quota can attach to.
		{Name: "judge"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestDiscoverMissingCRD(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Discover(context.Background(), "ai-gateway"); err == nil {
		t.Error("want an error when the AIGatewayRoute CRD is not installed")
	}
}

func TestDiscoverAttached(t *testing.T) {
	const list = `{"apiVersion":"v1","kind":"List","metadata":{},"items":[
	  {"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"AIGatewayRoute","metadata":{"name":"a","namespace":"team-a"},
	   "spec":{"parentRefs":[{"name":"llm","namespace":"ai-gateway"}],
	           "rules":[{"matches":[{"headers":[{"name":"x-ai-eg-model","value":"glm-5.3"}]}],"backendRefs":[{"name":"glm","modelNameOverride":"glm"}]}]}},
	  {"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"AIGatewayRoute","metadata":{"name":"b","namespace":"ai-gateway"},
	   "spec":{"parentRefs":[{"name":"llm"}],
	           "rules":[{"matches":[{"headers":[{"name":"x-ai-eg-model","value":"judge"}]}],"backendRefs":[{"name":"judge"}]}]}},
	  {"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"AIGatewayRoute","metadata":{"name":"c","namespace":"team-a"},
	   "spec":{"parentRefs":[{"name":"llm"}],
	           "rules":[{"matches":[{"headers":[{"name":"x-ai-eg-model","value":"other-gateway"}]}],"backendRefs":[{"name":"x"}]}]}}
	]}`
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(list))
	}))
	defer srv.Close()
	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.DiscoverAttached(context.Background(), "ai-gateway", "llm")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/apis/aigateway.envoyproxy.io/v1alpha1/aigatewayroutes"; gotPath != want {
		t.Errorf("listed %s, want the all-namespaces path %s", gotPath, want)
	}
	// Route c names a gateway "llm" in its own namespace team-a, which is a different gateway.
	want := []DiscoveredModel{
		{Name: "glm-5.3", Backends: []ModelBackend{{Name: "glm", Namespace: "team-a", Model: "glm", Override: true}}},
		{Name: "judge", Backends: []ModelBackend{{Name: "judge", Namespace: "ai-gateway", Model: "judge"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}
