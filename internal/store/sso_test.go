package store

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// A tenant may be linked to one OpenShift Group, which no other tenant
// has; unlinked, it is a tenant as before. A person who signed in sees the
// enabled tenants of their groups.
func TestTenantGroups(t *testing.T) {
	s, ctx := open(t)
	a, err := s.CreateTenantInGroup(ctx, "team-a", "A", "ldap-team-a")
	if err != nil || a.SSOGroup != "ldap-team-a" {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := s.CreateTenantInGroup(ctx, "team-x", "X", "ldap-team-a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second tenant in the same group: %v", err)
	}
	b := tenant(t, s, ctx, "team-b")
	if b.SSOGroup != "" {
		t.Fatalf("a tenant without a group: %+v", b)
	}
	if _, err := s.SetTenantGroup(ctx, b.ID, "ldap-team-a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("taking another tenant's group: %v", err)
	}
	if b, err = s.SetTenantGroup(ctx, b.ID, "ldap-team-b"); err != nil || b.SSOGroup != "ldap-team-b" {
		t.Fatalf("%+v %v", b, err)
	}
	slugs := func(groups ...string) []string {
		t.Helper()
		ts, err := s.TenantsInGroups(ctx, groups)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, x := range ts {
			out = append(out, x.Slug)
		}
		return out
	}
	if got := slugs("ldap-team-b", "ldap-team-a", "other"); !reflect.DeepEqual(got, []string{"team-a", "team-b"}) {
		t.Errorf("teams = %v", got)
	}
	// A disabled tenant is nobody's team; one unlinked neither.
	if _, err := s.UpdateTenant(ctx, a.ID, "A", false); err != nil {
		t.Fatal(err)
	}
	if b, err = s.SetTenantGroup(ctx, b.ID, ""); err != nil || b.SSOGroup != "" {
		t.Fatalf("%+v %v", b, err)
	}
	if got := slugs("ldap-team-a", "ldap-team-b"); len(got) != 0 {
		t.Errorf("teams = %v", got)
	}
}

