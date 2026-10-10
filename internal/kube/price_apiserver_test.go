package kube

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/render"
)

// live reads an object back the way the API server stored it. A field the
// CRD does not know would be gone here.
func live(t *testing.T, c *Client, namespace, kind, name string) *unstructured.Unstructured {
	t.Helper()
	gvr, ok := gvrFor(kind)
	if !ok {
		t.Fatalf("no resource for %s", kind)
	}
	u, err := c.dyn.Resource(gvr).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A priced model as the real CRDs take it: the cost expression of its
// prices, every tenant counted and nobody refused in dry-run, the field that
// is taken out of a client's request, and the annotation that makes the
// gateway build the route again.
func TestRealAPIServerPricedModel(t *testing.T) {
	c, ns := realClient(t)
	ctx := context.Background()
	cost := render.Prices{Input: 11574, Cached: 1157, Output: 46296}.Expression()
	state := func(dryRun bool, limit int64) render.State {
		return render.State{
			Namespace: ns, GatewayName: "ai-gateway",
			Models: []render.Model{{
				Name: "glm-5.3", Slug: "glm-5-3", Host: "glm.models.svc.cluster.local", Port: 8000, UpstreamModel: "glm-5.3",
				DefaultLimit: 578_700, DefaultWindow: "1d", CostExpression: cost, DryRun: dryRun,
				Quotas: []render.TenantQuota{{TenantSlug: "team-a", Slot: 0, Limit: limit, Window: "1d"}},
			}},
		}
	}
	quota := func() map[string]any {
		t.Helper()
		perModel, _, _ := unstructured.NestedSlice(live(t, c, ns, "QuotaPolicy", "glm-5-3").Object, "spec", "perModelQuotas")
		if len(perModel) != 1 {
			t.Fatalf("perModelQuotas = %v", perModel)
		}
		return perModel[0].(map[string]any)["quota"].(map[string]any)
	}
	revision := func() string {
		t.Helper()
		return live(t, c, ns, "AIGatewayRoute", "glm-5-3").GetAnnotations()[render.QuotaRevisionAnnotation]
	}

	if _, err := c.Sync(ctx, ns, render.Objects(state(true, 23_148)), nil); err != nil {
		t.Fatalf("the API server refused an object of a priced model: %v", err)
	}
	q := quota()
	if q["costExpression"] != cost {
		t.Errorf("stored cost expression = %v", q["costExpression"])
	}
	rule := q["bucketRules"].([]any)[0].(map[string]any)
	if rule["shadowMode"] != true || q["defaultBucket"].(map[string]any)["limit"] != render.MaxLimit {
		t.Errorf("in dry-run: rule %v, pool %v", rule, q["defaultBucket"])
	}
	remove, _, _ := unstructured.NestedSlice(live(t, c, ns, "AIServiceBackend", "glm-5-3").Object, "spec", "bodyMutation", "remove")
	if !reflect.DeepEqual(remove, []any{"kv_transfer_params"}) {
		t.Errorf("the backend removes %v from the request body", remove)
	}
	dry := revision()
	if dry == "" {
		t.Fatalf("the route has no %s", render.QuotaRevisionAnnotation)
	}

	// Ending dry-run changes the policy and, through the annotation, the route.
	res, err := c.Sync(ctx, ns, render.Objects(state(false, 23_148)), nil)
	if err != nil {
		t.Fatal(err)
	}
	q = quota()
	rule = q["bucketRules"].([]any)[0].(map[string]any)
	if rule["shadowMode"] == true || q["defaultBucket"].(map[string]any)["limit"] != int64(578_700) {
		t.Errorf("enforced: rule %v, pool %v", rule, q["defaultBucket"])
	}
	enforced := revision()
	if enforced == dry {
		t.Errorf("the route's annotation did not change with dry-run; changes: %+v", res.Changes)
	}

	// So does a changed limit, and a sync without a change leaves both alone.
	if _, err := c.Sync(ctx, ns, render.Objects(state(false, 50_000)), nil); err != nil {
		t.Fatal(err)
	}
	if revision() == enforced {
		t.Error("the route's annotation did not change with the tenant's limit")
	}
	res, err = c.Sync(ctx, ns, render.Objects(state(false, 50_000)), nil)
	if err != nil || len(res.Changes) != 0 {
		t.Errorf("a sync that changes nothing: %+v %v", res.Changes, err)
	}
}
