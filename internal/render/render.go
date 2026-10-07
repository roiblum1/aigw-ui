// Package render turns the desired state of one cluster into the Kubernetes
// objects that are applied to it. It is pure: no database, no cluster access.
//
// Object names and the namespace feed into the rate limit counter keys, so
// they must come out identical on every cluster for quotas to add up across
// sites.
package render

import (
	"regexp"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	ManagedLabel = "app.kubernetes.io/managed-by"
	ManagedValue = "aigw-ui"

	// ClientIDHeader carries "<tenant-slug>.<key-id>" once the gateway has
	// authenticated the API key.
	ClientIDHeader = "x-aigw-client-id"
	KeysSecretName = "aigw-ui-api-keys"
	AuthPolicyName = "aigw-ui-api-key-auth"

	aigwAPI = "aigateway.envoyproxy.io/v1alpha1"
	egAPI   = "gateway.envoyproxy.io/v1alpha1"
	// ModelHeader is set by the AI gateway from the "model" field of the request body.
	ModelHeader = "x-ai-eg-model"
)

type TenantQuota struct {
	TenantSlug string
	Limit      int64
	Window     string
}

// Target is an AIServiceBackend and the model name requests carry when they
// reach it. QuotaPolicy matches on both.
type Target struct {
	Backend string
	Model   string
}

type Model struct {
	Name          string
	Slug          string
	Host          string
	Port          int
	UpstreamModel string
	DefaultLimit  int64
	DefaultWindow string
	Quotas        []TenantQuota
	// Existing is set for a model discovered on the cluster: its route and
	// backends are already there and owned by someone else, so only the
	// QuotaPolicy is rendered and it is attached to these backends.
	Existing []Target
}

func (m Model) targets() []Target {
	if m.Existing != nil {
		return m.Existing
	}
	return []Target{{Backend: m.Slug, Model: m.UpstreamModel}}
}

type Key struct {
	ClientID string
	Value    string
}

type State struct {
	Namespace   string
	GatewayName string
	AuthEnabled bool
	Models      []Model
	Keys        []Key
}

// Objects returns the desired objects in the order they should be applied.
func Objects(s State) []*unstructured.Unstructured {
	models := append([]Model(nil), s.Models...)
	sort.Slice(models, func(i, j int) bool { return models[i].Slug < models[j].Slug })

	var out []*unstructured.Unstructured
	if s.AuthEnabled {
		out = append(out, keysSecret(s))
	}
	for _, m := range models {
		if m.Existing == nil {
			out = append(out, backend(s, m), aiServiceBackend(s, m), route(s, m))
		}
		if len(m.Quotas) > 0 && len(m.targets()) > 0 {
			out = append(out, quotaPolicy(s, m))
		}
	}
	if s.AuthEnabled {
		out = append(out, authPolicy(s))
	}
	return out
}

func object(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetLabels(map[string]string{ManagedLabel: ManagedValue})
	return u
}

func backend(s State, m Model) *unstructured.Unstructured {
	u := object(egAPI, "Backend", s.Namespace, m.Slug)
	u.Object["spec"] = map[string]any{
		"endpoints": []any{
			map[string]any{"fqdn": map[string]any{"hostname": m.Host, "port": int64(m.Port)}},
		},
	}
	return u
}

func aiServiceBackend(s State, m Model) *unstructured.Unstructured {
	u := object(aigwAPI, "AIServiceBackend", s.Namespace, m.Slug)
	u.Object["spec"] = map[string]any{
		"schema": map[string]any{"name": "OpenAI"},
		"backendRef": map[string]any{
			"group": "gateway.envoyproxy.io",
			"kind":  "Backend",
			"name":  m.Slug,
		},
	}
	return u
}

