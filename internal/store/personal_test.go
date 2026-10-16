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
	plain, expires, got, err := s.RenewToken(ctx, secret, tokenSet, now)
	if err != nil || got.Slug != "team-a" {
		t.Fatalf("renew: %v %v", got.Slug, err)
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
	second, _, _, err := s.RenewToken(ctx, secret, tokenSet, now)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := token.Verify(second, PublicKeys(keys), tokenSet.Issuer, tokenSet.Audience, now)
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
	if _, _, _, err := s.RenewToken(ctx, PersonalKeyPrefix+"wrong", tokenSet, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a wrong key: %v", err)
	}

	// Revoked: no new token, and the gateways refuse those it got.
	if _, err := s.RevokeKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.RenewToken(ctx, secret, tokenSet, now); !errors.Is(err, ErrNotFound) {
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
	if _, _, _, err := s.RenewToken(ctx, secret2, tokenSet, now); !errors.Is(err, ErrNotFound) {
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
	plain, _, _, err := s.RenewToken(ctx, secret, tokenSet, time.Now())
	if err != nil {
		t.Fatal(err)
	}
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
