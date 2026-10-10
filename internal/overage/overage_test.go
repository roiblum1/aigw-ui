package overage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aigw-ui/internal/store"
	"aigw-ui/internal/usage"
)

type fake struct {
	models  []store.Model
	active  []store.Overage
	quotas  []usage.Quota
	redis   error
	changes []string
}

func (f *fake) ListModels(context.Context) ([]store.Model, error) { return f.models, nil }

func (f *fake) ActiveOverage(context.Context) ([]store.Overage, error) { return f.active, nil }

func (f *fake) StartOverage(_ context.Context, modelID, tenantID string, until time.Time) (bool, error) {
	f.active = append(f.active, store.Overage{ModelID: modelID, ModelName: modelID, TenantID: tenantID, TenantSlug: tenantID, Until: until})
	return true, nil
}

func (f *fake) CapOverage(_ context.Context, modelID, tenantID string) (bool, error) {
	for i, o := range f.active {
		if o.ModelID == modelID && o.TenantID == tenantID && o.CappedAt == nil {
			f.active[i].CappedAt = &o.Until
			return true, nil
		}
	}
	return false, nil
}

func (f *fake) Report(context.Context, string) (usage.Report, error) {
	return usage.Report{Quotas: f.quotas}, f.redis
}

func (f *fake) Changed(_ context.Context, _, summary string) { f.changes = append(f.changes, summary) }

func (f *fake) in(tenant string) bool {
	for _, o := range f.active {
		if o.TenantID == tenant {
			return true
		}
	}
	return false
}

func quota(tenant string, used, limit int64, window string) usage.Quota {
	return usage.Quota{TenantID: tenant, TenantSlug: tenant, ModelID: "glm", ModelName: "glm", Used: used, Limit: limit, Window: window, ResetsAt: time.Unix(1791403200, 0)}
}

func setup() (*fake, *Service) {
	f := &fake{models: []store.Model{
		{ID: "glm", Fleet: true, SpentMode: store.SpentBestEffort},
		{ID: "strict", Fleet: true, SpentMode: store.SpentRefuse},
	}}
	return f, New(f, f, f, 15*time.Second, 0.9)
}

func TestTickMovesAtTheThreshold(t *testing.T) {
	f, s := setup()
	shadow := quota("dry-run", 100, 100, "1h")
	shadow.Shadow = true
	strict := quota("strict-tenant", 100, 100, "1h")
	strict.ModelID = "strict"
	f.quotas = []usage.Quota{
		quota("below", 89, 100, "1h"),
		quota("at", 90, 100, "1h"),
		quota("over", 250, 100, "1d"),
		quota("minute", 100, 100, "1m"),
		shadow, strict,
	}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for tenant, want := range map[string]bool{"below": false, "at": true, "over": true, "minute": false, "dry-run": false, "strict-tenant": false} {
		if f.in(tenant) != want {
			t.Errorf("%s moved = %v, want %v", tenant, f.in(tenant), want)
		}
	}
	// One task, and so one sync, for the whole tick.
	if len(f.changes) != 1 || !strings.Contains(f.changes[0], "at on glm") || !strings.Contains(f.changes[0], "over on glm") {
		t.Errorf("changes = %v, want one line naming both tenants", f.changes)
	}
	if f.active[0].Until.Unix() != 1791403200 {
		t.Errorf("until = %v, want the end of the quota's window", f.active[0].Until)
	}

	// The next look finds the same tenants over the threshold and changes
	// nothing: no task, no sync.
	f.changes = nil
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.changes) != 0 || len(f.active) != 2 {
		t.Errorf("a second look changed something: %v", f.changes)
	}
}

// A tenant stays moved although its counter is no longer over the
// threshold, and comes back when its period is over.
func TestTickNoticesTheEndOfAPeriod(t *testing.T) {
	f, s := setup()
	f.quotas = []usage.Quota{quota("team-a", 95, 100, "1h")}
	s.Tick(context.Background())
	f.quotas = []usage.Quota{quota("team-a", 0, 100, "1h")}
	f.changes = nil
	s.Tick(context.Background())
	if !f.in("team-a") || len(f.changes) != 0 {
		t.Errorf("moved back before the period ended: %v", f.changes)
	}

	f.active = nil // the window ended
	s.Tick(context.Background())
	if len(f.changes) != 1 || !strings.Contains(f.changes[0], "standard again") {
		t.Errorf("changes = %v, want one line for the tenant that is back", f.changes)
	}
}

// Redis being down must not move anyone, in either direction.
func TestTickKeepsTheSetWithoutRedis(t *testing.T) {
	f, s := setup()
	f.quotas = []usage.Quota{quota("team-a", 95, 100, "1h")}
	s.Tick(context.Background())
	f.changes = nil

	f.redis = errors.New("connection refused")
	f.quotas = []usage.Quota{quota("team-b", 99, 100, "1h")}
	if err := s.Tick(context.Background()); err == nil {
		t.Error("no error although Redis is down")
	}
	if !f.in("team-a") || f.in("team-b") || len(f.changes) != 0 {
		t.Errorf("the set changed without Redis: %+v %v", f.active, f.changes)
	}
}

// Without a model in best-effort mode Redis is not asked.
func TestTickDoesNothingWithoutABestEffortModel(t *testing.T) {
	f, s := setup()
	f.models = f.models[1:]
	f.redis = errors.New("must not be asked")
	if err := s.Tick(context.Background()); err != nil {
		t.Errorf("err = %v", err)
	}
}

// A period can end while the server is not running. The first tick cannot
// know, so it has the clusters synced once.
func TestFirstTickSyncsOnce(t *testing.T) {
	f, s := setup()
	s.Tick(context.Background())
	if len(f.changes) != 1 || !strings.Contains(f.changes[0], "server started") {
		t.Errorf("changes = %v, want one line for the start", f.changes)
	}
	s.Tick(context.Background())
	if len(f.changes) != 1 {
		t.Errorf("a later tick synced again: %v", f.changes)
	}

	f, s = setup()
	f.models = f.models[1:]
	s.Tick(context.Background())
	if len(f.changes) != 0 {
		t.Errorf("synced without a best-effort model: %v", f.changes)
	}
}

// A moved tenant that reaches the model's limit on best-effort use is taken
// off the best-effort route once, and stays in its period.
func TestTickCapsBestEffort(t *testing.T) {
	f, s := setup()
	limit := int64(50)
	f.models[0].BestEffortLimit = &limit
	q := quota("team-a", 95, 100, "1h")
	f.quotas = []usage.Quota{q}
	s.Tick(context.Background())

	q.OverageUsed = 49
	f.quotas, f.changes = []usage.Quota{q}, nil
	s.Tick(context.Background())
	if len(f.changes) != 0 || f.active[0].CappedAt != nil {
		t.Errorf("capped below the limit: %v", f.changes)
	}

	q.OverageUsed = 50
	f.quotas = []usage.Quota{q}
	s.Tick(context.Background())
	if len(f.changes) != 1 || !strings.Contains(f.changes[0], "team-a on glm has used 50 tokens as best-effort") || f.active[0].CappedAt == nil {
		t.Errorf("changes = %v, want the tenant capped", f.changes)
	}

	f.changes = nil
	s.Tick(context.Background())
	if len(f.changes) != 0 || !f.in("team-a") {
		t.Errorf("a second look changed something: %v", f.changes)
	}
}