// A person who signed in makes their own personal key; a new one replaces
// the one they had in that tenant, which is revoked. The look at the groups
// sees the live ones in linked tenants only.
func TestOwnedPersonalKeys(t *testing.T) {
	s, ctx := open(t)
	a, err := s.CreateTenantInGroup(ctx, "team-a", "A", "ldap-team-a")
	if err != nil {
		t.Fatal(err)
	}
	plain := tenant(t, s, ctx, "plain")
	first, secret, err := s.CreateOwnedPersonalKey(ctx, a.ID, "alice")
	if err != nil || first.Owner != "alice" || first.Name != "alice" || first.Kind != KindPersonal || secret == "" {
		t.Fatalf("%+v %v", first, err)
	}
	second, _, err := s.CreateOwnedPersonalKey(ctx, a.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.OwnedKey(ctx, a.ID, "alice"); err != nil || got.ID != second.ID {
		t.Fatalf("alice's key = %+v %v", got, err)
	}
	keys, _ := s.ListKeys(ctx, a.ID)
	for _, k := range keys {
		if k.ID == first.ID && k.RevokedAt == nil {
			t.Fatal("the key replaced is not revoked")
		}
	}
	// The replaced key no longer gets tokens.
	if _, err := s.RenewToken(ctx, secret, s.Tokens(), time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renewing with the replaced key: %v", err)
	}
	if _, _, err := s.CreateOwnedPersonalKey(ctx, a.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	// An admin's key and a key in a tenant without a group are not looked at.
	if _, _, err := s.CreatePersonalKey(ctx, a.ID, "shared laptop"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateOwnedPersonalKey(ctx, plain.ID, "carol"); err != nil {
		t.Fatal(err)
	}
	owned, err := s.OwnedKeysInGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range owned {
		got = append(got, o.TenantSlug+"/"+o.Group+"/"+o.Key.Owner)
	}
	if !reflect.DeepEqual(got, []string{"team-a/ldap-team-a/alice", "team-a/ldap-team-a/bob"}) {
		t.Errorf("owned keys = %v", got)
	}
	if _, err := s.OwnedKey(ctx, a.ID, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("no key: %v", err)
	}
}

// Each key's counters feed key_token_hours, and their cursors carry the
// key; the tenant's keep feeding token_hours. The counted keys are the live
// tokens and personal keys of enabled tenants.
func TestKeyTokenHistory(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", false)
	a := tenant(t, s, ctx, "team-a")
	alice, _, err := s.CreateOwnedPersonalKey(ctx, a.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	laptop, _, err := s.CreatePersonalKey(ctx, a.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	ka := UsageKey{TenantID: a.ID, ModelID: glm}
	kAlice := UsageKey{TenantID: a.ID, ModelID: glm, KeyID: alice.ID}
	kLaptop := UsageKey{TenantID: a.ID, ModelID: glm, KeyID: laptop.ID}
	cursor := CounterCursor{Key: "k-alice", UsageKey: kAlice, Kind: "requests", Window: "1h", WindowStart: day, Value: 3}
	look := func(found Look) Before {
		t.Helper()
		var seen Before
		if err := s.RecordUsage(ctx, func(b Before) (Look, error) { seen = b; return found, nil }); err != nil {
			t.Fatal(err)
		}
		return seen
	}
	look(Look{
		Tokens: []TokenDelta{
			{UsageKey: ka, Hour: day.Add(9 * time.Hour), Requests: 5, Input: 500, Output: 50},
			{UsageKey: kAlice, Hour: day.Add(9 * time.Hour), Requests: 3, Input: 300, Cached: 100, Output: 30},
			{UsageKey: kAlice, Hour: day.Add(10 * time.Hour), Requests: 1, Input: 10, Output: 1},
			{UsageKey: kLaptop, Hour: day.Add(9 * time.Hour), Requests: 2, Input: 200, Output: 20},
		},
		Cursors: []CounterCursor{cursor},
	})
	if seen := look(Look{Cursors: []CounterCursor{cursor}}); seen.Cursors["k-alice"] != cursor {
		t.Errorf("the key's cursor = %+v", seen.Cursors["k-alice"])
	}
	tokens, err := s.TokenHistory(ctx, day, StepDay, a.ID)
	if err != nil || len(tokens) != 1 || tokens[0].Requests != 5 {
		t.Fatalf("the tenant's history = %+v %v", tokens, err)
	}
	uses, err := s.KeyUses(ctx, a.ID, day)
	if err != nil {
		t.Fatal(err)
	}
	want := []KeyUse{
		{KeyID: laptop.ID, KeyName: "laptop", ClientID: laptop.ClientID, Kind: KindPersonal, Model: "glm", Requests: 2, Input: 200, Output: 20},
		{KeyID: alice.ID, KeyName: "alice", Owner: "alice", ClientID: alice.ClientID, Kind: KindPersonal, Model: "glm", Requests: 4, Input: 310, Cached: 100, Output: 31},
	}
	if !reflect.DeepEqual(uses, want) {
		t.Errorf("key uses = %+v\nwant %+v", uses, want)
	}
	if uses, _ := s.KeyUses(ctx, a.ID, day.Add(10*time.Hour)); len(uses) != 1 || uses[0].Requests != 1 {
		t.Errorf("from 10:00: %+v", uses)
	}
	if err := s.PruneUsage(ctx, day.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if uses, _ := s.KeyUses(ctx, a.ID, day); len(uses) != 1 {
		t.Errorf("after pruning: %+v", uses)
	}

	// Counted: live tokens and personal keys of enabled tenants.
	if _, _, err := s.CreateKey(ctx, a.ID, "an API key"); err != nil {
		t.Fatal(err)
	}
	gone, _, _ := s.CreatePersonalKey(ctx, a.ID, "gone")
	if _, err := s.RevokeKey(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	off := tenant(t, s, ctx, "off")
	if _, _, err := s.CreatePersonalKey(ctx, off.ID, "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTenant(ctx, off.ID, "", false); err != nil {
		t.Fatal(err)
	}
	counted, err := s.CountedKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, k := range counted {
		ids = append(ids, k.ID)
	}
	if len(ids) != 2 || !((ids[0] == alice.ID && ids[1] == laptop.ID) || (ids[0] == laptop.ID && ids[1] == alice.ID)) {
		t.Errorf("counted keys = %+v", counted)
	}
}
