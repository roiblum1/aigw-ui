package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/token"
)

var tokenSet = TokenSettings{Issuer: "aigw-ui", Audience: "aigw-fleet", Lifetime: 180 * 24 * time.Hour, RenewedLifetime: 7 * 24 * time.Hour}

// A token is signed by a key the hub makes on the first one, checks out
// against the published keys, and names its tenant and key; the hub finds the
// tenant again from it, not from the API key path.
func TestCreateToken(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	team := tenant(t, s, ctx, "team-a")
	if id, _ := s.Identity(ctx); id.Mode != render.IdentityKeys {
		t.Fatalf("before any token: %+v", id)
	}
	now := time.Now()
	k, plain, err := s.CreateToken(ctx, team.ID, "laptop", tokenSet, now)
	if err != nil {
		t.Fatal(err)
	}
	if k.Kind != KindToken || len(k.ClientID) != len("team-a.")+16 || k.KeyPrefix != plain[len(plain)-9:] || k.ExpiresAt == nil ||
		k.ExpiresAt.Sub(now) < 179*24*time.Hour {
		t.Fatalf("key %+v", k)
	}
	keys, err := s.ListSigningKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0].RetiredAt != nil || keys[0].Tokens != 1 {
		t.Fatalf("signing keys %+v %v", keys, err)
	}
	c, err := token.Verify(plain, PublicKeys(keys), "aigw-ui", "aigw-fleet", now)
	if err != nil || c.Subject != "team-a" || c.ClientID != k.ClientID || "team-a."+c.ID != k.ClientID {
		t.Fatalf("claims %+v %v", c, err)
	}
	if got, err := s.TenantOfToken(ctx, c); err != nil || got.ID != team.ID {
		t.Fatalf("tenant of token: %+v %v", got, err)
	}
	if _, err := s.TenantOfKey(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a token was taken as an API key: %v", err)
	}
	// A second token is signed by the same key.
	if _, _, err := s.CreateToken(ctx, team.ID, "ci", tokenSet, now); err != nil {
		t.Fatal(err)
	}
	if keys, _ = s.ListSigningKeys(ctx); len(keys) != 1 || keys[0].Tokens != 2 {
		t.Fatalf("second token: %+v", keys)
	}
	if got, _ := s.GetTenant(ctx, team.ID); got.KeyCount != 2 || got.APIKeyCount != 0 {
		t.Fatalf("counts %+v", got)
	}
}

