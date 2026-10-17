package usage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/render"
	"aigw-ui/internal/secretbox"
	"aigw-ui/internal/store"
)

// service gives a test a database of its own in the Postgres that
// STORE_TEST_DATABASE_URL names and the Redis USAGE_TEST_REDIS_URL names,
// with its keys under a prefix of its own; without both it is skipped.
func service(t *testing.T) (*Service, *store.Store, *Reader, context.Context) {
	t.Helper()
	admin, redisURL := os.Getenv("STORE_TEST_DATABASE_URL"), os.Getenv("USAGE_TEST_REDIS_URL")
	if admin == "" || redisURL == "" {
		t.Skip("STORE_TEST_DATABASE_URL or USAGE_TEST_REDIS_URL is not set")
	}
	ctx := context.Background()
	suffix := make([]byte, 6)
	rand.Read(suffix)
	name := "aigw_usage_test_" + hex.EncodeToString(suffix)
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
	rd, err := New(Options{URL: redisURL, KeyPrefix: name + ":"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		keys, _ := rd.rdb.Keys(ctx, name+":*").Result()
		if len(keys) > 0 {
			rd.rdb.Del(ctx, keys...)
		}
		rd.Close()
		st.Close()
		conn.Exec(ctx, `DROP DATABASE `+name+` WITH (FORCE)`)
		conn.Close(ctx)
	})
	return NewService(st, rd), st, rd, ctx
}

