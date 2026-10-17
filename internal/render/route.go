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

// routeSpec adds to the spec of a model's route what every route of the
// model carries: for a priced model, the cost of a request in credits,
// unless its prices are in a dry run, and while the gateway counts tokens,
// the token counts of each answer.
func routeSpec(s State, m Model, spec map[string]any) map[string]any {
	var costs []any
	if m.chargesBudgets() {
		costs = creditsCost(m)
	}
	if s.CountsTokens() {
		costs = append(costs, TokenCosts()...)
	}
	if len(costs) > 0 {
		spec["llmRequestCosts"] = costs
	}
	return spec
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
	spec := serviceBackendSpec(m.Slug)
	if h := backendHeaders(s, ""); h != nil {
		spec["headerMutation"] = h
	}
	u.Object["spec"] = spec
	return u
}

// backendHeaders is the header mutation of a backend this tool renders: the
// request class it sets, when objective is not empty, and the credentials it
// removes while the gateway takes tokens. Nil when there is neither.
//
// The headers the gateway wrote to tell tenants apart (x-aigw-tenant,
// x-aigw-key, x-aigw-client-id, x-aigw-budget, x-aigw-quota-<model>) stay,
// on a model server's backend too. A rate limit charges an answer's cost
// with a second rule that reads the request's headers again when the
// answer ends (Envoy Gateway's ApplyOnStreamDone), and a backend removes a
// header from those same request headers. A model server's backend that
// removed them had every answer counted as 1 under a tenant's quota and as
// nothing under its budget, while the pool, which reads no tenant, counted
// the cost (the lab, 2026-10-17, AI gateway 1.2.0 and Envoy Gateway 1.9.2).
// No rule reads the credentials.
func backendHeaders(s State, objective string) map[string]any {
	var h map[string]any
	if objective != "" {
		h = objectiveHeader(objective)
	}
	if s.AuthEnabled && s.Identity.tokens() {
		if h == nil {
			h = map[string]any{}
		}
		h["remove"] = credentialsRemoved()["remove"]
	}
	return h
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
	u.Object["spec"] = routeSpec(s, m, map[string]any{
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
	})
	return u
}