func route(s State, m Model) *unstructured.Unstructured {
	u := object(aigwAPI, "AIGatewayRoute", s.Namespace, m.Slug)
	u.Object["spec"] = map[string]any{
		"parentRefs": []any{
			map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": s.GatewayName},
		},
		"rules": []any{
			map[string]any{
				"matches": []any{
					map[string]any{"headers": []any{
						map[string]any{"type": "Exact", "name": ModelHeader, "value": m.Name},
					}},
				},
				"backendRefs": []any{
					// QuotaPolicy matches on modelNameOverride, so it is always set.
					map[string]any{"name": m.Slug, "modelNameOverride": m.UpstreamModel},
				},
			},
		},
	}
	return u
}

func quotaPolicy(s State, m Model) *unstructured.Unstructured {
	quotas := append([]TenantQuota(nil), m.Quotas...)
	sort.Slice(quotas, func(i, j int) bool { return quotas[i].TenantSlug < quotas[j].TenantSlug })

	rules := make([]any, 0, len(quotas))
	for _, q := range quotas {
		rules = append(rules, map[string]any{
			"clientSelectors": []any{
				map[string]any{"headers": []any{
					map[string]any{"name": ClientIDHeader, "type": "RegularExpression", "value": TenantClientIDPattern(q.TenantSlug)},
				}},
			},
			"quota": map[string]any{"limit": q.Limit, "duration": q.Window},
		})
	}

	// One target per backend and one quota entry per distinct model name.
	var targetRefs, perModel []any
	seenBackend, seenModel := map[string]bool{}, map[string]bool{}
	for _, t := range m.targets() {
		if !seenBackend[t.Backend] {
			seenBackend[t.Backend] = true
			targetRefs = append(targetRefs, map[string]any{"group": "aigateway.envoyproxy.io", "kind": "AIServiceBackend", "name": t.Backend})
		}
		if !seenModel[t.Model] {
			seenModel[t.Model] = true
			perModel = append(perModel, map[string]any{
				"modelName": t.Model,
				"quota": map[string]any{
					"mode": "Shared",
					// In Shared mode every request is also charged to the default
					// bucket and passes if any matching bucket has quota left, so
					// this pool is what tenants without their own rule can use.
					"defaultBucket": map[string]any{"limit": m.DefaultLimit, "duration": m.DefaultWindow},
					"bucketRules":   rules,
				},
			})
		}
	}

	u := object(aigwAPI, "QuotaPolicy", s.Namespace, m.Slug)
	u.Object["spec"] = map[string]any{"targetRefs": targetRefs, "perModelQuotas": perModel}
	return u
}

func keysSecret(s State) *unstructured.Unstructured {
	data := map[string]any{}
	for _, k := range s.Keys {
		data[k.ClientID] = k.Value
	}
	u := object("v1", "Secret", s.Namespace, KeysSecretName)
	u.Object["type"] = "Opaque"
	u.Object["stringData"] = data
	return u
}

func authPolicy(s State) *unstructured.Unstructured {
	u := object(egAPI, "SecurityPolicy", s.Namespace, AuthPolicyName)
	u.Object["spec"] = map[string]any{
		"targetRefs": []any{
			map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": s.GatewayName},
		},
		"apiKeyAuth": map[string]any{
			"credentialRefs": []any{
				map[string]any{"group": "", "kind": "Secret", "name": KeysSecretName},
			},
			"extractFrom":           []any{map[string]any{"headers": []any{"Authorization"}}},
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

// Redacted returns a copy with secret values masked, for display.
func Redacted(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		c := o.DeepCopy()
		if c.GetKind() == "Secret" {
			if data, ok := c.Object["stringData"].(map[string]any); ok {
				for k := range data {
					data[k] = "<redacted>"
				}
			}
		}
		out = append(out, c)
	}
	return out
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a model name such as "GLM5.3" into a Kubernetes object name.
func Slug(name string) string {
	b := []byte(name)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	s := slugUnsafe.ReplaceAllString(string(b), "-")
	for len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	if len(s) > 50 {
		s = s[:50]
	}
	for len(s) > 0 && s[len(s)-1] == '-' {
		s = s[:len(s)-1]
	}
	return s
}
