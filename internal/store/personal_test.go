package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"aigw-ui/internal/token"
)

// A personal key is shown once and kept as a hash. It gets a token that
// lives RenewedLifetime with the tenant's limits as they are now, under the
// key's client ID; the token checks out against the published keys and
// names the tenant. A revoked key gets none, and the gateways refuse the
// tokens it got by their client ID until the last one expires. Its tenant
// disabled, it gets none either. The key never reaches a gateway.
func TestPersonalKeys(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	site, err := s.CreateCluster(ctx, Cluster{Name: "site-a", Namespace: "ai-gateway", GatewayName: "gw", AuthEnabled: true}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	a := tenant(t, s, ctx, "team-a")
	k, secret, err := s.CreatePersonalKey(ctx, a.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, PersonalKeyPrefix) || k.Kind != KindPersonal || !strings.HasPrefix(k.ClientID, "team-a.") || k.ExpiresAt != nil {
		t.Fatalf("key %+v %q", k, secret)
	}
	var stored []byte
	if err := s.db.QueryRow(ctx, `SELECT coalesce(key_enc, '') FROM api_keys WHERE id = $1`, k.ID).Scan(&stored); err != nil || len(stored) != 0 {
		t.Fatalf("the key is kept, not only its hash: %v", err)
	}
	if _, err := s.SetTenantBudget(ctx, a.ID, &TenantBudget{Credits: 50_000, Window: "1h"}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	r, err := s.RenewToken(ctx, secret, tokenSet, now)
	plain, expires := r.Token, r.Expires
	if err != nil || r.Tenant.Slug != "team-a" || r.Listed {
		t.Fatalf("renew: %+v %v", r.Tenant.Slug, err)
	}
	if d := expires.Sub(now); d < 7*24*time.Hour-time.Second || d > 7*24*time.Hour {
		t.Fatalf("lives %v", d)
	}
	keys, err := s.ListSigningKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c, err := token.Verify(plain, PublicKeys(keys), tokenSet.Issuer, tokenSet.Audience, now)
	if err != nil || c.Subject != "team-a" || c.ClientID != k.ClientID || c.RequestsPerUnit != 50_000 || c.Unit != "HOUR" {
		t.Fatalf("claims %+v %v", c, err)
	}
	// Each token has its own jti; the client ID is the key's.
	second, err := s.RenewToken(ctx, secret, tokenSet, now)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := token.Verify(second.Token, PublicKeys(keys), tokenSet.Issuer, tokenSet.Audience, now)
	if c2.ID == c.ID || c2.ClientID != c.ClientID {
		t.Fatalf("two tokens: %+v %+v", c, c2)
	}
	// The tenant's own page takes the key and its tokens.
	if tn, err := s.TenantOfPersonalKey(ctx, secret); err != nil || tn.ID != a.ID {
		t.Fatalf("tenant of key: %v", err)
	}
	if tn, err := s.TenantOfToken(ctx, c); err != nil || tn.ID != a.ID {
		t.Fatalf("tenant of a renewed token: %v", err)
	}
	// Nothing of it reaches a gateway, and it moves nothing there.
	st, err := s.RenderState(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range st.Keys {
		if key.ClientID == k.ClientID {
			t.Fatal("a personal key is listed for the gateways")
		}
	}
	if _, err := s.RenewToken(ctx, PersonalKeyPrefix+"wrong", tokenSet, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a wrong key: %v", err)
	}

	// Revoked: no new token, and the gateways refuse those it got.
	if _, err := s.RevokeKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewToken(ctx, secret, tokenSet, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked key renewed: %v", err)
	}
	if _, err := s.TenantOfToken(ctx, c); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked key's token signs in: %v", err)
	}
	id, err := s.Identity(ctx)
	if err != nil || !slices.Contains(id.DeniedClients, k.ClientID) {
		t.Fatalf("denied clients %v %v", id.DeniedClients, err)
	}

	// A disabled tenant's key gets no token.
	k2, secret2, err := s.CreatePersonalKey(ctx, a.ID, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTenant(ctx, a.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewToken(ctx, secret2, tokenSet, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a disabled tenant's key renewed: %v", err)
	}
	_ = k2
}

// A retired signing key stays published while a token it signed for a
// personal key is valid, although no such token is stored.
func TestRenewedTokensKeepTheirSigningKey(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	a := tenant(t, s, ctx, "team-a")
	_, secret, err := s.CreatePersonalKey(ctx, a.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.RenewToken(ctx, secret, tokenSet, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plain := r.Token
	if _, err := s.RotateSigningKey(ctx); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListSigningKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(GatewayKeys(keys)) != 2 {
		t.Fatalf("%d published keys, want the new one and the retired one", len(GatewayKeys(keys)))
	}
	if _, err := token.Verify(plain, PublicKeys(keys), tokenSet.Issuer, tokenSet.Audience, time.Now()); err != nil {
		t.Fatalf("a renewed token from before the rotation: %v", err)
	}
}

// While the gateways take API keys too, a personal key's tokens are listed
// with the API keys, under the tenant's slug and the token's jti. A key made
// then gets its first token at once. Renewing gives the same token until a
// new one is signed at half its life; the helper is moved to that one once
// it has had time to reach the gateways, and a key has at most two. A
// revoked key's tokens leave the list. With tokens only, nothing is stored.
func TestPersonalTokensListedWhileAPIKeysAreTaken(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	site, err := s.CreateCluster(ctx, Cluster{Name: "site-a", Namespace: "ai-gateway", GatewayName: "gw", AuthEnabled: true}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	a := tenant(t, s, ctx, "team-a")
	apiKey, _, err := s.CreateKey(ctx, a.ID, "old client")
	if err != nil {
		t.Fatal(err)
	}
	b := tenant(t, s, ctx, "team-b")
	k, secret, err := s.CreatePersonalKey(ctx, b.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	listed := func() map[string]string {
		t.Helper()
		st, err := s.RenderState(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, key := range st.Keys {
			if strings.HasPrefix(key.ClientID, "team-b.") {
				out[key.ClientID] = key.Value
			}
		}
		return out
	}
	stored := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(ctx, `SELECT count(*) FROM personal_tokens WHERE key_id = $1`, k.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	keys, err := s.ListSigningKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jti := func(plain string) string {
		t.Helper()
		c, err := token.Verify(plain, PublicKeys(keys), tokenSet.Issuer, tokenSet.Audience, time.Now())
		if err != nil || c.ClientID != k.ClientID {
			t.Fatalf("claims %+v %v", c, err)
		}
		return c.ID
	}

	// Made while API keys are taken: its token is listed before any renewal.
	first := listed()
	if len(first) != 1 {
		t.Fatalf("listed %v", first)
	}
	now := time.Now()
	r, err := s.RenewToken(ctx, secret, tokenSet, now)
	if err != nil || r.Listed || first["team-b."+jti(r.Token)] != r.Token {
		t.Fatalf("the first renewal is not the listed token: %+v %v", r.Listed, err)
	}
	old := r.Token
	// Renewing every few minutes gives the same token and lists nothing new.
	if r, err := s.RenewToken(ctx, secret, tokenSet, now.Add(10*time.Minute)); err != nil || r.Listed || r.Token != old {
		t.Fatalf("renewed again: %v %v", r.Listed, err)
	}
	// At half its life a new one is signed and listed; the helper keeps the
	// old one until the new one has had time to reach the gateways.
	half := now.Add(tokenSet.RenewedLifetime / 2)
	r, err = s.RenewToken(ctx, secret, tokenSet, half)
	if err != nil || !r.Listed || r.Token != old {
		t.Fatalf("at half its life: listed %v, same %v, %v", r.Listed, r.Token == old, err)
	}
	if got := listed(); len(got) != 2 || stored() != 2 {
		t.Fatalf("listed %d, stored %d", len(got), stored())
	}
	r, err = s.RenewToken(ctx, secret, tokenSet, half.Add(listedSettle))
	if err != nil || r.Listed || r.Token == old {
		t.Fatalf("after the new one settled: %v %v", r.Listed, err)
	}
	if d := r.Expires.Sub(half); d <= tokenSet.RenewedLifetime-time.Second || d > tokenSet.RenewedLifetime {
		t.Fatalf("the new token lives %v from when it was signed", d)
	}
	// Once the old one has expired it is no longer stored.
	if _, err := s.RenewToken(ctx, secret, tokenSet, now.Add(tokenSet.RenewedLifetime+time.Minute)); err != nil || stored() != 2 {
		t.Fatalf("stored %d after the first expired: %v", stored(), err)
	}

	// Revoked: its tokens leave the list.
	if _, err := s.RevokeKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if got := listed(); len(got) != 0 {
		t.Fatalf("a revoked key's tokens are listed: %v", got)
	}

	// Tokens only: a new key's tokens are signed each time and not stored.
	if _, err := s.RevokeKey(ctx, apiKey.ID); err != nil {
		t.Fatal(err)
	}
	k, secret, err = s.CreatePersonalKey(ctx, b.ID, "desktop")
	if err != nil {
		t.Fatal(err)
	}
	r1, err := s.RenewToken(ctx, secret, tokenSet, now)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.RenewToken(ctx, secret, tokenSet, now)
	if err != nil || r1.Listed || r2.Listed || r1.Token == r2.Token || stored() != 0 {
		t.Fatalf("tokens only: listed %v %v, same %v, stored %d, %v", r1.Listed, r2.Listed, r1.Token == r2.Token, stored(), err)
	}
}
