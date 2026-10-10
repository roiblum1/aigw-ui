package render

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// BestEffortName is the name of the objects of a model's best-effort route:
// the entry route for the tenants whose budget is spent. It differs from
// every FleetName in its prefix, so the best-effort objects of a model "x"
// cannot be the entry objects of a model "x-be".
func BestEffortName(slug string) string { return "fleetbe-" + slug }

const (
	// ObjectiveHeader names the class a serving site queues a request in.
	// The value is the name of an InferenceObjective in the model's
	// namespace there, which the model's release has to define.
	ObjectiveHeader     = "x-llm-d-inference-objective"
	ObjectiveStandard   = "standard"
	ObjectiveBestEffort = "best-effort"

	// MaxOverageTenants is how many tenants one best-effort route lists.
	// The list is one regular expression in the gateway's route table.
	MaxOverageTenants = 200
)

// OverageTenants returns the tenants the model's best-effort route lists:
// the first MaxOverageTenants by slug.
func (m Model) OverageTenants() []string {
	tenants := m.Overage
	if len(tenants) > MaxOverageTenants {
		tenants = tenants[:MaxOverageTenants]
	}
	tenants = append([]string(nil), tenants...)
	sort.Strings(tenants)
	return tenants
}

// objectiveHeader sets the class of a request, replacing one a client sent.
func objectiveHeader(objective string) map[string]any {
	return map[string]any{"set": []any{map[string]any{"name": ObjectiveHeader, "value": objective}}}
}

// fleetOverageRoute takes the requests of the listed tenants for the model.
// It matches two headers where the model's entry route matches one, which
// makes it the more specific route, so it wins for these tenants.
//
// The client ID is there to match on because the gateway checks the API key
// before the AI gateway reads the model from the body and matches again.
func fleetOverageRoute(s State, m Model, tenants []string) *unstructured.Unstructured {
	name := BestEffortName(m.Slug)
	u := modelRoute(s.Namespace, name, m)
	u.Object["spec"] = map[string]any{
		"parentRefs": []any{fleetParent(s)},
		"rules": []any{
			map[string]any{
				"name": "overage",
				"matches": []any{
					map[string]any{"headers": []any{
						map[string]any{"type": "Exact", "name": ModelHeader, "value": m.Name},
						map[string]any{"type": "RegularExpression", "name": ClientIDHeader, "value": TenantsClientIDPattern(tenants)},
					}},
				},
				"backendRefs": []any{
					map[string]any{"name": name, "modelNameOverride": m.Name},
				},
				"timeouts": map[string]any{"request": fleetRequestTimeout},
			},
		},
	}
	return u
}
