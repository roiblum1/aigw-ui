package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/secretbox"
	"aigw-ui/internal/store"
)

// openStore gives a test a database of its own in the Postgres that
// STORE_TEST_DATABASE_URL names, as the store's tests do; without it the
// test is skipped.
func openStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	admin := os.Getenv("STORE_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("STORE_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	suffix := make([]byte, 6)
	rand.Read(suffix)
	name := "aigw_api_test_" + hex.EncodeToString(suffix)
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	box, err := secretbox.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, u.String(), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		conn.Exec(ctx, `DROP DATABASE `+name)
		conn.Close(ctx)
	})
	return st, ctx
}

// A budget across models and a list of models need the gateways on tokens
// only, and nothing else. In 0.22.0 the check was the one for a new API key,
// which also refused them while another tenant had a budget: the sandbox
// hub answered 409 to every budget (2026-10-16).
func TestBudgetsNeedTokensOnly(t *testing.T) {
	st, ctx := openStore(t)
	set := store.TokenSettings{Issuer: "aigw-ui", Audience: "aigw-fleet", Lifetime: 24 * time.Hour}
	st.SetTokens(set)
	s := &Server{st: st}
	allowed := func() int {
		w := httptest.NewRecorder()
		if s.tokensOnly(w, httptest.NewRequest(http.MethodPut, "/api/v1/tenants/x/budget", nil)) {
			return http.StatusOK
		}
		return w.Code
	}

	a, err := st.CreateTenant(ctx, "team-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if code := allowed(); code != http.StatusConflict {
		t.Fatalf("before any token: %d", code)
	}
	if _, _, err := st.CreateToken(ctx, a.ID, "t", set, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := allowed(); code != http.StatusOK {
		t.Fatalf("tokens only: %d", code)
	}

	// Another tenant's budget changes nothing.
	if _, err := st.SetTenantBudget(ctx, a.ID, &store.TenantBudget{Credits: 100, Window: "1d"}); err != nil {
		t.Fatal(err)
	}
	if code := allowed(); code != http.StatusOK {
		t.Fatalf("with a budget: %d", code)
	}

	// An API key again: refused.
	b, err := st.CreateTenant(ctx, "team-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateKey(ctx, b.ID, "k"); err != nil {
		t.Fatal(err)
	}
	if code := allowed(); code != http.StatusConflict {
		t.Fatalf("with an API key: %d", code)
	}
}

// A tenant has one budget across models or quotas of its own on models with
// prices, never both: each is refused while the other is there (HTTP 409),
// naming what stands in the way. Prices that wait for their day count, and
// a quota on a model without prices stays possible beside a budget.
func TestBudgetOrQuotas(t *testing.T) {
	st, ctx := openStore(t)
	set := store.TokenSettings{Issuer: "aigw-ui", Audience: "aigw-fleet", Lifetime: 24 * time.Hour}
	st.SetTokens(set)
	s := &Server{st: st}
	a, err := st.CreateTenant(ctx, "team-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateToken(ctx, a.ID, "t", set, time.Now()); err != nil {
		t.Fatal(err)
	}
	opus, err := st.CreateModel(ctx, store.ModelInput{Name: "opus", DefaultLimit: 1, DefaultWindow: "1d"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetPendingPrice(ctx, opus, store.Price{Input: 1_500_000, Output: 7_500_000, EffectiveAt: time.Now().Add(24 * time.Hour)}, false); err != nil {
		t.Fatal(err)
	}
	free, err := st.CreateModel(ctx, store.ModelInput{Name: "free", DefaultLimit: 1, DefaultWindow: "1d"})
	if err != nil {
		t.Fatal(err)
	}
	put := func(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/tenants/"+a.ID, strings.NewReader(body))
		r.SetPathValue("id", a.ID)
		w := httptest.NewRecorder()
		h(w, r)
		return w
	}

	for _, m := range []string{opus, free} {
		if err := st.UpsertQuota(ctx, a.ID, m, 100, "1d", nil); err != nil {
			t.Fatal(err)
		}
	}
	if names, err := st.PricedQuotaModels(ctx, a.ID); err != nil || strings.Join(names, ",") != "opus" {
		t.Fatalf("quotas on models with prices: %v %v", names, err)
	}
	if w := put(s.setTenantBudget, `{"credits":100,"window":"1d"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "remove its quotas on opus first") {
		t.Fatalf("a budget beside a quota on opus: %d %s", w.Code, w.Body)
	}

	// Without the quota on opus the budget can be set; the quota on the
	// model without prices stays beside it.
	quotas, err := st.ListQuotas(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range quotas {
		if q.ModelID == opus {
			if _, err := st.DeleteQuota(ctx, q.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if names, err := st.PricedQuotaModels(ctx, a.ID); err != nil || len(names) != 0 {
		t.Fatalf("after the removal: %v %v", names, err)
	}
	if _, err := st.SetTenantBudget(ctx, a.ID, &store.TenantBudget{Credits: 100, Window: "1d"}); err != nil {
		t.Fatal(err)
	}
	if w := put(s.upsertQuota, `{"model_id":"`+opus+`","token_limit":5,"window":"1d"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Remove the budget first") {
		t.Fatalf("a quota on opus beside the budget: %d %s", w.Code, w.Body)
	}
	if covered, err := st.BudgetCovers(ctx, a.ID, free); err != nil || covered {
		t.Fatalf("the model without prices: %v %v", covered, err)
	}
	if _, err := st.BudgetCovers(ctx, "00000000-0000-0000-0000-000000000000", opus); err != store.ErrNotFound {
		t.Fatalf("an unknown tenant: %v", err)
	}
}
