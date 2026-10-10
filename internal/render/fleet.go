package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// FleetConfig is what every site's entry route has in common. It is the
// same for every cluster.
type FleetConfig struct {
	// PeerSNI is the server name of every site's listener for requests
	// forwarded by other sites. It is also the Host of the health check.
	PeerSNI string
	// CAConfigMap holds the CA that signed the sites' peer certificates, and
	// ClientSecret the certificate a gateway presents to another site. Both
	// are in the gateway namespace.
	CAConfigMap  string
	ClientSecret string
	// SessionHeaders carry the conversation key, one name per kind of
	// client. Requests with the same value go to the same site. A request
	// is hashed on the ones it has.
	SessionHeaders []string
}

// FleetSite is one site that serves a model, as an entry gateway reaches it.
type FleetSite struct {
	// Name is the cluster's name. It is the zone of the site.
	Name string
	Host string
	Port int
	// Weight is the site's zone weight. It is never below 1: Envoy rejects a
	// locality weight of 0, and Envoy Gateway then stops publishing every
	// change to the gateway.
	Weight int64
}

const (
	// fleetRequestTimeout replaces the gateway's default of 60s, which is
	// too short for a long generation.
	fleetRequestTimeout = "3600s"
	// fleetBreakerLimit is as good as no limit: each site's own shed limit
	// decides how much it takes.
	fleetBreakerLimit int64 = 100000
	// fleetHostAttempts is how many times the gateway picks a site for one
	// try of a request before it takes a site that already failed. Envoy
	// Gateway sets 5, and with a hash that is too few: every pick lands by
	// the same weights, so with one site holding 800 of 1100 all five land
	// on it for one conversation in seven, and those get that site's 503
	// however many retries are allowed. 20 leaves none.
	fleetHostAttempts int64 = 20
)

// RetryPatchName is the name of the EnvoyPatchPolicy that sets
// fleetHostAttempts on one model's entry route.
func RetryPatchName(slug string) string { return FleetName(slug) + "-retry" }

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

