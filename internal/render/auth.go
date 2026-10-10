package render

import (
	"encoding/base64"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// keysSecret holds one entry per active key: the client ID and the key.
//
// The keys go under data, not stringData. The API server copies stringData
// into data, but server-side apply only records who owns the stringData
// entries, so an entry left out of a later apply stays in data and a revoked
// key keeps working.
func keysSecret(s State) *unstructured.Unstructured {
	data := map[string]any{}
	for _, k := range s.Keys {
		data[k.ClientID] = base64.StdEncoding.EncodeToString([]byte(k.Value))
	}
	u := object("v1", "Secret", s.Namespace, KeysSecretName)
	u.Object["type"] = "Opaque"
	u.Object["data"] = data
	return u
}

func authPolicy(s State) *unstructured.Unstructured {
	// On the whole Gateway the policy would also guard a listener that other
	// gateways forward to. They strip the key before forwarding, so every
	// such request would be refused.
	target := map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": s.GatewayName}
	if s.ClientListener != "" {
		target["sectionName"] = s.ClientListener
	}
	u := object(egAPI, "SecurityPolicy", s.Namespace, AuthPolicyName)
	u.Object["spec"] = map[string]any{
		"targetRefs": []any{target},
		"apiKeyAuth": map[string]any{
			"credentialRefs": []any{
				map[string]any{"group": "", "kind": "Secret", "name": KeysSecretName},
			},
			// Authorization is what OpenAI clients send, x-api-key what
			// Anthropic clients send unless told otherwise.
			"extractFrom":           []any{map[string]any{"headers": []any{"Authorization", APIKeyHeader}}},
			"forwardClientIDHeader": ClientIDHeader,
			"sanitize":              true,
		},
	}
	return u
}

// TenantClientIDPattern matches every key client ID of one tenant, so all of
// a tenant's keys draw from the same bucket.
func TenantClientIDPattern(slug string) string {
	return "^" + regexp.QuoteMeta(slug) + `\.[a-f0-9]+$`
}

// TenantsClientIDPattern matches the client IDs of all the given tenants.
func TenantsClientIDPattern(slugs []string) string {
	quoted := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		quoted = append(quoted, regexp.QuoteMeta(slug))
	}
	return "^(" + strings.Join(quoted, "|") + `)\.[a-f0-9]+$`
}
