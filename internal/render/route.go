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