// healthPath is where a site answers for one model. The slashes of a name
// such as "zai-org/GLM-5.3" stay: they are part of the path. Anything else
// that is not allowed in a path is escaped.
func healthPath(model string) string {
	segments := strings.Split(model, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return "/healthz/" + strings.Join(segments, "/")
}

// FleetName is the name of a model's entry objects.
func FleetName(slug string) string { return "fleet-" + slug }

func (m Model) fleetSites() []FleetSite {
	sites := append([]FleetSite(nil), m.Fleet...)
	// Every gateway builds its hash table from this list, so the order has
	// to be the same on every cluster.
	sort.Slice(sites, func(i, j int) bool { return sites[i].Name < sites[j].Name })
	return sites
}

// fleetObjects returns a model's entry route: the objects that let any
// site's gateway take a request for the model and send it to a site that
// serves it.
func fleetObjects(s State, m Model) []*unstructured.Unstructured {
	name := FleetName(m.Slug)
	out := []*unstructured.Unstructured{
		fleetBackend(s, m), fleetServiceBackend(s, m, name, ""), fleetRoute(s, m),
		fleetTrafficPolicy(s, m, name, 503), fleetRetryPatch(s, name),
	}
	if !m.BestEffort {
		return out
	}
	name = BestEffortName(m.Slug)
	out = append(out, fleetServiceBackend(s, m, name, ObjectiveBestEffort))
	if tenants := m.OverageTenants(); len(tenants) > 0 {
		// A 429 from a serving site says it has no room for best-effort
		// work right now, so the next site gets a chance.
		out = append(out, fleetOverageRoute(s, m, tenants), fleetTrafficPolicy(s, m, name, 503, 429), fleetRetryPatch(s, name))
	}
	return out
}

// fleetBackend lists the sites that serve the model, one zone each. The
// traffic policy's zone weights list exactly the same sites: a zone that has
// an endpoint here and no weight there gets a weight of 1 from the gateway.
func fleetBackend(s State, m Model) *unstructured.Unstructured {
	sites := m.fleetSites()
	endpoints := make([]any, 0, len(sites))
	for _, site := range sites {
		endpoints = append(endpoints, map[string]any{
			"fqdn": map[string]any{"hostname": site.Host, "port": int64(site.Port)},
			"zone": site.Name,
		})
	}
	u := object(egAPI, "Backend", s.Namespace, FleetName(m.Slug))
	u.Object["spec"] = map[string]any{
		"endpoints": endpoints,
		"tls": map[string]any{
			"sni":                  s.Fleet.PeerSNI,
			"caCertificateRefs":    []any{map[string]any{"group": "", "kind": "ConfigMap", "name": s.Fleet.CAConfigMap}},
			"clientCertificateRef": map[string]any{"name": s.Fleet.ClientSecret},
		},
	}
	return u
}

// fleetServiceBackend returns an AIServiceBackend for the model's sites.
// objective, when not empty, is the class every request through it gets.
func fleetServiceBackend(s State, m Model, name, objective string) *unstructured.Unstructured {
	u := object(aigwAPI, "AIServiceBackend", s.Namespace, name)
	spec := map[string]any{
		"schema": map[string]any{"name": "OpenAI"},
		"backendRef": map[string]any{
			"group": "gateway.envoyproxy.io",
			"kind":  "Backend",
			"name":  FleetName(m.Slug),
		},
	}
	if objective != "" {
		spec["headerMutation"] = objectiveHeader(objective)
	}
	u.Object["spec"] = spec
	return u
}

// objectiveHeader sets the class of a request, replacing one a client sent.
func objectiveHeader(objective string) map[string]any {
	return map[string]any{"set": []any{map[string]any{"name": ObjectiveHeader, "value": objective}}}
}

func fleetParent(s State) map[string]any {
	parent := map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": s.GatewayName}
	if s.ClientListener != "" {
		parent["sectionName"] = s.ClientListener
	}
	return parent
}

func fleetRoute(s State, m Model) *unstructured.Unstructured {
	// QuotaPolicy matches on modelNameOverride, so it is always set.
	backendRef := map[string]any{"name": FleetName(m.Slug), "modelNameOverride": m.Name}
	if m.BestEffort {
		// The class is then always explicit: a tenant within its budget
		// cannot be sent as another class by a header of its own.
		backendRef["headerMutation"] = objectiveHeader(ObjectiveStandard)
	}
	u := modelRoute(s.Namespace, FleetName(m.Slug), m)
	u.Object["spec"] = map[string]any{
		"parentRefs": []any{fleetParent(s)},
		"rules": []any{
			map[string]any{
				"name": "fleet",
				"matches": []any{
					map[string]any{"headers": []any{
						map[string]any{"type": "Exact", "name": ModelHeader, "value": m.Name},
					}},
				},
				"backendRefs": []any{backendRef},
				"timeouts":    map[string]any{"request": fleetRequestTimeout},
			},
		},
	}
	return u
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

// fleetTrafficPolicy attaches to the HTTPRoute the gateway generates from
// the AIGatewayRoute called name, which has the same name. A request that
// gets one of the retryOn statuses goes to the next site.
func fleetTrafficPolicy(s State, m Model, name string, retryOn ...int64) *unstructured.Unstructured {
	sites := m.fleetSites()
	zones := make([]any, 0, len(sites))
	for _, site := range sites {
		zones = append(zones, map[string]any{"zone": site.Name, "weight": max(site.Weight, 1)})
	}
	statuses := make([]any, 0, len(retryOn))
	for _, code := range retryOn {
		statuses = append(statuses, code)
	}
	u := object(egAPI, "BackendTrafficPolicy", s.Namespace, name)
	loadBalancer := map[string]any{
		"type": "ConsistentHash",
		"consistentHash": map[string]any{
			"type": "Headers",
			// Only the conversation key. Hashing the tenant or the key
			// too would pin a customer to one site.
			"headers": sessionHeaders(s.Fleet),
		},
	}
	// With one site there is nothing to weigh, and the weights have a
	// price: with them Envoy Gateway names the route's upstream per
	// backend, and Envoy AI Gateway up to 1.2.0 then attaches no quota to
	// the route. So a model with a single site keeps its quotas.
	if len(sites) > 1 {
		loadBalancer["zoneAware"] = map[string]any{"weightedZones": zones}
	}
	u.Object["spec"] = map[string]any{
		"targetRefs": []any{
			map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": name},
		},
		"loadBalancer": loadBalancer,
		// A site over its limit answers 503 at once; the request then goes
		// to the next site.
		"retry": map[string]any{
			"numRetries": int64(len(sites) - 1),
			"retryOn": map[string]any{
				"triggers":        []any{"connect-failure", "reset", "retriable-status-codes"},
				"httpStatusCodes": statuses,
			},
			"perRetry": map[string]any{
				"backOff": map[string]any{"baseInterval": "100ms", "maxInterval": "1s"},
			},
		},
		"healthCheck": map[string]any{
			// 0 means: never send to a site that fails its check, however
			// many sites fail. Do not remove.
			"panicThreshold": int64(0),
			"active": map[string]any{
				"type":     "HTTP",
				"interval": "5s",
				// The model's own health answer can be slow under load.
				"timeout":            "5s",
				"unhealthyThreshold": int64(3),
				"healthyThreshold":   int64(2),
				"http": map[string]any{
					"hostname":         s.Fleet.PeerSNI,
					"path":             healthPath(m.Name),
					"expectedStatuses": []any{int64(200)},
				},
			},
		},
		"circuitBreaker": map[string]any{
			"maxConnections":      fleetBreakerLimit,
			"maxPendingRequests":  fleetBreakerLimit,
			"maxParallelRequests": fleetBreakerLimit,
		},
		"timeout": map[string]any{
			"http": map[string]any{"requestTimeout": fleetRequestTimeout},
		},
	}
	return u
}

func sessionHeaders(f FleetConfig) []any {
	out := make([]any, 0, len(f.SessionHeaders))
	for _, name := range f.SessionHeaders {
		out = append(out, map[string]any{"name": name})
	}
	return out
}

// fleetRetryPatch changes two things in the route Envoy Gateway generates
// for the entry route called name, which have no setting of their own. It needs
// enableEnvoyPatchPolicy in the Envoy Gateway configuration; without it the
// policy is not programmed and a sync reports it.
//
//   - The gateway picks from fleetHostAttempts sites on a retry, not 5.
//   - A forwarded request carries the peer server name as its Host. Envoy
//     Gateway sets the Host to the site's peer host instead. A peer listener
//     answers for the peer server name, so it would return 404 to every
//     forwarded request, while the health check, which sets the name itself,
//     keeps passing.
//
// Each route has a policy of its own: Envoy Gateway applies the patches of
// one policy together, so in a shared policy one route that is not there
// yet would take the settings away from every other.
func fleetRetryPatch(s State, name string) *unstructured.Unstructured {
	// The route Envoy Gateway generates from the first rule of the
	// AIGatewayRoute, which is its only one.
	route := "..routes[?(@.name == 'httproute/" + s.Namespace + "/" + name + "/rule/0/match/0/*')].route"
	patch := func(op, jsonPath, path string, value any) map[string]any {
		operation := map[string]any{"op": op, "jsonPath": jsonPath, "path": path}
		if value != nil {
			operation["value"] = value
		}
		return map[string]any{
			"type": "type.googleapis.com/envoy.config.route.v3.RouteConfiguration",
			// The route configuration of the listener the entry route is on.
			"name":      s.Namespace + "/" + s.GatewayName + "/" + s.ClientListener,
			"operation": operation,
		}
	}
	u := object(egAPI, "EnvoyPatchPolicy", s.Namespace, name+"-retry")
	u.Object["spec"] = map[string]any{
		"targetRef": map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": s.GatewayName},
		"type":      "JSONPatch",
		"jsonPatches": []any{
			// "add" also replaces a value that is there, so the patch still
			// works if Envoy Gateway stops setting its own.
			patch("add", route+".retry_policy", "host_selection_retry_max_attempts", fleetHostAttempts),
			// Only one way of setting the Host is allowed on a route, so the
			// automatic one goes first. Setting it to false does not remove
			// it for Envoy.
			patch("remove", route, "auto_host_rewrite", nil),
			patch("add", route, "host_rewrite_literal", s.Fleet.PeerSNI),
		},
	}
	return u
}

// HeldNames returns the names of the objects a sync must not remove: the
// entry objects of every held model. All of them are in the gateway
// namespace, and no other object of this tool has such a name.
func HeldNames(s State) map[string]bool {
	names := map[string]bool{}
	for _, m := range s.Models {
		if m.Held() {
			names[FleetName(m.Slug)] = true
			names[RetryPatchName(m.Slug)] = true
			names[BestEffortName(m.Slug)] = true
			names[BestEffortName(m.Slug)+"-retry"] = true
		}
	}
	return names
}

// FleetRevision identifies everything that must be the same on every fleet
// cluster: the sites and weights of each model and the shared settings. Two
// clusters with the same revision send a conversation to the same site. It
// is empty when the state renders no entry route.
func FleetRevision(s State) string {
	// The best-effort fields are left out when unset, so a fleet without
	// such a model keeps the revision it had before they existed.
	type model struct {
		Name       string
		Sites      []FleetSite
		BestEffort bool     `json:",omitempty"`
		Overage    []string `json:",omitempty"`
	}
	var models []model
	for _, m := range s.Models {
		if len(m.Fleet) > 0 {
			models = append(models, model{m.Name, m.fleetSites(), m.BestEffort, m.OverageTenants()})
		}
	}
	if len(models) == 0 {
		return ""
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	data, err := json.Marshal(struct {
		Namespace string
		Fleet     FleetConfig
		Models    []model
	}{s.Namespace, s.Fleet, models})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	// Six bytes are enough to tell two fleets apart. It is not a signature.
	return hex.EncodeToString(sum[:6])
}