// The history reads, for a cluster that counts tokens, the requests counter
// of every tenant on every model, and its other token counters only where a
// request was counted or it already follows the pair. A tenant whose limits
// changed while a token with its former amount may be in use is read under
// every amount of the model.
func TestHistoryCounters(t *testing.T) {
	svc, st, rd, ctx := service(t)
	set := store.TokenSettings{Issuer: "aigw-ui", Audience: "aigw-fleet", Lifetime: 24 * time.Hour}
	st.SetTokens(set)
	st.SetTokenCounts(true)
	glm, err := st.CreateModel(ctx, store.ModelInput{Name: "glm", DefaultLimit: 1, DefaultWindow: "1d"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tenants := map[string]store.Tenant{}
	for _, slug := range []string{"team-a", "team-b", "team-c"} {
		tn, err := st.CreateTenant(ctx, slug, slug)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.CreateToken(ctx, tn.ID, "t", set, now); err != nil {
			t.Fatal(err)
		}
		tenants[slug] = tn
	}
	site, err := st.CreateCluster(ctx, store.Cluster{Name: "site-a", Namespace: "gw", GatewayName: "gw", AuthEnabled: true}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyDiscovery(ctx, site.ID, []store.DiscoveredModel{{Name: "glm", Backends: []store.BackendRef{{Name: "glm", Model: "glm"}}}}, ""); err != nil {
		t.Fatal(err)
	}
	// Two amounts on the model: team-a's and team-c's.
	if err := st.UpsertQuota(ctx, tenants["team-a"].ID, glm, 1000, "1h", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertQuota(ctx, tenants["team-c"].ID, glm, 500, "1h", nil); err != nil {
		t.Fatal(err)
	}
	for kind, v := range map[render.TokenKind]int64{render.TokenRequests: 2, render.TokenInput: 3000, render.TokenCached: 1000, render.TokenOutput: 50} {
		if err := rd.rdb.Set(ctx, render.TokenCounterKey(rd.Prefix, "gw", kind, "team-a", "glm", now), v, time.Hour).Err(); err != nil {
			t.Fatal(err)
		}
	}

	busy := map[store.UsageKey]bool{{TenantID: tenants["team-b"].ID, ModelID: glm}: true}
	counters, err := svc.HistoryCounters(ctx, now, func(k store.UsageKey) bool { return busy[k] })
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[string]map[string]int64{}
	amounts := map[string][]string{}
	for _, c := range counters {
		slug := ""
		for s, tn := range tenants {
			if tn.ID == c.TenantID {
				slug = s
			}
		}
		switch c.Kind {
		case store.KindUsed, store.KindBestEffort:
			for _, a := range []string{"1000-1h", "500-1h"} {
				if strings.Contains(c.Key, "|"+a+"-match-0") && c.Kind == store.KindUsed {
					amounts[slug] = append(amounts[slug], a)
				}
			}
		default:
			if tokens[slug] == nil {
				tokens[slug] = map[string]int64{}
			}
			tokens[slug][c.Kind] = c.Value
		}
	}
	if got := tokens["team-a"]; len(got) != 4 || got["requests"] != 2 || got["input"] != 3000 || got["cached"] != 1000 || got["output"] != 50 {
		t.Errorf("team-a's token counters: %v", got)
	}
	if got := tokens["team-b"]; len(got) != 4 || got["requests"] != 0 {
		t.Errorf("team-b, followed already: %v", got)
	}
	if _, ok := tokens["team-c"]; ok {
		t.Errorf("team-c sent nothing, yet its token counters were read: %v", tokens["team-c"])
	}
	// Both changed their limits just now: each is read under both amounts.
	if len(amounts["team-a"]) != 2 || len(amounts["team-c"]) != 2 || len(amounts["team-b"]) != 0 {
		t.Errorf("amounts read: %v", amounts)
	}
}

// A tenant whose limits changed lately is read under each amount rule of a
// model once, however many tenants hold the amount, on each route the model
// counts on. Taking every tenant's counter in turn gave each key once per
// holder: at 1,000 tenants up to 850,000 counters a minute, of which 8,500
// differed, and the hub ran out of memory.
func TestFormerAmountsTakeEachRuleOnce(t *testing.T) {
	st := new(store.Store)
	st.SetTokens(store.TokenSettings{Lifetime: 24 * time.Hour})
	svc := NewService(st, &Reader{})
	now := time.Now()
	changed := now.Add(-time.Hour)
	model := render.Model{Name: "glm", Slug: "glm", DefaultLimit: 1, DefaultWindow: "1d", ByAmount: true, BestEffort: true,
		Fleet:   []render.FleetSite{{Name: "site-a", Host: "llm.site-a.example.com", Port: 443, Weight: 100}},
		Amounts: []render.QuotaAmount{{Slot: 0, Limit: 1000, Window: "1h"}, {Slot: 1, Limit: 500, Window: "1h"}}}
	f := fleet{models: []store.Model{{ID: "m-glm", Name: "glm", Slug: "glm", Unit: "tokens"}}}
	for i := range 40 {
		slug := fmt.Sprintf("team-%02d", i)
		f.tenants = append(f.tenants, store.Tenant{ID: "t-" + slug, Slug: slug, Enabled: true, LimitsChangedAt: &changed})
		model.Quotas = append(model.Quotas, render.TenantQuota{TenantSlug: slug, Slot: i, Limit: int64(500 * (1 + i%2)), Window: "1h"})
	}
	state := render.State{Namespace: "gw", AuthEnabled: true, Identity: render.Identity{Mode: render.IdentityTokens}, Models: []render.Model{model}}
	// Two clusters in one gateway namespace count in the same keys.
	f.states = []render.State{state, state}

	got := svc.formerAmounts(f, now)
	keys := map[string]bool{}
	kinds := map[string]int{}
	for _, c := range got {
		if keys[c.Key] {
			t.Fatalf("key %s more than once", c.Key)
		}
		keys[c.Key] = true
		kinds[c.Kind]++
	}
	// 40 tenants under 2 amounts on 2 routes: the entry route and the
	// best-effort one.
	if len(got) != 160 || kinds[store.KindUsed] != 80 || kinds[store.KindBestEffort] != 80 {
		t.Errorf("%d counters, by kind %v; want 160, 80 used and 80 best-effort", len(got), kinds)
	}
}
