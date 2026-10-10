package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// modelRoute returns an AIGatewayRoute of a model without its spec.
func modelRoute(namespace, name string, m Model) *unstructured.Unstructured {
	u := object(aigwAPI, "AIGatewayRoute", namespace, name)
	u.SetAnnotations(map[string]string{QuotaRevisionAnnotation: quotaRevision(m)})
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
	u.Object["spec"] = serviceBackendSpec(m.Slug)
	return u
}

// serviceBackendSpec is the spec of an AIServiceBackend in front of the
// Backend of that name.
func serviceBackendSpec(backend string) map[string]any {
	return map[string]any{
		"schema": map[string]any{"name": "OpenAI"},
		"backendRef": map[string]any{
			"group": "gateway.envoyproxy.io",
			"kind":  "Backend",
			"name":  backend,
		},
		"bodyMutation": map[string]any{"remove": clientOnlyFields()},
	}
}

// clientOnlyFields are the request fields the gateway takes out before the
// model server sees them. kv_transfer_params is how the parts of a split
// model server talk to each other. From a client it can crash vLLM up to
// 0.29, and from 0.31 it sets the cached token count of the answer, which a
// price for cached prompts would then follow.
func clientOnlyFields() []any { return []any{"kv_transfer_params"} }

func route(s State, m Model) *unstructured.Unstructured {
	u := modelRoute(s.Namespace, m.Slug, m)
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
