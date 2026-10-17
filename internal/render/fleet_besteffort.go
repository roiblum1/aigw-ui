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

	// FairnessHeader names the flow a serving site's scheduler shares a
	// class out by, once its flow control is on: the tenant, so one tenant
	// cannot crowd out the others of its class. The gateway writes it from
	// a token (jwtProvider). A request with a plain API key has none and
	// joins the scheduler's default flow.
	FairnessHeader = "x-llm-d-inference-fairness-id"

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
	u.Object["spec"] = routeSpec(s, m, map[string]any{
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
	})
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

// classPools returns, by namespace, the pool the request class
// "best-effort" there is for. A class is found by its name within a
// namespace, so a namespace has one. Where two pools of best-effort models
// share a namespace, the first by name gets it.
func classPools(s State) map[string]Pool {
	byNamespace := map[string]Pool{}
	for _, m := range s.Models {
		for _, p := range m.BestEffortPools {
			if have, ok := byNamespace[p.Namespace]; !ok || p.Name < have.Name {
				byNamespace[p.Namespace] = p
			}
		}
	}
	return byNamespace
}

// BestEffortGaps says, for each pool of a best-effort model that the class
// is not for, that best-effort requests rank there with everyone else's. A
// scheduler takes only the classes that name its own pool (llm-d router
// v0.10.0, inferenceobjective_reconciler.go), so on such a pool the class is
// unknown, a request naming it gets priority 0 as a regular tenant's does,
// and it is not the first refused when the pool is full.
func BestEffortGaps(s State) []string {
	byNamespace := classPools(s)
	seen := map[Pool]bool{}
	var gaps []string
	for _, m := range s.Models {
		for _, p := range m.BestEffortPools {
			if have := byNamespace[p.Namespace]; p == have || seen[p] {
				continue
			}
			seen[p] = true
			gaps = append(gaps, "Best-effort requests rank with regular tenants' on InferencePool "+p.Namespace+"/"+p.Name+
				": a namespace takes one request class best-effort, and in "+p.Namespace+" it is for "+byNamespace[p.Namespace].Name+
				". Give the pool a namespace of its own.")
		}
	}
	sort.Strings(gaps)
	return gaps
}

// objectives returns the request class "best-effort" for every pool that
// serves a model in best-effort mode on this cluster (classPools). The class
// is what the header of the best-effort route names; without it the serving
// site treats the request like any other.
func objectives(s State) []*unstructured.Unstructured {
	byNamespace := classPools(s)
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
