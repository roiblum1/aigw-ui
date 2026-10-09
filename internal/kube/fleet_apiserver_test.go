package kube

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/render"
)

// This test needs the Envoy Gateway and Envoy AI Gateway CRDs on the test
// API server; hack/test-apiserver.sh installs the real ones. The API server
// then checks every rendered object against their schema and their rules.

func fleetTestState(namespace string, sites ...render.FleetSite) render.State {
	return render.State{
		Namespace: namespace, GatewayName: "ai-gateway", ClientListener: "https", AuthEnabled: true,
		Fleet: render.FleetConfig{PeerSNI: "peers.llm.example.com", CAConfigMap: "llm-peer-ca", ClientSecret: "llm-peer-client", SessionHeader: "x-claude-code-session-id"},
		Keys:  []render.Key{{ClientID: "team-a.01", Value: "sk-team-a"}},
		Models: []render.Model{{
			Name: "glm-5.3", Slug: "glm-5-3", DefaultLimit: 1000, DefaultWindow: "1d",
			Fleet:  sites,
			Quotas: []render.TenantQuota{{TenantSlug: "team-a", Slot: 0, Limit: 500, Window: "1d"}},
		}},
	}
}

func zonesOf(t *testing.T, c *Client, namespace string) []any {
	t.Helper()
	gvr, _ := gvrFor("BackendTrafficPolicy")
	live, err := c.dyn.Resource(gvr).Namespace(namespace).Get(context.Background(), "fleet-glm-5-3", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	zones, _, _ := unstructured.NestedSlice(live.Object, "spec", "loadBalancer", "zoneAware", "weightedZones")
	return zones
}

// The entry route as the real CRDs take it: created, left alone when nothing
// changed, changed when a site leaves, and removed with the model's switch.
func TestRealAPIServerEntryRoute(t *testing.T) {
	c, ns := realClient(t)
	ctx := context.Background()
	site1 := render.FleetSite{Name: "site1-a", Host: "llm.site1-a.example.com", Port: 8443, Weight: 800}
	site2 := render.FleetSite{Name: "site2-a", Host: "llm.site2-a.example.com", Port: 8443, Weight: 1}

	res, err := c.Sync(ctx, ns, render.Objects(fleetTestState(ns, site1, site2)), nil)
	if err != nil {
		t.Fatalf("the API server refused a rendered object: %v", err)
	}
	created := map[string]bool{}
	for _, ch := range res.Changes {
		if ch.Action == "created" {
			created[ch.Kind+"/"+ch.Name] = true
		}
	}
	for _, want := range []string{"Backend/fleet-glm-5-3", "AIServiceBackend/fleet-glm-5-3", "AIGatewayRoute/fleet-glm-5-3",
		"BackendTrafficPolicy/fleet-glm-5-3", "QuotaPolicy/glm-5-3", "SecurityPolicy/" + render.AuthPolicyName} {
		if !created[want] {
			t.Errorf("%s was not created; changes: %+v", want, res.Changes)
		}
	}
	if zones := zonesOf(t, c, ns); len(zones) != 2 {
		t.Errorf("zones = %v, want both sites", zones)
	}

	res, err = c.Sync(ctx, ns, render.Objects(fleetTestState(ns, site1, site2)), nil)
	if err != nil || len(res.Changes) != 0 {
		t.Errorf("a sync that changes nothing: %+v %v", res.Changes, err)
	}

	// site2-a is drained out: it leaves the Backend and the zones together.
	if _, err := c.Sync(ctx, ns, render.Objects(fleetTestState(ns, site1)), nil); err != nil {
		t.Fatal(err)
	}
	if zones := zonesOf(t, c, ns); len(zones) != 1 || zones[0].(map[string]any)["zone"] != "site1-a" {
		t.Errorf("zones after a site left = %v", zones)
	}
	backendGVR, _ := gvrFor("Backend")
	backend, err := c.dyn.Resource(backendGVR).Namespace(ns).Get(ctx, "fleet-glm-5-3", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if endpoints, _, _ := unstructured.NestedSlice(backend.Object, "spec", "endpoints"); len(endpoints) != 1 {
		t.Errorf("endpoints after a site left = %v", endpoints)
	}

	// The switch is turned off: the entry objects go, the keys stay.
	off := fleetTestState(ns)
	off.Models = nil
	res, err = c.Sync(ctx, ns, render.Objects(off), nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted := 0
	for _, ch := range res.Changes {
		if ch.Action == "deleted" {
			deleted++
		}
	}
	if deleted != 5 {
		t.Errorf("deleted %d objects, want the four entry objects and the QuotaPolicy: %+v", deleted, res.Changes)
	}
}
