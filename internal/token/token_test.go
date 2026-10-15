package token

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func claims(exp time.Duration) Claims {
	return Claims{Issuer: "aigw-ui", Audience: "aigw-fleet", Subject: "acme", ID: "00112233aabbccdd", ClientID: "acme.00112233aabbccdd",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(exp).Unix()}
}

// A token the hub signs checks out against its key, and every way it can be
// wrong is told apart.
func TestSignVerify(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := k.Private.Public().(*ecdsa.PublicKey)
	keys := map[string]*ecdsa.PublicKey{k.ID: pub}
	tok, err := Sign(k, claims(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLike(tok) || LooksLike("sk-0123456789abcdef") {
		t.Fatal("LooksLike")
	}
	c, err := Verify(tok, keys, "aigw-ui", "aigw-fleet", now)
	if err != nil || c != claims(time.Hour) {
		t.Fatalf("verify: %+v %v", c, err)
	}

	other, _ := NewKey()
	forged, _ := Sign(Key{ID: k.ID, Private: other.Private}, claims(time.Hour))
	expired, _ := Sign(k, claims(-time.Second))
	unknown, _ := Sign(other, claims(time.Hour))
	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + strings.Split(expired, ".")[1] + "." + parts[2]
	for name, c := range map[string]struct {
		tok              string
		issuer, audience string
		want             error
	}{
		"forged":      {forged, "aigw-ui", "aigw-fleet", ErrSignature},
		"tampered":    {tampered, "aigw-ui", "aigw-fleet", ErrSignature},
		"expired":     {expired, "aigw-ui", "aigw-fleet", ErrExpired},
		"unknown key": {unknown, "aigw-ui", "aigw-fleet", ErrUnknownKey},
		"other hub":   {tok, "another", "aigw-fleet", ErrClaims},
		"other fleet": {tok, "aigw-ui", "another", ErrClaims},
		"api key":     {"sk-0123456789abcdef", "aigw-ui", "aigw-fleet", ErrMalformed},
	} {
		if _, err := Verify(c.tok, keys, c.issuer, c.audience, now); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

// A key survives being stored, and its public half is what a gateway needs:
// a JWKS with the kid the tokens name, whose key checks them.
func TestKeyStorageAndJWKS(t *testing.T) {
	k, _ := NewKey()
	der, err := k.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseKey(der)
	if err != nil || back.ID != k.ID || len(k.ID) != 16 {
		t.Fatalf("parse: %v %q", err, back.ID)
	}
	var set struct{ Keys []JWK }
	if err := json.Unmarshal([]byte(JWKS([]JWK{k.PublicJWK()})), &set); err != nil || len(set.Keys) != 1 || set.Keys[0].Kid != k.ID ||
		set.Keys[0].Alg != "ES256" || len(set.Keys[0].X) != 43 {
		t.Fatalf("jwks: %+v %v", set, err)
	}
	pub, err := set.Keys[0].Public()
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := Sign(back, claims(time.Hour))
	if _, err := Verify(tok, map[string]*ecdsa.PublicKey{k.ID: pub}, "aigw-ui", "aigw-fleet", now); err != nil {
		t.Fatal(err)
	}
	if JWKS(nil) != `{"keys":[]}` {
		t.Fatal(JWKS(nil))
	}
	bad := set.Keys[0]
	bad.Y = bad.X
	if _, err := bad.Public(); err == nil {
		t.Fatal("a point off the curve was accepted")
	}
}
