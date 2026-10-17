package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
	if len(st.Budgets) != 0 || len(st.Restricted) != 0 || st.Models[0].BudgetsHold {
		t.Fatalf("rendered while on API keys: %+v %+v %v", st.Budgets, st.Restricted, st.Models[0].BudgetsHold)
	}
	// With tokens only, both are rendered; the list names the model.
	if _, _, err := s.CreateToken(ctx, a.ID, "t", tokenSet, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st, err = s.RenderStateWithoutKeys(ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	if len(st.Budgets) != 1 || st.Budgets[0] != (render.TenantBudget{TenantSlug: "team-a", Limit: 300_000, Window: "1d"}) {
		t.Fatalf("budgets %+v", st.Budgets)
	}
	// The budget holds team-a on the models in place of their own rules.
	if !st.Models[0].BudgetsHold {
		t.Fatal("a budget, yet the models do not let it hold")
	}
	if len(st.Restricted) != 1 || st.Restricted[0].TenantSlug != "team-b" || len(st.Restricted[0].Models) != 1 || st.Restricted[0].Models[0] != "glm" {
		t.Fatalf("restricted %+v", st.Restricted)
	}
	// A disabled tenant is left out; an empty list is a list.
	if _, err := s.UpdateTenant(ctx, a.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if n, err := s.EnabledTenants(ctx); err != nil || n != 1 {
		t.Fatalf("enabled tenants = %d, %v; want 1, b", n, err)
	}
	if b, err = s.SetTenantModels(ctx, b.ID, []string{}); err != nil || b.Models == nil || len(b.Models) != 0 {
		t.Fatalf("empty list = %v, %v", b.Models, err)
	}
	if st, err = s.RenderStateWithoutKeys(ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	if len(st.Budgets) != 0 || len(st.Restricted) != 1 || len(st.Restricted[0].Models) != 0 || st.Models[0].BudgetsHold {
		t.Fatalf("after disabling a and emptying b: %+v %+v %v", st.Budgets, st.Restricted, st.Models[0].BudgetsHold)
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

// A token says what the tenant's budget across models and its list of
// models were when it was signed: the gateways hold it to that. A change
// moves the tenant's LimitsChangedAt, so older tokens can be told apart.
func TestTokensCarryLimits(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	glm, err := s.CreateModel(ctx, ModelInput{Name: "GLM5.3", DefaultLimit: 1, DefaultWindow: "1d"})
	if err != nil {
		t.Fatal(err)
	}
	a := tenant(t, s, ctx, "team-a")
	claims := func() map[string]any {
		_, plain, err := s.CreateToken(ctx, a.ID, "t", tokenSet, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		body, err := base64.RawURLEncoding.DecodeString(strings.Split(plain, ".")[1])
		if err != nil {
			t.Fatal(err)
		}
		var c map[string]any
		if err := json.Unmarshal(body, &c); err != nil {
			t.Fatal(err)
		}
		// The gateway takes the budget from the token only when both claims
		// are there; with one alone it would hold nobody.
		if (c["requests_per_unit"] == nil) != (c["unit"] == nil) {
			t.Fatalf("one budget claim without the other: %v", c)
		}
		return c
	}
	if c := claims(); c["requests_per_unit"] != nil || c["unit"] != nil || c["models"] != nil {
		t.Fatalf("a tenant without limits: %v", c)
	}
	if a.LimitsChangedAt != nil {
		t.Fatal("limits changed before any was set")
	}
	a, err = s.SetTenantBudget(ctx, a.ID, &TenantBudget{Credits: 300_000, Window: "1h"})
	if err != nil || a.LimitsChangedAt == nil {
		t.Fatalf("budget: %v %v", a.LimitsChangedAt, err)
	}
	if _, err := s.SetTenantModels(ctx, a.ID, []string{glm}); err != nil {
		t.Fatal(err)
	}
	c := claims()
	if c["requests_per_unit"] != 300_000.0 || c["unit"] != "HOUR" || fmt.Sprint(c["models"]) != "[GLM5.3]" {
		t.Fatalf("with a budget and a model: %v", c)
	}
	// Held to no model: the claim is an empty list, which refuses every
	// model, not an absent one, which allows every model.
	if _, err := s.SetTenantModels(ctx, a.ID, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTenantBudget(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if c := claims(); c["requests_per_unit"] != nil || fmt.Sprint(c["models"]) != "[]" {
		t.Fatalf("held to no model, without a budget: %v", c)
	}

	// An own quota: the token names its amount by the model's slug, and the
	// change marks the tokens signed before.
	m, err := s.GetModel(ctx, glm)
	if err != nil {
		t.Fatal(err)
	}
	// Marked against the last mark: Postgres's clock is not this one.
	if a, err = s.GetTenant(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	before := *a.LimitsChangedAt
	if err := s.UpsertQuota(ctx, a.ID, glm, 500_000, "1h", nil); err != nil {
		t.Fatal(err)
	}
	if a, err = s.GetTenant(ctx, a.ID); err != nil || !a.LimitsChangedAt.After(before) {
		t.Fatalf("a quota did not mark the tokens: %v %v", a.LimitsChangedAt, err)
	}
	if c := claims(); fmt.Sprint(c["quotas"]) != "map["+m.Slug+":500000-1h]" {
		t.Fatalf("with a quota: %v", c["quotas"])
	}
	quotas, err := s.ListQuotas(ctx, a.ID)
	if err != nil || len(quotas) != 1 {
		t.Fatal(quotas, err)
	}
	before = *a.LimitsChangedAt
	if _, err := s.DeleteQuota(ctx, quotas[0].ID); err != nil {
		t.Fatal(err)
	}
	if a, err = s.GetTenant(ctx, a.ID); err != nil || !a.LimitsChangedAt.After(before) {
		t.Fatalf("removing a quota did not mark the tokens: %v %v", a.LimitsChangedAt, err)
	}
	if c := claims(); c["quotas"] != nil {
		t.Fatalf("without a quota: %v", c["quotas"])
	}
}

// The tenants with the same amount on a model share its position; a new
// amount takes the lowest free one, and an amount nobody has frees its own.
// Position 0 of a new model is the budget-pass rule's, which no amount
// takes. While the gateways take tokens only the render state has them.
func TestQuotaAmounts(t *testing.T) {
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
	a, b, c := tenant(t, s, ctx, "team-a"), tenant(t, s, ctx, "team-b"), tenant(t, s, ctx, "team-c")
	shadow := true
	amounts := func() string {
		rows, err := s.db.Query(ctx, `SELECT slot, token_limit, window_size, shadow FROM quota_amounts WHERE model_id = $1 ORDER BY slot`, glm)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a render.QuotaAmount
			if err := rows.Scan(&a.Slot, &a.Limit, &a.Window, &a.Shadow); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%d:%s", a.Slot, render.QuotaAmountName(a.Limit, a.Window, a.Shadow)))
		}
		return strings.Join(out, " ")
	}
	set := func(tenantID string, limit int64, window string, sh *bool, want string) {
		t.Helper()
		if err := s.UpsertQuota(ctx, tenantID, glm, limit, window, sh); err != nil {
			t.Fatal(err)
		}
		if got := amounts(); got != want {
			t.Fatalf("amounts %q, want %q", got, want)
		}
	}
	set(a.ID, 66, "1h", nil, "1:66-1h")
	set(b.ID, 66, "1h", nil, "1:66-1h")
	set(c.ID, 100, "1d", &shadow, "1:66-1h 2:100-1d-shadow")
	set(a.ID, 7, "1m", nil, "1:66-1h 2:100-1d-shadow 3:7-1m")
	// b leaves 66 an hour, which nobody else has: its position is free,
	// and the next new amount takes it.
	quotas, err := s.ListQuotas(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteQuota(ctx, quotas[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := amounts(); got != "2:100-1d-shadow 3:7-1m" {
		t.Fatalf("after b left: %q", got)
	}
	set(b.ID, 5, "1d", nil, "1:5-1d 2:100-1d-shadow 3:7-1m")
	// The same limit and window, counted only, is an amount of its own.
	set(a.ID, 5, "1d", &shadow, "1:5-1d 2:100-1d-shadow 3:5-1d-shadow")

	// Rendered by amount once the gateways take tokens only.
	st, err := s.RenderStateWithoutKeys(ctx, site.ID)
	if err != nil || st.Models[0].ByAmount {
		t.Fatalf("by amount while on API keys: %v", err)
	}
	if _, _, err := s.CreateToken(ctx, a.ID, "t", tokenSet, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st, err = s.RenderStateWithoutKeys(ctx, site.ID); err != nil {
		t.Fatal(err)
	}
	if m := st.Models[0]; !m.ByAmount || fmt.Sprint(m.Amounts) != "[{1 5 1d false} {2 100 1d true} {3 5 1d true}]" || m.BudgetPassSlot != 0 {
		t.Fatalf("render state: %v %+v %d", m.ByAmount, m.Amounts, m.BudgetPassSlot)
	}
	models, err := s.ListModels(ctx)
	if err != nil || models[0].QuotaAmounts != 4 || models[0].BudgetPassSlot != 0 {
		t.Fatalf("quota amounts %d, budget-pass position %d, %v", models[0].QuotaAmounts, models[0].BudgetPassSlot, err)
	}

	// A model from before 0.32.0 keeps its rule where it was, after its
	// amounts: new amounts fill the free positions before it, then go after.
	if _, err := s.db.Exec(ctx, `UPDATE models SET budget_pass_slot = 4 WHERE id = $1`, glm); err != nil {
		t.Fatal(err)
	}
	d, e := tenant(t, s, ctx, "team-d"), tenant(t, s, ctx, "team-e")
	set(c.ID, 9, "1h", nil, "0:9-1h-shadow 1:5-1d 3:5-1d-shadow")
	set(d.ID, 8, "1h", nil, "0:9-1h-shadow 1:5-1d 2:8-1h 3:5-1d-shadow")
	set(e.ID, 11, "1h", nil, "0:9-1h-shadow 1:5-1d 2:8-1h 3:5-1d-shadow 5:11-1h")
}

// Different new amounts on one model set at the same time each get a
// position of their own: two of them took the same free position and one
// write failed with 409 when 1,000 tenants were seeded 8 at a time (locally,
// 2026-10-17).
func TestQuotaAmountsAtOnce(t *testing.T) {
	s, ctx := open(t)
	glm, err := s.CreateModel(ctx, ModelInput{Name: "glm", DefaultLimit: 1, DefaultWindow: "1d"})
	if err != nil {
		t.Fatal(err)
	}
	var tenants []Tenant
	for n := range 16 {
		tenants = append(tenants, tenant(t, s, ctx, fmt.Sprintf("team-%d", n)))
	}
	errs := make(chan error, len(tenants))
	var wg sync.WaitGroup
	for n, tn := range tenants {
		wg.Go(func() { errs <- s.UpsertQuota(ctx, tn.ID, glm, int64(10+n%8), "1h", nil) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var amounts, slots int
	if err := s.db.QueryRow(ctx, `SELECT count(*), count(DISTINCT slot) FROM quota_amounts WHERE model_id = $1`, glm).Scan(&amounts, &slots); err != nil {
		t.Fatal(err)
	}
	if amounts != 8 || slots != 8 {
		t.Fatalf("%d amounts in %d positions, want 8 and 8", amounts, slots)
	}
}
