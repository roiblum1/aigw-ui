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

// ObjectiveKind is the kind of a request class of the serving stack.
const ObjectiveKind = "InferenceObjective"

const objectiveAPI = "llm-d.ai/v1alpha2"

// DefaultBestEffortPriority is the priority of the class "best-effort". A
// request without a class, or with a class the site does not know, has
// priority 0, and the scheduler drops requests below 0 first when the pool
// is full.
const DefaultBestEffortPriority = -1

// Pool names an InferencePool on the cluster that is rendered.
type Pool struct {
	Namespace string
	Name      string
	Group     string
}

// objectives returns the request class "best-effort" for every pool that
// serves a model in best-effort mode on this cluster. The class is what the
// header of the best-effort route names; without it the serving site treats
// the request like any other.
//
// A class is found by its name within a namespace, so a namespace has one.
// Where two pools of best-effort models share a namespace, the first by
// name gets it.
func objectives(s State) []*unstructured.Unstructured {
	byNamespace := map[string]Pool{}
	for _, m := range s.Models {
		for _, p := range m.BestEffortPools {
			if have, ok := byNamespace[p.Namespace]; !ok || p.Name < have.Name {
				byNamespace[p.Namespace] = p
			}
		}
	}
	namespaces := make([]string, 0, len(byNamespace))
	for ns := range byNamespace {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	priority := s.Fleet.BestEffortPriority
	if priority == 0 {
		priority = DefaultBestEffortPriority
	}
	out := make([]*unstructured.Unstructured, 0, len(namespaces))
	for _, ns := range namespaces {
		p := byNamespace[ns]
		u := object(objectiveAPI, ObjectiveKind, ns, ObjectiveBestEffort)
		u.Object["spec"] = map[string]any{
			"priority": priority,
			"poolRef":  map[string]any{"group": p.Group, "kind": "InferencePool", "name": p.Name},
		}
		out = append(out, u)
	}
	return out
}
