package kube

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"aigw-ui/internal/render"
)

// This test needs the Envoy Gateway and Envoy AI Gateway CRDs on the test
// API server; hack/test-apiserver.sh installs the real ones. The API server
// then checks every rendered object against their schema and their rules.

func fleetTestState(namespace string, sites ...render.FleetSite) render.State {
	return render.State{
		Namespace: namespace, GatewayName: "ai-gateway", ClientListener: "https", AuthEnabled: true,
		Fleet: render.FleetConfig{PeerSNI: "peers.llm.example.com", CAConfigMap: "llm-peer-ca", ClientSecret: "llm-peer-client", SessionHeaders: []string{"x-claude-code-session-id", "x-openwebui-chat-id"}},
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
		"BackendTrafficPolicy/fleet-glm-5-3", "QuotaPolicy/glm-5-3", "EnvoyPatchPolicy/" + render.RetryPatchName("glm-5-3"),
		"SecurityPolicy/" + render.AuthPolicyName} {
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
	// One site is left, and a single site gets no zone weights.
	if zones := zonesOf(t, c, ns); len(zones) != 0 {
		t.Errorf("zones after a site left = %v, want none for a single site", zones)
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
	if deleted != 6 {
		t.Errorf("deleted %d objects, want the four entry objects, the QuotaPolicy and the retry patch: %+v", deleted, res.Changes)
	}
}

// The best-effort route as the real CRDs take it: its backend and counting
// policy come with the mode, its route with the first tenant past its
// budget, and the route goes again when the last one is back.
func TestRealAPIServerBestEffortRoute(t *testing.T) {
	c, ns := realClient(t)
	ctx := context.Background()
	state := func(overage ...string) render.State {
		s := fleetTestState(ns, render.FleetSite{Name: "site1-a", Host: "llm.site1-a.example.com", Port: 8443, Weight: 800})
		s.Models[0].BestEffort = true
		s.Models[0].Overage = overage
		return s
	}
	changes := func(res SyncResult, action string) map[string]bool {
		out := map[string]bool{}
		for _, ch := range res.Changes {
			if ch.Action == action {
				out[ch.Kind+"/"+ch.Name] = true
			}
		}
		return out
	}
	be := render.BestEffortName("glm-5-3")

	res, err := c.Sync(ctx, ns, render.Objects(state()), nil)
	if err != nil {
		t.Fatalf("the API server refused a rendered object: %v", err)
	}
	created := changes(res, "created")
	for _, want := range []string{"AIServiceBackend/" + be, "QuotaPolicy/" + be} {
		if !created[want] {
			t.Errorf("%s was not created; changes: %+v", want, res.Changes)
		}
	}
	if created["AIGatewayRoute/"+be] {
		t.Error("the best-effort route was created with nobody past its budget")
	}

	res, err = c.Sync(ctx, ns, render.Objects(state("team-a", "team-b")), nil)
	if err != nil {
		t.Fatalf("the API server refused the best-effort route: %v", err)
	}
	created = changes(res, "created")
	for _, want := range []string{"AIGatewayRoute/" + be, "BackendTrafficPolicy/" + be, "EnvoyPatchPolicy/" + be + "-retry"} {
		if !created[want] {
			t.Errorf("%s was not created; changes: %+v", want, res.Changes)
		}
	}
	res, err = c.Sync(ctx, ns, render.Objects(state("team-a", "team-b")), nil)
	if err != nil || len(res.Changes) != 0 {
		t.Errorf("a sync that changes nothing: %+v %v", res.Changes, err)
	}

	res, err = c.Sync(ctx, ns, render.Objects(state()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleted := changes(res, "deleted"); len(deleted) != 3 || !deleted["AIGatewayRoute/"+be] {
		t.Errorf("deleted %v, want the route, its traffic policy and its patch", deleted)
	}
}

// The request class of a best-effort model is accepted by the real CRD, sits
// in the pool's namespace and not the gateway's, and is removed again when
// the model goes back to refusing. hack/test-apiserver.sh installs the CRD.
func TestRealAPIServerBestEffortClass(t *testing.T) {
	c, gatewayNS := realClient(t)
	_, modelNS := realClient(t)
	ctx := context.Background()
	state := render.State{Namespace: gatewayNS, Models: []render.Model{{
		Name: "glm-5.3", Slug: "glm-5-3", Existing: []render.Target{},
		BestEffortPools: []render.Pool{{Namespace: modelNS, Name: "glm-inference-pool", Group: "inference.networking.k8s.io"}},
	}}}
	classes := c.dyn.Resource(schema.GroupVersionResource{Group: "llm-d.ai", Version: "v1alpha2", Resource: "inferenceobjectives"}).Namespace(modelNS)

	res, err := c.Sync(ctx, gatewayNS, render.Objects(state), nil)
	if err != nil {
		t.Fatalf("the API server refused the class: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("skipped %v although the CRD is installed", res.Skipped)
	}
	got, err := classes.Get(ctx, "best-effort", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	priority, _, _ := unstructured.NestedInt64(got.Object, "spec", "priority")
	pool, _, _ := unstructured.NestedString(got.Object, "spec", "poolRef", "name")
	if priority != -1 || pool != "glm-inference-pool" {
		t.Errorf("class has priority %d and pool %q", priority, pool)
	}
	if res, err = c.Sync(ctx, gatewayNS, render.Objects(state), nil); err != nil || len(res.Changes) != 0 {
		t.Errorf("a sync that changes nothing: %+v %v", res.Changes, err)
	}

	// Back to refusing: the class goes, although it is in another namespace.
	state.Models[0].BestEffortPools = nil
	if _, err := c.Sync(ctx, gatewayNS, render.Objects(state), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := classes.Get(ctx, "best-effort", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("the class is still there after the model went back to refusing: %v", err)
	}
}
