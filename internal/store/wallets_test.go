package store

import (
	"testing"
	"time"
)

// A wallet tenant gets a bucket equal to its wallet on every priced model it
// has no cap on; a quota set by hand is a cap; the buckets follow the wallet
// and go with it.
func TestWalletBuckets(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", true)
	qwen := model(t, s, ctx, "qwen", true)
	if _, err := s.SetPendingPrice(ctx, glm, Price{Input: 100, Cached: 10, Output: 400, EffectiveAt: time.Now().Add(-time.Second)}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDuePrices(ctx); err != nil {
		t.Fatal(err)
	}
	a := tenant(t, s, ctx, "team-a")
	got, err := s.SetWallet(ctx, a.ID, &Wallet{Credits: 1_000_000, Window: "1d"})
	if err != nil || got.Wallet == nil || got.Wallet.Credits != 1_000_000 || got.QuotaCount != 0 {
		t.Fatalf("set: %+v %v", got, err)
	}
	quotas, _ := s.ListQuotas(ctx, a.ID)
	if len(quotas) != 1 || quotas[0].ModelID != glm || !quotas[0].Wallet || quotas[0].TokenLimit != 1_000_000 || quotas[0].Window != "1d" {
		t.Fatalf("buckets %+v (qwen has no prices: none there)", quotas)
	}
	_ = qwen
	// The wallet changes: its bucket follows.
	if _, err := s.SetWallet(ctx, a.ID, &Wallet{Credits: 50_000, Window: "1h"}); err != nil {
		t.Fatal(err)
	}
	if quotas, _ = s.ListQuotas(ctx, a.ID); quotas[0].TokenLimit != 50_000 || quotas[0].Window != "1h" {
		t.Fatalf("followed %+v", quotas)
	}
	// A quota by hand is a cap inside the wallet, and stays one.
	if err := s.UpsertQuota(ctx, a.ID, glm, 10_000, "1d", nil); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.ReconcileWalletBuckets(ctx); err != nil || changed {
		t.Fatalf("reconcile after a cap: %v %v", changed, err)
	}
	if quotas, _ = s.ListQuotas(ctx, a.ID); quotas[0].Wallet || quotas[0].TokenLimit != 10_000 {
		t.Fatalf("cap %+v", quotas)
	}
	if tn, _ := s.GetTenant(ctx, a.ID); tn.QuotaCount != 1 {
		t.Fatalf("a cap counts as a quota: %+v", tn)
	}
	// Removing the cap gives the bucket back; removing the wallet removes it.
	if _, err := s.DeleteQuota(ctx, quotas[0].ID); err != nil {
		t.Fatal(err)
	}
	if changed, _ := s.ReconcileWalletBuckets(ctx); !changed {
		t.Fatal("no bucket after the cap went")
	}
	if _, err := s.SetWallet(ctx, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if quotas, _ = s.ListQuotas(ctx, a.ID); len(quotas) != 0 {
		t.Fatalf("after the wallet: %+v", quotas)
	}
}

// What a wallet tenant spent in its window comes from the usage history, in
// credits only; a closed tenant is refused on the priced models that do not
// serve it as best-effort, until the window ends.
func TestWalletSpendAndClosure(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", true)
	if _, err := s.SetPendingPrice(ctx, glm, Price{Input: 100, Cached: 10, Output: 400, EffectiveAt: time.Now().Add(-time.Second)}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDuePrices(ctx); err != nil {
		t.Fatal(err)
	}
	a := tenant(t, s, ctx, "team-a")
	if _, err := s.SetWallet(ctx, a.ID, &Wallet{Credits: 1000, Window: "1d"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	start, end := WalletWindow("1d", now)
	for _, row := range []struct {
		hour time.Time
		unit string
		used int64
	}{{start, UnitCredits, 600}, {now.Truncate(time.Hour), UnitCredits, 500}, {start.Add(-time.Hour), UnitCredits, 9999}, {now.Truncate(time.Hour), UnitTokens, 9999}} {
		if _, err := s.db.Exec(ctx, `INSERT INTO usage_hours (tenant_id, model_id, hour, unit, used) VALUES ($1, $2, $3, $4, $5)
		                             ON CONFLICT (tenant_id, model_id, hour, unit) DO UPDATE SET used = usage_hours.used + EXCLUDED.used`,
			a.ID, glm, row.hour, row.unit, row.used); err != nil {
			t.Fatal(err)
		}
	}
	spends, err := s.WalletSpends(ctx, now, "")
	if err != nil || len(spends) != 1 || spends[0].Spent != 1100 || spends[0].ByModel["glm"] != 1100 || !spends[0].End.Equal(end) {
		t.Fatalf("spends %+v %v", spends, err)
	}
	if closed, err := s.CloseWallet(ctx, a.ID, end, 1100); err != nil || !closed {
		t.Fatalf("close: %v %v", closed, err)
	}
	if closed, _ := s.CloseWallet(ctx, a.ID, end, 1200); closed {
		t.Fatal("closed twice")
	}
	id, err := s.Identity(ctx)
	if err != nil || len(id.Closed) != 1 || id.Closed[0].Slug != "team-a" || len(id.Closed[0].Models) != 1 || id.Closed[0].Models[0] != "glm" {
		t.Fatalf("identity %+v %v", id, err)
	}
	if tn, _ := s.GetTenant(ctx, a.ID); tn.ClosedUntil == nil {
		t.Fatal("not closed")
	}
	// The window ends: the closure goes.
	if _, err := s.db.Exec(ctx, `UPDATE wallet_closures SET until = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if opened, err := s.OpenWallets(ctx); err != nil || len(opened) != 1 || opened[0] != "team-a" {
		t.Fatalf("opened %v %v", opened, err)
	}
	if id, _ = s.Identity(ctx); len(id.Closed) != 0 {
		t.Fatalf("still closed %+v", id)
	}
}
