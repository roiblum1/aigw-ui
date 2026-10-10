package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/secretbox"
)

// testDatabaseEnv names a Postgres the tests may create databases in, such
// as postgres://postgres:secret@127.0.0.1:5432/postgres. Without it the
// tests of this package are skipped. Every test gets a database of its own,
// which is dropped when the test ends.
const testDatabaseEnv = "STORE_TEST_DATABASE_URL"

func open(t *testing.T) (*Store, context.Context) {
	t.Helper()
	admin := os.Getenv(testDatabaseEnv)
	if admin == "" {
		t.Skip(testDatabaseEnv + " is not set")
	}
	ctx := context.Background()
	suffix := make([]byte, 6)
	rand.Read(suffix)
	name := "aigw_test_" + hex.EncodeToString(suffix)

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
	s, err := Open(ctx, u.String(), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		conn.Exec(ctx, `DROP DATABASE `+name)
		conn.Close(ctx)
	})
	return s, ctx
}

// model adds a model and, when fleet is set, gives it an entry route. The
// real switch needs a site to send to, which these tests have no use for.
func model(t *testing.T, s *Store, ctx context.Context, name string, fleet bool) string {
	t.Helper()
	id, err := s.CreateModel(ctx, ModelInput{Name: name, DefaultLimit: 1, DefaultWindow: "1d"})
	if err != nil {
		t.Fatal(err)
	}
	if fleet {
		if _, err := s.db.Exec(ctx, `UPDATE models SET fleet = true WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func tenant(t *testing.T, s *Store, ctx context.Context, slug string) Tenant {
	t.Helper()
	tn, err := s.CreateTenant(ctx, slug, slug)
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

func active(t *testing.T, s *Store, ctx context.Context) int {
	t.Helper()
	list, err := s.ActiveOverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

// Every migration applies to an empty database, and a second start finds
// nothing left to do.
func TestMigrationsApplyOnce(t *testing.T) {
	s, ctx := open(t)
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var applied int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	entries, _ := migrations.ReadDir("migrations")
	if applied != len(entries) {
		t.Errorf("%d migrations recorded, %d files", applied, len(entries))
	}
}

func TestTenantKeysAndQuotas(t *testing.T) {
	s, ctx := open(t)
	team := tenant(t, s, ctx, "team-a")
	if _, err := s.CreateTenant(ctx, "team-a", "again"); !errors.Is(err, ErrConflict) {
		t.Errorf("second tenant with the same slug: %v, want ErrConflict", err)
	}

	// A list without entries is [] in the API, so it must not be nil here.
	keys, err := s.ListKeys(ctx, team.ID)
	if err != nil || keys == nil || len(keys) != 0 {
		t.Fatalf("keys of a new tenant = %v, %v, want an empty list", keys, err)
	}
	key, plain, err := s.CreateKey(ctx, team.ID, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) < 20 || plain[:3] != "sk-" || key.KeyPrefix != plain[:9] || key.ClientID[:7] != "team-a." {
		t.Errorf("key %+v with plaintext of %d characters", key, len(plain))
	}
	var stored []byte
	if err := s.db.QueryRow(ctx, `SELECT key_enc FROM api_keys WHERE id = $1`, key.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if opened, err := s.box.Open(stored); err != nil || string(opened) != plain {
		t.Errorf("the stored key does not decrypt to the key that was handed out: %v", err)
	}
	if keys, _ = s.ListKeys(ctx, team.ID); len(keys) != 1 || keys[0].ID != key.ID {
		t.Errorf("keys = %+v", keys)
	}
	if id, err := s.RevokeKey(ctx, key.ID); err != nil || id != key.ClientID {
		t.Errorf("revoke = %q, %v", id, err)
	}
	if _, err := s.RevokeKey(ctx, key.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking a revoked key: %v, want ErrNotFound", err)
	}

	glm := model(t, s, ctx, "glm-5.3", false)
	if err := s.UpsertQuota(ctx, team.ID, glm, 1000, "1h", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertQuota(ctx, team.ID, glm, 2000, "1d", nil); err != nil {
		t.Fatal(err)
	}
	quotas, err := s.ListQuotas(ctx, team.ID)
	if err != nil || len(quotas) != 1 || quotas[0].TokenLimit != 2000 || quotas[0].Window != "1d" || quotas[0].ModelName != "glm-5.3" {
		t.Fatalf("quotas = %+v, %v, want the one quota with its new values", quotas, err)
	}
	if label, err := s.DeleteQuota(ctx, quotas[0].ID); err != nil || label != "team-a on glm-5.3" {
		t.Errorf("delete = %q, %v", label, err)
	}
}

func TestSpentModeNeedsTheEntryRoute(t *testing.T) {
	s, ctx := open(t)
	plain := model(t, s, ctx, "plain", false)
	if _, err := s.SetModelSpentMode(ctx, plain, SpentBestEffort, false); !errors.Is(err, ErrNoEntryRoute) {
		t.Errorf("best-effort without an entry route: %v, want ErrNoEntryRoute", err)
	}

	glm := model(t, s, ctx, "glm", true)
	if _, err := s.SetModelSpentMode(ctx, glm, SpentBestEffort, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetModelFleet(ctx, glm, false); !errors.Is(err, ErrBestEffortOn) {
		t.Errorf("entry route off in best-effort mode: %v, want ErrBestEffortOn", err)
	}

	// Back to refusing: nobody stays on the best-effort route.
	team := tenant(t, s, ctx, "team-a")
	if err := s.UpsertQuota(ctx, team.ID, glm, 100, "1h", nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.StartOverage(ctx, glm, team.ID, time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	if _, err := s.SetModelSpentMode(ctx, glm, SpentRefuse, true); err != nil {
		t.Fatal(err)
	}
	if n := active(t, s, ctx); n != 0 {
		t.Errorf("%d overage periods still running after the model went back to refusing", n)
	}
	models, _ := s.ListModels(ctx)
	for _, m := range models {
		if m.ID == glm && (m.SpentMode != SpentRefuse || m.BestEffortUnlimited) {
			t.Errorf("model = %+v, want refuse and no unlimited best-effort", m)
		}
	}
}

func TestOveragePeriod(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", true)
	team := tenant(t, s, ctx, "team-a")
	until := time.Now().Add(time.Hour)
	quota := func(limit int64) {
		t.Helper()
		if err := s.UpsertQuota(ctx, team.ID, glm, limit, "1h", nil); err != nil {
			t.Fatal(err)
		}
	}
	start := func() bool {
		t.Helper()
		ok, err := s.StartOverage(ctx, glm, team.ID, until)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	quota(100)
	if !start() || start() {
		t.Error("want the first start to record a period and the second to find it running")
	}
	quota(100)
	if active(t, s, ctx) != 1 {
		t.Error("saving the quota unchanged ended the period")
	}
	quota(500)
	if active(t, s, ctx) != 0 {
		t.Error("a changed quota is a new budget, and the period is still running")
	}

	start()
	quotas, _ := s.ListQuotas(ctx, team.ID)
	if _, err := s.DeleteQuota(ctx, quotas[0].ID); err != nil {
		t.Fatal(err)
	}
	if active(t, s, ctx) != 0 {
		t.Error("the period outlived its quota")
	}

	quota(100)
	start()
	if ended, err := s.EndOverage(ctx, glm, team.ID); err != nil || !ended {
		t.Errorf("end = %v, %v", ended, err)
	}
	if ended, _ := s.EndOverage(ctx, glm, team.ID); ended {
		t.Error("ended a period that was over")
	}
	history, err := s.ListOverage(ctx, 10)
	if err != nil || len(history) != 3 {
		t.Errorf("history has %d periods, %v, want all 3", len(history), err)
	}
}

// The route lists a limited number of tenants, in this order. A tenant that
// was moved because its budget is spent comes before the tenants that are
// there only because they have no quota.
func TestBestEffortTenantsOrder(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", true)
	if _, err := s.SetModelSpentMode(ctx, glm, SpentBestEffort, true); err != nil {
		t.Fatal(err)
	}
	tenant(t, s, ctx, "alpha")
	tenant(t, s, ctx, "beta")
	within := tenant(t, s, ctx, "gamma")
	spent := tenant(t, s, ctx, "zeta")
	off := tenant(t, s, ctx, "disabled")
	if _, err := s.UpdateTenant(ctx, off.ID, "disabled", false); err != nil {
		t.Fatal(err)
	}
	for _, tn := range []Tenant{within, spent} {
		if err := s.UpsertQuota(ctx, tn.ID, glm, 100, "1h", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.StartOverage(ctx, glm, spent.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := s.BestEffortTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"zeta", "alpha", "beta"}
	if len(got[glm]) != len(want) {
		t.Fatalf("tenants = %v, want %v", got[glm], want)
	}
	for i := range want {
		if got[glm][i] != want[i] {
			t.Fatalf("tenants = %v, want %v", got[glm], want)
		}
	}

	// Without "also without a quota" only the moved tenant is left.
	if _, err := s.SetModelSpentMode(ctx, glm, SpentBestEffort, false); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.BestEffortTenants(ctx); len(got[glm]) != 1 || got[glm][0] != "zeta" {
		t.Errorf("tenants = %v, want only zeta", got[glm])
	}
}
