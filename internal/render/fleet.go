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

	// RetryPatchName is the EnvoyPatchPolicy that sets fleetHostAttempts.
	RetryPatchName = "aigw-ui-fleet-retry"
)

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
	return []*unstructured.Unstructured{fleetBackend(s, m), fleetServiceBackend(s, m), fleetRoute(s, m), fleetTrafficPolicy(s, m)}
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

func fleetServiceBackend(s State, m Model) *unstructured.Unstructured {
	u := object(aigwAPI, "AIServiceBackend", s.Namespace, FleetName(m.Slug))
	u.Object["spec"] = map[string]any{
		"schema": map[string]any{"name": "OpenAI"},
		"backendRef": map[string]any{
			"group": "gateway.envoyproxy.io",
			"kind":  "Backend",
			"name":  FleetName(m.Slug),
		},
	}
	return u
}

func fleetRoute(s State, m Model) *unstructured.Unstructured {
	parent := map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": s.GatewayName}
	if s.ClientListener != "" {
		parent["sectionName"] = s.ClientListener
	}
	u := object(aigwAPI, "AIGatewayRoute", s.Namespace, FleetName(m.Slug))
	u.Object["spec"] = map[string]any{
		"parentRefs": []any{parent},
		"rules": []any{
			map[string]any{
				"name": "fleet",
				"matches": []any{
					map[string]any{"headers": []any{
						map[string]any{"type": "Exact", "name": ModelHeader, "value": m.Name},
					}},
				},
				"backendRefs": []any{
					// QuotaPolicy matches on modelNameOverride, so it is always set.
					map[string]any{"name": FleetName(m.Slug), "modelNameOverride": m.Name},
				},
				"timeouts": map[string]any{"request": fleetRequestTimeout},
			},
		},
	}
	return u
}

// fleetTrafficPolicy attaches to the HTTPRoute the gateway generates from
// the model's AIGatewayRoute, which has the same name.
func fleetTrafficPolicy(s State, m Model) *unstructured.Unstructured {
	sites := m.fleetSites()
	zones := make([]any, 0, len(sites))
	for _, site := range sites {
		zones = append(zones, map[string]any{"zone": site.Name, "weight": max(site.Weight, 1)})
	}
	u := object(egAPI, "BackendTrafficPolicy", s.Namespace, FleetName(m.Slug))
	u.Object["spec"] = map[string]any{
		"targetRefs": []any{
			map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": FleetName(m.Slug)},
		},
		"loadBalancer": map[string]any{
			"type": "ConsistentHash",
			"consistentHash": map[string]any{
				"type": "Headers",
				// Only the conversation key. Hashing the tenant or the key
				// too would pin a customer to one site.
				"headers": sessionHeaders(s.Fleet),
			},
			"zoneAware": map[string]any{"weightedZones": zones},
		},
		// A site over its limit answers 503 at once; the request then goes
		// to the next site.
		"retry": map[string]any{
			"numRetries": int64(len(sites) - 1),
			"retryOn": map[string]any{
				"triggers":        []any{"connect-failure", "reset", "retriable-status-codes"},
				"httpStatusCodes": []any{int64(503)},
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

// fleetRetryPatch raises the number of sites the gateway picks from on a
// retry, for the route of every model with an entry route. Envoy Gateway has
// no setting for it, so the generated route is patched. It needs
// enableEnvoyPatchPolicy in the Envoy Gateway configuration; without it the
// policy is not programmed and a sync reports it.
func fleetRetryPatch(s State, models []Model) *unstructured.Unstructured {
	patches := make([]any, 0, len(models))
	for _, m := range models {
		patches = append(patches, map[string]any{
			"type": "type.googleapis.com/envoy.config.route.v3.RouteConfiguration",
			// The route configuration of the listener the entry route is on.
			"name": s.Namespace + "/" + s.GatewayName + "/" + s.ClientListener,
			"operation": map[string]any{
				"op": "replace",
				// The route Envoy Gateway generates from the "fleet" rule,
				// the first of the model's AIGatewayRoute.
				"jsonPath": "..routes[?(@.name == 'httproute/" + s.Namespace + "/" + FleetName(m.Slug) + "/rule/0/match/0/*')].route.retry_policy",
				"path":     "host_selection_retry_max_attempts",
				"value":    fleetHostAttempts,
			},
		})
	}
	u := object(egAPI, "EnvoyPatchPolicy", s.Namespace, RetryPatchName)
	u.Object["spec"] = map[string]any{
		"targetRef":   map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": s.GatewayName},
		"type":        "JSONPatch",
		"jsonPatches": patches,
	}
	return u
}

// HeldNames returns the names of the objects a sync must not remove: the
// entry objects and the QuotaPolicy of every held model.
func HeldNames(s State) map[string]bool {
	names := map[string]bool{}
	for _, m := range s.Models {
		if m.Held() {
			names[FleetName(m.Slug)] = true
			names[m.Slug] = true
			// The patch is rendered from the models that are not held. It
			// stays while any is, or a held route would lose its part of it.
			names[RetryPatchName] = true
		}
	}
	return names
}

// FleetRevision identifies everything that must be the same on every fleet
// cluster: the sites and weights of each model and the shared settings. Two
// clusters with the same revision send a conversation to the same site. It
// is empty when the state renders no entry route.
func FleetRevision(s State) string {
	type model struct {
		Name  string
		Sites []FleetSite
	}
	var models []model
	for _, m := range s.Models {
		if len(m.Fleet) > 0 {
			models = append(models, model{m.Name, m.fleetSites()})
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
