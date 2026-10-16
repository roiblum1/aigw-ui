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
// unless its prices are in a dry run.
func routeSpec(m Model, spec map[string]any) map[string]any {
	if m.chargesBudgets() {
		spec["llmRequestCosts"] = creditsCost(m)
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
	if h := backendHeaders(s, "", true); h != nil {
		spec["headerMutation"] = h
	}
	u.Object["spec"] = spec
	return u
}

// backendHeaders is the header mutation of a backend this tool renders: the
// request class it sets, when objective is not empty, and the credentials it
// removes while the gateway takes tokens. Nil when there is neither.
//
// A backend that is a model server (modelServer) also loses the headers the
// gateway wrote to tell tenants apart: the model server has no use for them.
// A backend that forwards to another site's gateway keeps them, since a
// route there may count the request by its tenant too.
func backendHeaders(s State, objective string, modelServer bool) map[string]any {
	var h map[string]any
	if objective != "" {
		h = objectiveHeader(objective)
	}
	var remove []any
	if s.AuthEnabled && s.Identity.tokens() {
		remove = append(remove, credentialsRemoved()["remove"].([]any)...)
	}
	if s.AuthEnabled && modelServer {
		for _, name := range identityHeaders(s) {
			remove = append(remove, name)
		}
	}
	if len(remove) > 0 {
		if h == nil {
			h = map[string]any{}
		}
		h["remove"] = remove
	}
	return h
}

// identityHeaders are the headers the gateway writes from a key or a token:
// the tenant, the key, the client ID, the budget and the amount of each own
// quota.
func identityHeaders(s State) []string {
	names := []string{TenantHeader, KeyHeader, ClientIDHeader, BudgetHeader}
	for _, q := range QuotaClaimHeaders(s.Models) {
		names = append(names, q["header"].(string))
	}
	return names
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
	u.Object["spec"] = routeSpec(m, map[string]any{
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
