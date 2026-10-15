package render

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// How the gateway tells tenants apart.
const (
	// IdentityKeys: API keys only, as before tokens; no token was issued.
	IdentityKeys = "keys"
	// IdentityBoth: during the switch to tokens, some tenant still has an
	// API key. The gateway takes either, and lists every token as an API
	// key too.
	IdentityBoth = "both"
	// IdentityTokens: tokens only; the gateway checks their signature and
	// lists nothing.
	IdentityTokens = "tokens"
)

const (
	// TenantHeader carries the tenant's slug, KeyHeader the key's ID (the
	// token's jti), once the gateway has checked the token. A client cannot
	// set them: the gateway replaces what a client sends.
	TenantHeader = "x-aigw-tenant"
	KeyHeader    = "x-aigw-key"
	// TokenProvider names the hub as the gateway's JWT provider.
	TokenProvider = "aigw-hub"
	// maxClaimValues is the most values the gateway takes in one claim of
	// an authorization rule; longer lists are split over several rules.
	maxClaimValues = 128
)

// Identity is what the gateway needs to check tokens.
type Identity struct {
	// Mode is IdentityKeys, IdentityBoth or IdentityTokens.
	Mode string
	// Issuer and Audience are what every token the hub signs says.
	Issuer, Audience string
	// JWKS is the hub's public keys, as the JSON the policy takes inline:
	// no gateway calls the hub to check a token.
	JWKS string
	// Denied is the jti of every token to refuse although it is signed:
	// revoked, or of a deleted tenant, until it expires.
	Denied []string
	// Disabled is the slug of every disabled tenant.
	Disabled []string
	// Closed are the tenants whose wallet is spent.
	Closed []Closure
}

// Closure is a tenant whose wallet is spent: the gateway refuses its
// requests to Models until Until, by itself, so the closure ends with the
// window also while the hub is down.
type Closure struct {
	Slug   string
	Models []string
	Until  time.Time
}

// tokens reports whether the gateway checks tokens.
func (i Identity) tokens() bool { return i.Mode == IdentityBoth || i.Mode == IdentityTokens }

// jwtProvider is the hub as the gateway's JWT provider. A token is read from
// where an API key is: Authorization, as OpenAI clients send it, and
// x-api-key, as Anthropic clients do. Once no API key is left, the provider
// also writes the client ID, which no API key policy writes any more.
func jwtProvider(i Identity) map[string]any {
	claims := []any{
		map[string]any{"header": TenantHeader, "claim": "sub"},
		map[string]any{"header": KeyHeader, "claim": "jti"},
	}
	// While API keys are taken too, the API key policy writes the client
	// ID, and it must stay: the JWT filter removes every header it writes
	// before it reads a token, and a request with an API key has none.
	if i.Mode == IdentityTokens {
		claims = append(claims, map[string]any{"header": ClientIDHeader, "claim": "cid"})
	}
	return map[string]any{
		"name":      TokenProvider,
		"issuer":    i.Issuer,
		"audiences": []any{i.Audience},
		"localJWKS": map[string]any{"type": "Inline", "inline": i.JWKS},
		"extractFrom": map[string]any{"headers": []any{
			map[string]any{"name": "Authorization", "valuePrefix": "Bearer "},
			map[string]any{"name": APIKeyHeader},
		}},
		"claimToHeaders": claims,
	}
}

// authorization returns the rules that refuse a signed token: one revoked
// or of a deleted tenant (by jti), one of a disabled tenant (by sub). While
// API keys are taken too, the gateway lets a token through its JWT check even
// when it does not check out, and the API key policy takes it as one of the
// keys it lists, so two more rules follow: a token that checks out passes,
// and any other credential shaped like a token, such as an expired token
// still listed, is refused. An API key never looks like a token.
func authorization(i Identity) map[string]any {
	rules := walletRules(i.Closed)
	deny := func(name, claim string, values []string) {
		values = append([]string(nil), values...)
		sort.Strings(values)
		for n := 0; n < len(values); n += maxClaimValues {
			chunk := values[n:min(n+maxClaimValues, len(values))]
			vals := make([]any, len(chunk))
			for j, v := range chunk {
				vals[j] = v
			}
			rules = append(rules, map[string]any{
				"name":      name,
				"action":    "Deny",
				"principal": map[string]any{"jwt": map[string]any{"provider": TokenProvider, "claims": []any{map[string]any{"name": claim, "values": vals}}}},
			})
		}
	}
	if i.tokens() {
		deny("revoked", "jti", i.Denied)
		deny("disabled-tenant", "sub", i.Disabled)
	}
	if i.Mode == IdentityBoth {
		rules = append(rules,
			map[string]any{
				"name":   "token",
				"action": "Allow",
				// A principal needs a claim; every token the hub signs has
				// its issuer.
				"principal": map[string]any{"jwt": map[string]any{"provider": TokenProvider, "claims": []any{
					map[string]any{"name": "iss", "values": []any{i.Issuer}},
				}}},
			},
			map[string]any{
				"name":   "token-not-checked",
				"action": "Deny",
				"cel": "('authorization' in request.headers && request.headers['authorization'].startsWith('Bearer eyJ')) || " +
					"('" + APIKeyHeader + "' in request.headers && request.headers['" + APIKeyHeader + "'].startsWith('eyJ'))",
			})
	}
	if len(rules) == 0 {
		return nil
	}
	return map[string]any{"defaultAction": "Allow", "rules": rules}
}

// maxCEL is the longest CEL expression a rule takes, with room to spare.
const maxCEL = 3800

// walletRules refuse each closed tenant on the models its wallet covers. The
// client ID is in every request the gateway takes, from an API key or a
// token, and the model's name is set by the AI gateway before the rules are
// read. A tenant with many models gets several rules.
func walletRules(closed []Closure) []any {
	var rules []any
	for _, c := range closed {
		head := fmt.Sprintf("request.time < timestamp('%s') && '%s' in request.headers && request.headers['%s'].startsWith('%s.') && '%s' in request.headers && request.headers['%s'] in [",
			c.Until.UTC().Format(time.RFC3339), ClientIDHeader, ClientIDHeader, celString(c.Slug), ModelHeader, ModelHeader)
		var names []string
		flush := func() {
			if len(names) == 0 {
				return
			}
			rules = append(rules, map[string]any{"name": "wallet-spent", "action": "Deny", "cel": head + strings.Join(names, ", ") + "]"})
			names = nil
		}
		size := len(head)
		for _, m := range c.Models {
			q := "'" + celString(m) + "'"
			if size+len(q)+3 > maxCEL {
				flush()
				size = len(head)
			}
			names = append(names, q)
			size += len(q) + 2
		}
		flush()
	}
	return rules
}

// celString escapes s for a CEL string in single quotes.
func celString(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// credentialsRemoved is the header mutation of a backend this tool renders
// while tokens are taken: the gateway passes a token on to the model server,
// and during the switch an API key too, so the backend removes both.
func credentialsRemoved() map[string]any {
	return map[string]any{"remove": []any{"authorization", APIKeyHeader}}
}
