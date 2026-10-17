package render

import (
	"sort"
	"strconv"
	"strings"
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
	// maxCEL is the longest CEL expression the gateway takes in one rule.
	maxCEL = 4096
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
	// revoked, or of a deleted tenant, until a few minutes after it expires.
	// Those in DeniedSpans are not listed here.
	Denied []string
	// DeniedSpans refuse the stored tokens (cid is the slug and the jti)
	// that expire within each span: runs of denied tokens, such as a test's
	// tenants deleted together, that no live stored token expires among.
	// Each is one entry instead of a jti per token.
	DeniedSpans []ExpirySpan
	// DeniedClients is the client ID of every personal key whose tokens to
	// refuse: revoked, or of a deleted tenant, until a few minutes after the
	// last token it got expires. Those tokens are not stored, so they are
	// refused by the key they carry (cid), not one by one.
	DeniedClients []string
	// Disabled is the slug of every disabled tenant.
	Disabled []string
}

// ExpirySpan is a run of denied tokens by their expiry (exp, Unix seconds):
// every stored token that expires from From to To, both included, is
// denied. Tokens is how many denied tokens it stands for.
type ExpirySpan struct {
	From, To int64
	Tokens   int
}

// tokens reports whether the gateway checks tokens.
func (i Identity) tokens() bool { return i.Mode == IdentityBoth || i.Mode == IdentityTokens }

// jwtProvider is the hub as the gateway's JWT provider. A token is read from
// where an API key is: Authorization, as OpenAI clients send it, and
// x-api-key, as Anthropic clients do. It writes the tenant twice: for the
// gateway's rules, and for the scheduler of the site that serves the
// request, which reaches it through the peer listener or on this site.
// Once no API key is left, the provider also writes the client ID, which no
// API key policy writes any more, and the amount of each own quota the token
// names (quotas, from QuotaClaimHeaders).
func jwtProvider(i Identity, quotas []map[string]any) map[string]any {
	claims := []any{
		map[string]any{"header": TenantHeader, "claim": "sub"},
		map[string]any{"header": KeyHeader, "claim": "jti"},
		map[string]any{"header": FairnessHeader, "claim": "sub"},
	}
	// While API keys are taken too, the API key policy writes the client
	// ID, and it must stay: the JWT filter removes every header it writes
	// before it reads a token, and a request with an API key has none.
	if i.Mode == IdentityTokens {
		claims = append(claims,
			map[string]any{"header": ClientIDHeader, "claim": "cid"},
			// A token with a budget across models: the budget rule matches it.
			map[string]any{"header": BudgetHeader, "claim": BudgetClaim})
		for _, q := range quotas {
			claims = append(claims, q)
		}
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
// or of a deleted tenant (by jti, or by its expiry within a span of denied
// ones), one of a disabled tenant (by sub). While
// API keys are taken too, the gateway lets a token through its JWT check even
// when it does not check out, and the API key policy takes it as one of the
// keys it lists, so two more rules follow: a token that checks out passes,
// and any other credential shaped like a token, such as an expired token
// still listed, is refused. An API key never looks like a token.
//
// With some tenant held to some models, one more rule refuses a request for
// a model its token does not list (HTTP 403). With API keys still taken a
// request with one carries no token and is not held by it.
func authorization(i Identity, restricted []TenantModels) map[string]any {
	var rules []any
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
	deny("revoked", "jti", i.Denied)
	rules = append(rules, spanRules(i.DeniedSpans)...)
	deny("revoked-personal-key", "cid", i.DeniedClients)
	deny("disabled-tenant", "sub", i.Disabled)
	if len(restricted) > 0 {
		rules = append(rules, modelsRule())
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

// spanRules returns the rules that refuse the stored tokens that expire
// within a span of DeniedSpans, as many spans to a rule as its CEL takes.
//
// The rule reads the claims the gateway's JWT filter keeps for a token it
// checked. A stored token's cid is its tenant's slug and its jti; a token a
// personal key got has the key's ID there, so it never matches: those are
// refused by DeniedClients. A token signed after the spans were worked out
// expires after every span (the store leaves the last minutes out). An
// expression that fails counts as no match, so each claim is checked for
// first. The numbers are doubles, as the claims are once read.
//
// Envoy builds CEL without comprehensions and without joining strings
// (filters/common/expr/evaluator.cc, "security-oriented defaults"), and
// refuses the whole route configuration when an expression uses them: 0.32.0
// wrote the spans as a list with exists() and compared cid with
// sub + '.' + jti, and the lab's proxy then took no change at all, revokes
// and new quotas included (2026-10-17). So the spans are a chain of
// comparisons and the cid is tested with endsWith (envoyCEL in the tests).
func spanRules(spans []ExpirySpan) []any {
	if len(spans) == 0 {
		return nil
	}
	claims := "metadata.filter_metadata['" + jwtMetadata + "']['" + TokenProvider + "']"
	claim := func(name string) string { return claims + "['" + name + "']" }
	head := "'" + jwtMetadata + "' in metadata.filter_metadata" +
		" && '" + TokenProvider + "' in metadata.filter_metadata['" + jwtMetadata + "']" +
		" && 'exp' in " + claims + " && 'cid' in " + claims + " && 'jti' in " + claims +
		" && " + claim("cid") + ".endsWith(" + claim("jti") + ") && ("
	const tail = ")"
	var rules []any
	var list []string
	size := len(head) + len(tail)
	flush := func() {
		rules = append(rules, map[string]any{"name": "revoked-span", "action": "Deny", "cel": head + strings.Join(list, " || ") + tail})
		list, size = nil, len(head)+len(tail)
	}
	for _, sp := range spans {
		item := "(" + claim("exp") + " >= " + strconv.FormatInt(sp.From, 10) + ".0 && " +
			claim("exp") + " <= " + strconv.FormatInt(sp.To, 10) + ".0)"
		if len(list) > 0 && size+len(item)+4 > maxCEL {
			flush()
		}
		list = append(list, item)
		size += len(item) + 4
	}
	flush()
	return rules
}

// credentialsRemoved is the header mutation of a backend this tool renders
// while tokens are taken: the gateway passes a token on to the model server,
// and during the switch an API key too, so the backend removes both.
func credentialsRemoved() map[string]any {
	return map[string]any{"remove": []any{"authorization", APIKeyHeader}}
}
