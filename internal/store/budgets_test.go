package store

import (
	"errors"
	"fmt"
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

// A tenant's first budget takes the lowest free position and the tenant
// keeps it: a new amount keeps it, removing the budget keeps it, and another
// tenant's budget coming or going moves nobody.
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
	if got, err := s.SetTenantBudget(ctx, a.ID, nil); err != nil || got.Budget != nil {
		t.Fatalf("removed = %+v, %v", got.Budget, err)
	}
	if slot(a.ID) == nil || *slot(a.ID) != 0 || *slot(b.ID) != 1 {
		t.Fatal("removing a's budget moved b or took a's position")
	}
	// A tenant that sorts first takes the next free position, not a's.
	set(c.ID, 300)
	if *slot(c.ID) != 2 {
		t.Fatalf("c took %d", *slot(c.ID))
	}
	set(a.ID, 100)
	if *slot(a.ID) != 0 {
		t.Fatalf("a came back at %d", *slot(a.ID))
	}
	// Deleting a tenant frees its position.
	if err := s.DeleteTenant(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	d := tenant(t, s, ctx, "team-d")
	set(d.ID, 1)
	if *slot(d.ID) != 0 {
		t.Fatalf("d took %d", *slot(d.ID))
	}
}

// Past the gateway's 256 rules a new budget is refused; a tenant that has
// one can still change it. A position held without a budget goes to a new
// budget when every other one is held.
func TestTenantBudgetsFull(t *testing.T) {
	s, ctx := open(t)
	var first, second string
	for i := 0; i < render.MaxBudgets; i++ {
		tn := tenant(t, s, ctx, fmt.Sprintf("team-%03d", i))
		if _, err := s.SetTenantBudget(ctx, tn.ID, &TenantBudget{Credits: 1, Window: "1d"}); err != nil {
			t.Fatalf("budget %d: %v", i, err)
		}
		switch i {
		case 0:
			first = tn.ID
		case 1:
			second = tn.ID
		}
	}
	extra := tenant(t, s, ctx, "one-too-many")
	budget := &TenantBudget{Credits: 1, Window: "1d"}
	if _, err := s.SetTenantBudget(ctx, extra.ID, budget); !errors.Is(err, ErrBudgetsFull) {
		t.Fatalf("budget %d: %v", render.MaxBudgets+1, err)
	}
	if got, err := s.SetTenantBudget(ctx, first, &TenantBudget{Credits: 5, Window: "1h"}); err != nil || got.Budget.Credits != 5 {
		t.Fatalf("changing a budget when full: %v", err)
	}
	// The second tenant's budget is removed: the extra one takes its
	// position, and the second one is then refused.
	if _, err := s.SetTenantBudget(ctx, second, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTenantBudget(ctx, extra.ID, budget); err != nil {
		t.Fatalf("the extra one with a position held without a budget: %v", err)
	}
	var slot *int
	if err := s.db.QueryRow(ctx, `SELECT budget_slot FROM tenants WHERE id = $1`, second).Scan(&slot); err != nil || slot != nil {
		t.Fatalf("the second one still holds %v, %v", slot, err)
	}
	if _, err := s.SetTenantBudget(ctx, second, budget); !errors.Is(err, ErrBudgetsFull) {
		t.Fatalf("the second one again: %v", err)
	}
}