// The gateways take API keys until the first token, both while an enabled
// tenant has an API key left, then tokens only. A revoked token, and every
// token of a deleted tenant, is denied until it expires; a disabled tenant is
// denied by its slug.
func TestIdentityModes(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	a, b := tenant(t, s, ctx, "team-a"), tenant(t, s, ctx, "team-b")
	old, _, err := s.CreateKey(ctx, a.ID, "old")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, _, _ := s.CreateToken(ctx, a.ID, "new", tokenSet, now)
	id, err := s.Identity(ctx)
	if err != nil || id.Mode != render.IdentityBoth || id.Issuer != "aigw-ui" || len(id.Denied) != 0 || len(id.Disabled) != 0 {
		t.Fatalf("both: %+v %v", id, err)
	}
	// Both are listed while both are taken; an expired token is not.
	expired, _, _ := s.CreateToken(ctx, a.ID, "short", TokenSettings{Issuer: "aigw-ui", Audience: "aigw-fleet", Lifetime: -time.Hour}, now)
	site, err := s.CreateCluster(ctx, Cluster{Name: "site-a", Namespace: "ai-gateway", GatewayName: "gw", AuthEnabled: true}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.RenderState(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, k := range st.Keys {
		listed[k.ClientID] = true
	}
	if !listed[old.ClientID] || !listed[tok.ClientID] || listed[expired.ClientID] || st.Identity.Mode != render.IdentityBoth {
		t.Fatalf("listed %v, identity %+v", listed, st.Identity)
	}

	if _, err := s.RevokeKey(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if id, _ = s.Identity(ctx); id.Mode != render.IdentityTokens || len(id.Denied) != 0 {
		t.Fatalf("tokens only: %+v", id)
	}
	// Revoking a token denies it; revoking it again changes nothing.
	if _, err := s.RevokeKey(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	jti := tok.ClientID[len("team-a."):]
	if id, _ = s.Identity(ctx); len(id.Denied) != 1 || id.Denied[0] != jti {
		t.Fatalf("revoked: %+v", id)
	}
	// Tenant b's token is denied when b is deleted, and stays denied.
	gone, _, _ := s.CreateToken(ctx, b.ID, "b", tokenSet, now)
	if err := s.DeleteTenant(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if id, _ = s.Identity(ctx); len(id.Denied) != 2 || (id.Denied[0] != gone.ClientID[len("team-b."):] && id.Denied[1] != gone.ClientID[len("team-b."):]) {
		t.Fatalf("deleted tenant: %+v", id)
	}
	if _, err := s.UpdateTenant(ctx, a.ID, "A", false); err != nil {
		t.Fatal(err)
	}
	if id, _ = s.Identity(ctx); len(id.Disabled) != 1 || id.Disabled[0] != "team-a" {
		t.Fatalf("disabled: %+v", id)
	}
	// A denial outlives its token by DenialMargin, since a gateway takes a
	// token for a while past its expiry; then it is dropped, since the
	// gateway refuses the token anyway.
	if _, err := s.db.Exec(ctx, `UPDATE token_denials SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if id, _ = s.Identity(ctx); len(id.Denied) != 2 {
		t.Fatalf("just expired denials: %+v", id)
	}
	if _, err := s.db.Exec(ctx, `UPDATE token_denials SET expires_at = $1`, time.Now().Add(-DenialMargin-time.Second)); err != nil {
		t.Fatal(err)
	}
	if id, _ = s.Identity(ctx); len(id.Denied) != 0 {
		t.Fatalf("expired denials: %+v", id)
	}
}

// Denied tokens go into spans by expiry where at least three follow one
// another with no live token expiring among them; the others, and the ones
// signed in the last minutes, stay jtis.
func TestSpansOf(t *testing.T) {
	d := func(jti string, exp int64) denial { return denial{jti: jti, exp: exp} }
	denied := []denial{
		d("a1", 100), d("a2", 101), d("a3", 101), d("a4", 105), // a run of four
		d("b1", 110), d("b2", 111), // two, after a live token at 107
		d("c1", 120),                                           // as old as a live token
		d("e1", 130), d("e2", 131), d("e3", 132), d("e4", 140), // the last one is too recent
	}
	singles, spans := spansOf(denied, []int64{90, 107, 120, 200}, 140)
	if fmt.Sprint(spans) != "[{100 105 4} {130 132 3}]" || fmt.Sprint(singles) != "[b1 b2 c1 e4]" {
		t.Fatalf("spans %v, jtis %v", spans, singles)
	}
	if singles, spans = spansOf(nil, []int64{1}, 10); singles != nil || spans != nil {
		t.Fatalf("nothing denied: %v %v", singles, spans)
	}
}

// A test's tenants deleted together leave one span instead of a jti per
// token. A tenant that keeps its token splits the run where its token
// expires, and a token revoked on its own stays a jti.
func TestDeniedTokensInSpans(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	start := time.Now().Add(-time.Hour)
	var ids []string
	for n := range 8 {
		tn := tenant(t, s, ctx, fmt.Sprintf("e2e-%d", n))
		ids = append(ids, tn.ID)
		if _, _, err := s.CreateToken(ctx, tn.ID, "e2e", tokenSet, start.Add(time.Duration(n)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	keep := tenant(t, s, ctx, "team-keep")
	if _, _, err := s.CreateToken(ctx, keep.ID, "kept", tokenSet, start.Add(4*time.Minute+30*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := s.DeleteTenant(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	id, err := s.Identity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exp := func(n int) int64 {
		return start.Add(time.Duration(n)*time.Minute + tokenSet.Lifetime).Truncate(time.Second).Unix()
	}
	want := []render.ExpirySpan{{From: exp(0), To: exp(4), Tokens: 5}, {From: exp(5), To: exp(7), Tokens: 3}}
	if !reflect.DeepEqual(id.DeniedSpans, want) || len(id.Denied) != 0 {
		t.Fatalf("spans %v, jtis %v, want %v", id.DeniedSpans, id.Denied, want)
	}
	// A token signed now and revoked at once is a jti: it is among the most
	// recent, which no span takes.
	tok, _, err := s.CreateToken(ctx, keep.ID, "now", tokenSet, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeKey(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if id, _ = s.Identity(ctx); len(id.Denied) != 1 || id.Denied[0] != tok.ClientID[len("team-keep."):] || len(id.DeniedSpans) != 2 {
		t.Fatalf("a token revoked at once: %v %v", id.Denied, id.DeniedSpans)
	}
}

// A new signing key signs from now on; the old one stays published while a
// token it signed is valid, and is dropped once none is.
func TestRotateSigningKey(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	team := tenant(t, s, ctx, "team-a")
	now := time.Now()
	first, firstPlain, _ := s.CreateToken(ctx, team.ID, "one", tokenSet, now)
	newID, err := s.RotateSigningKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, secondPlain, _ := s.CreateToken(ctx, team.ID, "two", tokenSet, now)
	keys, _ := s.ListSigningKeys(ctx)
	if len(keys) != 2 || keys[0].ID != newID || keys[0].RetiredAt != nil || keys[1].RetiredAt == nil || len(GatewayKeys(keys)) != 2 {
		t.Fatalf("after rotation: %+v", keys)
	}
	pub := PublicKeys(keys)
	for _, p := range []string{firstPlain, secondPlain} {
		if _, err := token.Verify(p, pub, "aigw-ui", "aigw-fleet", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RevokeKey(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	keys, _ = s.ListSigningKeys(ctx)
	if g := GatewayKeys(keys); len(g) != 1 || g[0].Kid != newID {
		t.Fatalf("the retired key is still published: %+v", g)
	}
}
