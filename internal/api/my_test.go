package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"aigw-ui/internal/store"
)

func TestHistoryFrom(t *testing.T) {
	now := time.Date(2026, 10, 10, 15, 30, 0, 0, time.UTC)
	for query, want := range map[string]string{
		"":                   "day 2026-09-11",
		"?days=1":            "day 2026-10-10",
		"?step=hour":         "hour 2026-10-09",
		"?step=hour&days=31": "hour 2026-09-10",
		"?days=400":          "day 2025-09-06",
	} {
		step, from, err := historyFrom(httptest.NewRequest("GET", "/api/v1/usage/history"+query, nil), now)
		if got := step + " " + from.Format("2006-01-02"); err != nil || got != want {
			t.Errorf("%q: %s, %v, want %s", query, got, err, want)
		}
	}
	for _, query := range []string{"?step=week", "?days=0", "?days=401", "?step=hour&days=32", "?days=x"} {
		if _, _, err := historyFrom(httptest.NewRequest("GET", "/api/v1/usage/history"+query, nil), now); err == nil {
			t.Errorf("%q: accepted", query)
		}
	}
}

func TestKeyFailures(t *testing.T) {
	var f keyFailures
	now := time.Unix(1791400000, 0)
	for range maxKeyFailures {
		if f.blocked("10.0.0.1", now) {
			t.Fatal("blocked before the limit")
		}
		f.add("10.0.0.1", now)
	}
	if !f.blocked("10.0.0.1", now) || f.blocked("10.0.0.2", now) {
		t.Error("want the one address blocked and no other")
	}
	if f.blocked("10.0.0.1", now.Add(time.Minute)) {
		t.Error("still blocked in the next minute")
	}
}

// The address is the hop the platform's router added, not one a client sent.
func TestClientAddr(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/my/usage", nil)
	r.RemoteAddr = "10.128.0.9:41000"
	if got := clientAddr(r); got != "10.128.0.9" {
		t.Errorf("without the header: %s", got)
	}
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.7")
	if got := clientAddr(r); got != "203.0.113.7" {
		t.Errorf("with the header: %s", got)
	}
}

// A tenant held to some models sees prices for those only, and which they
// are. On the sandbox hub a tenant held to one model was shown the prices of
// four (2026-10-16).
func TestMyUsageShowsTheTenantsModels(t *testing.T) {
	st, ctx := openStore(t)
	s := &Server{st: st}
	tenant, err := st.CreateTenant(ctx, "team-a", "")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, name := range []string{"opus", "sonnet"} {
		id, err := st.CreateModel(ctx, store.ModelInput{Name: name, DefaultLimit: 1, DefaultWindow: "1d"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetPendingPrice(ctx, id, store.Price{Input: 300000, Cached: 30000, Output: 1500000}, true); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	if _, err := st.ApplyDuePrices(ctx); err != nil {
		t.Fatal(err)
	}
	// The tenant used both, so both have a price on its page.
	err = st.RecordUsage(ctx, func(store.Before) (store.Look, error) {
		var out store.Look
		for _, id := range ids {
			key := store.UsageKey{TenantID: tenant.ID, ModelID: id}
			out.Usage = append(out.Usage, store.UsageDelta{UsageKey: key, Hour: time.Now(), Unit: "credits", Used: 100})
			out.Tokens = append(out.Tokens, store.TokenDelta{UsageKey: key, Hour: time.Now(), Requests: 2, Input: 900, Cached: 600, Output: 40})
		}
		return out, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	page := func(tenant store.Tenant) (models, prices, tokens []string) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/v1/my/usage", nil)
		s.getMyUsage(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, tenant)))
		var out myUsage
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		for _, p := range out.Prices {
			prices = append(prices, p.ModelName)
		}
		for _, p := range out.TokenDays {
			tokens = append(tokens, fmt.Sprintf("%s %d/%d/%d/%d", p.ModelName, p.Requests, p.Input, p.Cached, p.Output))
		}
		return out.Models, prices, tokens
	}

	if models, prices, tokens := page(tenant); models != nil || !slices.Equal(prices, []string{"opus", "sonnet"}) ||
		!slices.Equal(tokens, []string{"opus 2/900/600/40", "sonnet 2/900/600/40"}) {
		t.Errorf("every model: models %v, prices %v, tokens %v", models, prices, tokens)
	}
	held, err := st.SetTenantModels(ctx, tenant.ID, []string{ids["opus"]})
	if err != nil {
		t.Fatal(err)
	}
	// What it sent to a model it may no longer call is not shown.
	if models, prices, tokens := page(held); !slices.Equal(models, []string{"opus"}) || !slices.Equal(prices, []string{"opus"}) ||
		!slices.Equal(tokens, []string{"opus 2/900/600/40"}) {
		t.Errorf("held to opus: models %v, prices %v, tokens %v", models, prices, tokens)
	}
	none, err := st.SetTenantModels(ctx, tenant.ID, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if models, prices, _ := page(none); models == nil || len(models) != 0 || len(prices) != 0 {
		t.Errorf("held to no model: models %v, prices %v", models, prices)
	}
}
