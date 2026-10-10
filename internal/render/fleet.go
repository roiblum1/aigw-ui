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
