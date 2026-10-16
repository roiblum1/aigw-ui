// Package token issues and checks the tokens tenants use as their API keys:
// JSON Web Tokens the hub signs with ES256. The gateways check them against
// the public keys the hub renders into their SecurityPolicy, so no cluster
// calls the hub to authenticate a request. It is pure: no database.
//
// A token says who the tenant is and which key it is, and nothing else: no
// budget or limit, which change daily while a token lives for months.
package token

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

var b64 = base64.RawURLEncoding

// Key is a signing key: its ID, the kid in a token's header, and the key.
type Key struct {
	ID      string
	Private *ecdsa.PrivateKey
}

// NewKey makes a P-256 signing key.
func NewKey() (Key, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Key{}, err
	}
	return Key{ID: KeyID(&k.PublicKey), Private: k}, nil
}

// KeyID names a public key: the first 12 bytes of the SHA-256 of its DER
// form, URL-safe base64. Two hubs never make the same one by chance.
func KeyID(pub *ecdsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return b64.EncodeToString(sum[:12])
}

// Marshal returns the private key's DER form, for storing encrypted.
func (k Key) Marshal() ([]byte, error) { return x509.MarshalECPrivateKey(k.Private) }

// ParseKey reads a key Marshal wrote.
func ParseKey(der []byte) (Key, error) {
	p, err := x509.ParseECPrivateKey(der)
	if err != nil {
		return Key{}, err
	}
	if p.Curve != elliptic.P256() {
		return Key{}, errors.New("not a P-256 key")
	}
	return Key{ID: KeyID(&p.PublicKey), Private: p}, nil
}

// JWK is a public key as a JSON Web Key.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// PublicJWK returns the key's public half.
func (k Key) PublicJWK() JWK {
	return JWK{Kty: "EC", Crv: "P-256", Alg: "ES256", Use: "sig", Kid: k.ID,
		X: coordinate(k.Private.PublicKey.X), Y: coordinate(k.Private.PublicKey.Y)}
}

func coordinate(n *big.Int) string {
	out := make([]byte, 32)
	n.FillBytes(out)
	return b64.EncodeToString(out)
}

// Public returns the public key a JWK holds.
func (j JWK) Public() (*ecdsa.PublicKey, error) {
	if j.Kty != "EC" || j.Crv != "P-256" {
		return nil, fmt.Errorf("key %s is not an EC P-256 key", j.Kid)
	}
	x, err := b64.DecodeString(j.X)
	if err != nil {
		return nil, err
	}
	y, err := b64.DecodeString(j.Y)
	if err != nil {
		return nil, err
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return nil, fmt.Errorf("key %s is not on the curve", j.Kid)
	}
	return pub, nil
}

// JWKS returns the key set the gateways check tokens against, in the order
// given, as the JSON a SecurityPolicy takes inline.
func JWKS(keys []JWK) string {
	if keys == nil {
		keys = []JWK{}
	}
	out, _ := json.Marshal(struct {
		Keys []JWK `json:"keys"`
	}{keys})
	return string(out)
}

// Claims are what a token says.
type Claims struct {
	// Issuer is the hub, Audience the fleet of gateways.
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	// Subject is the tenant's slug, ID the key's: the gateway passes them
	// on as x-aigw-tenant and x-aigw-key.
	Subject string `json:"sub"`
	ID      string `json:"jti"`
	// ClientID is "<tenant>.<key ID>", the form an API key's client ID has,
	// which the gateway passes on as x-aigw-client-id once no API key is
	// left, so the per-tenant rules keep matching.
	ClientID  string `json:"cid"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	// RequestsPerUnit and Unit are the tenant's budget across models when
	// the token was signed, in credits per Envoy rate limit unit ("DAY"):
	// the gateway's budget rule takes its limit from them. Both are absent
	// for a tenant without a budget.
	RequestsPerUnit int64  `json:"requests_per_unit,omitempty"`
	Unit            string `json:"unit,omitempty"`
	// Models are the models the tenant may call, when it is held to some;
	// an empty list refuses every model. Absent: every model.
	Models *[]string `json:"models,omitempty"`
	// Quotas names, by model slug, the amount of each own quota the tenant
	// had on a model when the token was signed ("500000-1h"): the gateway
	// copies it to a header that picks the model's rule for that amount.
	Quotas map[string]string `json:"quotas,omitempty"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// Sign returns the signed token.
func Sign(k Key, c Claims) (string, error) {
	head, err := json.Marshal(header{Alg: "ES256", Typ: "JWT", Kid: k.ID})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	input := b64.EncodeToString(head) + "." + b64.EncodeToString(body)
	sum := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, k.Private, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64.EncodeToString(sig), nil
}

// LooksLike says whether s has the form of a token rather than of an API
// key: three parts, the first a JSON object. An API key starts with "sk-".
func LooksLike(s string) bool {
	return strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") == 2
}

// Errors of Verify.
var (
	ErrMalformed  = errors.New("not a token")
	ErrUnknownKey = errors.New("signed by a key the hub does not know")
	ErrSignature  = errors.New("the signature does not match")
	ErrExpired    = errors.New("expired")
	ErrClaims     = errors.New("issued for another hub or fleet")
)

// Verify checks a token's signature against keys (by kid), its issuer and
// audience, and that it has not expired at now, and returns its claims.
// Whether its key was revoked is the caller's to check.
func Verify(tok string, keys map[string]*ecdsa.PublicKey, issuer, audience string, now time.Time) (Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformed
	}
	var h header
	if err := decode(parts[0], &h); err != nil || h.Alg != "ES256" {
		return Claims{}, ErrMalformed
	}
	pub, ok := keys[h.Kid]
	if !ok {
		return Claims{}, ErrUnknownKey
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return Claims{}, ErrMalformed
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return Claims{}, ErrSignature
	}
	var c Claims
	if err := decode(parts[1], &c); err != nil {
		return Claims{}, ErrMalformed
	}
	switch {
	case c.Issuer != issuer || c.Audience != audience:
		return Claims{}, ErrClaims
	case now.Unix() >= c.ExpiresAt:
		return Claims{}, ErrExpired
	case c.Subject == "" || c.ID == "":
		return Claims{}, ErrMalformed
	}
	return c, nil
}

func decode(part string, v any) error {
	data, err := b64.DecodeString(part)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
