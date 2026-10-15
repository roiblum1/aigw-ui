package wallet

import (
	"context"
	"strings"
	"testing"
	"time"

	"aigw-ui/internal/store"
)

type fakeStore struct {
	spends  []store.WalletSpend
	closed  map[string]time.Time
	overage []string
	opened  []string
	buckets bool
	models  []store.WalletModel
}

func (f *fakeStore) ReconcileWalletBuckets(context.Context) (bool, error) { return f.buckets, nil }
func (f *fakeStore) OpenWallets(context.Context) ([]string, error)        { return f.opened, nil }
func (f *fakeStore) WalletSpends(context.Context, time.Time, string) ([]store.WalletSpend, error) {
	return f.spends, nil
}
func (f *fakeStore) CloseWallet(_ context.Context, id string, until time.Time, _ int64) (bool, error) {
	if _, ok := f.closed[id]; ok {
		return false, nil
	}
	f.closed[id] = until
	return true, nil
}
func (f *fakeStore) WalletModels(context.Context) ([]store.WalletModel, error) { return f.models, nil }
func (f *fakeStore) StartOverage(_ context.Context, model, tenant string, _ time.Time) (bool, error) {
	f.overage = append(f.overage, model+"/"+tenant)
	return true, nil
}

type fakeSyncer struct{ summaries []string }

func (f *fakeSyncer) Changed(_ context.Context, _, summary string) {
	f.summaries = append(f.summaries, summary)
}

// A tenant at its wallet is closed until its window ends and moved to
// best-effort on the models that serve a spent budget so; one below it, or
// closed already, is left alone; the clusters are synced once, and not at
// all when nothing changed.
func TestTick(t *testing.T) {
	end := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	until := end
	st := &fakeStore{closed: map[string]time.Time{}, models: []store.WalletModel{{ID: "glm", Name: "glm"}, {ID: "qwen", Name: "qwen", BestEffort: true}},
		spends: []store.WalletSpend{
			{TenantID: "a", Slug: "team-a", Wallet: store.Wallet{Credits: 1000, Window: "1d"}, Spent: 1000, End: end},
			{TenantID: "b", Slug: "team-b", Wallet: store.Wallet{Credits: 1000, Window: "1d"}, Spent: 999, End: end},
			{TenantID: "c", Slug: "team-c", Wallet: store.Wallet{Credits: 10, Window: "1h"}, Spent: 50, End: end, ClosedUntil: &until},
		}}
	sy := &fakeSyncer{}
	s := New(st, sy, time.Second)
	if err := s.Tick(context.Background(), end.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(st.closed) != 1 || !st.closed["a"].Equal(end) || strings.Join(st.overage, " ") != "qwen/a" {
		t.Fatalf("closed %v, best-effort %v", st.closed, st.overage)
	}
	if len(sy.summaries) != 1 || !strings.Contains(sy.summaries[0], "team-a spent its wallet") || !strings.Contains(sy.summaries[0], "best-effort on qwen") {
		t.Fatalf("summaries %v", sy.summaries)
	}
	// Nothing new: no sync.
	if err := s.Tick(context.Background(), end.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(sy.summaries) != 1 {
		t.Fatalf("a sync with nothing new: %v", sy.summaries)
	}
	// A window began, and buckets changed: one sync.
	st.opened, st.buckets = []string{"team-c"}, true
	if err := s.Tick(context.Background(), end); err != nil {
		t.Fatal(err)
	}
	if len(sy.summaries) != 2 || !strings.Contains(sy.summaries[1], "team-c's wallet window began") || !strings.Contains(sy.summaries[1], "Wallet buckets") {
		t.Fatalf("summaries %v", sy.summaries)
	}
}
