package store

import (
	"testing"
	"time"

	"aigw-ui/internal/render"
)

// A tenant's budget across models and its list of models are stored, read
// back, and reach the render state only while the gateways take tokens only;
// a tenant that may call every model has no list.
func TestTenantBudgetAndModels(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	site, err := s.CreateCluster(ctx, Cluster{Name: "site-a", Namespace: "ai-gateway", GatewayName: "gw", AuthEnabled: true}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	glm, err := s.CreateModel(ctx, ModelInput{Name: "glm", DefaultLimit: 1, DefaultWindow: "1d",
		Endpoints: []Endpoint{{ClusterID: site.ID, Host: "glm.glm.svc", Port: 8000}}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := tenant(t, s, ctx, "team-a"), tenant(t, s, ctx, "team-b")
	if a.Budget != nil || a.Models != nil {
		t.Fatalf("a new tenant: %+v", a)
	}
	if any, _ := s.AnyBudget(ctx); any {
		t.Fatal("a budget before any was set")
	}

	a, err = s.SetTenantBudget(ctx, a.ID, &TenantBudget{Credits: 300_000, Window: "1d"})
	if err != nil || a.Budget == nil || a.Budget.Credits != 300_000 || a.Budget.Window != "1d" {
		t.Fatalf("budget = %+v, %v", a.Budget, err)
	}
	if _, err := s.SetTenantBudget(ctx, b.ID, &TenantBudget{Credits: 0, Window: "1d"}); err == nil {
		t.Fatal("a budget of 0 was stored")
	}
	b, err = s.SetTenantModels(ctx, b.ID, []string{glm})
	if err != nil || len(b.Models) != 1 || b.Models[0] != glm {
		t.Fatalf("models = %v, %v", b.Models, err)
	}
	if any, _ := s.AnyBudget(ctx); !any {
		t.Fatal("no budget after one was set")
	}
	tenants, err := s.ListTenants(ctx)
	if err != nil || len(tenants) != 2 || tenants[0].Budget == nil || tenants[1].Models == nil {
		t.Fatalf("list = %+v, %v", tenants, err)
	}

	// No signing key yet: the gateways take API keys, so nothing is rendered.
	st, err := s.RenderStateWithoutKeys(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Budgets) != 0 || len(st.Restricted) != 0 {
		t.Fatalf("rendered while on API keys: %+v %+v", st.Budgets, st.Restricted)
	}
	// With tokens only, both are rendered; the list names the model.
	if _, _, err := s.CreateToken(ctx, a.ID, "t", tokenSet, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st, err = s.RenderStateWithoutKeys(ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	if len(st.Budgets) != 1 || st.Budgets[0] != (render.TenantBudget{TenantSlug: "team-a", Slot: 0, Limit: 300_000, Window: "1d"}) {
		t.Fatalf("budgets %+v", st.Budgets)
	}
	if len(st.Restricted) != 1 || st.Restricted[0].TenantSlug != "team-b" || len(st.Restricted[0].Models) != 1 || st.Restricted[0].Models[0] != "glm" {
		t.Fatalf("restricted %+v", st.Restricted)
	}
	// A disabled tenant is left out; an empty list is a list.
	if _, err := s.UpdateTenant(ctx, a.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if b, err = s.SetTenantModels(ctx, b.ID, []string{}); err != nil || b.Models == nil || len(b.Models) != 0 {
		t.Fatalf("empty list = %v, %v", b.Models, err)
	}
	if st, err = s.RenderStateWithoutKeys(ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	if len(st.Budgets) != 0 || len(st.Restricted) != 1 || len(st.Restricted[0].Models) != 0 {
		t.Fatalf("after disabling a and emptying b: %+v %+v", st.Budgets, st.Restricted)
	}
	// Back to every model, and the budget removed.
	if b, err = s.SetTenantModels(ctx, b.ID, nil); err != nil || b.Models != nil {
		t.Fatalf("every model = %v, %v", b.Models, err)
	}
	if a, err = s.SetTenantBudget(ctx, a.ID, nil); err != nil || a.Budget != nil {
		t.Fatalf("removed = %+v, %v", a.Budget, err)
	}
	if any, _ := s.AnyBudget(ctx); any {
		t.Fatal("a budget after both were removed")
	}
	// Deleting the model drops it from the list.
	if _, err = s.SetTenantModels(ctx, b.ID, []string{glm}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteModel(ctx, glm); err != nil {
		t.Fatal(err)
	}
	if b, err = s.GetTenant(ctx, b.ID); err != nil || b.Models == nil || len(b.Models) != 0 {
		t.Fatalf("after deleting the model: %v, %v", b.Models, err)
	}
}

// A budget takes the lowest free position and keeps it while it exists: a
// new amount keeps it, and another tenant's budget coming or going moves
// nobody. Its position goes with the budget.
func TestTenantBudgetSlots(t *testing.T) {
	s, ctx := open(t)
	a, b, c := tenant(t, s, ctx, "team-a"), tenant(t, s, ctx, "team-b"), tenant(t, s, ctx, "aaa-new")
	slot := func(id string) *int {
		var n *int
		if err := s.db.QueryRow(ctx, `SELECT budget_slot FROM tenants WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	set := func(id string, credits int64) {
		if _, err := s.SetTenantBudget(ctx, id, &TenantBudget{Credits: credits, Window: "1d"}); err != nil {
			t.Fatal(err)
		}
	}
	set(a.ID, 100)
	set(b.ID, 200)
	if *slot(a.ID) != 0 || *slot(b.ID) != 1 {
		t.Fatalf("slots a=%d b=%d", *slot(a.ID), *slot(b.ID))
	}
	set(b.ID, 999)
	if *slot(b.ID) != 1 {
		t.Fatalf("a new amount moved b to %d", *slot(b.ID))
	}
	if _, err := s.SetTenantBudget(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if slot(a.ID) != nil || *slot(b.ID) != 1 {
		t.Fatal("removing a moved b or kept a's position")
	}
	// A tenant that sorts first takes the free position, not b's.
	set(c.ID, 300)
	if *slot(c.ID) != 0 || *slot(b.ID) != 1 {
		t.Fatalf("slots c=%d b=%d", *slot(c.ID), *slot(b.ID))
	}
	set(a.ID, 100)
	if *slot(a.ID) != 2 {
		t.Fatalf("a came back at %d", *slot(a.ID))
	}
}
