package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
// which also refused them while a model had a standard or another tenant had
// a budget: the sandbox hub answered 409 to every budget (2026-10-16).
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

	// Another tenant's budget and a model's standard change nothing.
	if _, err := st.SetTenantBudget(ctx, a.ID, &store.TenantBudget{Credits: 100, Window: "1d"}); err != nil {
		t.Fatal(err)
	}
	m, err := st.CreateModel(ctx, store.ModelInput{Name: "glm", DefaultLimit: 1, DefaultWindow: "1d"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetStandard(ctx, m, &store.Standard{Limit: 10, Window: "1d"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := allowed(); code != http.StatusOK {
		t.Fatalf("with a budget and a standard: %d", code)
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
