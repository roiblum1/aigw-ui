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
		{Name: "GLM5.3", Backends: []ModelBackend{{Name: "glm-fallback", Model: "GLM5.3"}, {Name: "glm-primary", Model: "glm-5.3"}}},
		// Header names are case-insensitive; the backend in another namespace cannot be targeted.
		{Name: "embed", Backends: []ModelBackend{{Name: "embed", Model: "embed"}}},
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
