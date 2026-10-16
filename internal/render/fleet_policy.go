package render

import (
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// RetryPatchName is the name of the EnvoyPatchPolicy that sets
// fleetHostAttempts on one model's entry route.
func RetryPatchName(slug string) string { return FleetName(slug) + "-retry" }

// fleetTrafficPolicy attaches to the HTTPRoute the gateway generates from
// the AIGatewayRoute called name, which has the same name. A request that
// gets one of the retryOn statuses goes to the next site.
//
// It is merged into the policy on the listener, the tenants' budgets. A
// policy on a route otherwise replaces the listener's for that route, and
// Envoy Gateway reports the listener's as Overridden: every request through
// an entry route went uncharged by the budgets (lab, 2026-10-16). With no
// policy on the listener the merge changes nothing.
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
		"mergeType":    "StrategicMerge",
		"loadBalancer": loadBalancer,
		// A site over its limit answers 503 at once; the request then goes
		// to the next site. So does a 500: a site whose scheduler (the
		// endpoint picker) cannot be reached answers it to every request,
		// because Envoy AI Gateway up to 1.2.0 sets failure_mode_allow to
		// false on the pool's ext_proc filter and ignores the pool's
		// FailOpen. A 500 from the model itself is tried elsewhere too,
		// which costs at most one more try per other site.
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
			// 1 means: a site that fails its check is not used while any
			// other site passes, and when every site fails the request is
			// sent anyway. With 0 a model with one site answered 503 to
			// everyone when its check failed three times in a row for a
			// reason that was not the model. Do not raise: at 50 two failed
			// sites of three would get two thirds of the traffic.
			"panicThreshold": int64(1),
			"active": map[string]any{
				"type": "HTTP",
				// Not 5s: vLLM closes an idle connection after 5 seconds, so
				// a check every 5 seconds met the closing connection again
				// and again and counted as failed.
				"interval": fleetHealthInterval,
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
//
// The route configuration has two names. Envoy Gateway names it after the
// listener ("<namespace>/<gateway>/<listener>") and, with its runtime flag
// XDSNameSchemeV2, after the port of a plain-HTTP listener ("http-<port>");
// the flag is off up to 1.9 and is to be on by default from 1.10. With the
// old name only, every patch would find nothing on such a gateway, and every
// forwarded request would get a 404 for the lost Host. So the patches are
// written for both names when the port is known: Envoy Gateway applies those
// whose configuration exists and skips the others, which it reports as not
// found (RouteConfigsAnnotation lets the sync tell that apart).
func fleetRetryPatch(s State, name string) *unstructured.Unstructured {
	// The route Envoy Gateway generates from the first rule of the
	// AIGatewayRoute, which is its only one.
	route := "..routes[?(@.name == 'httproute/" + s.Namespace + "/" + name + "/rule/0/match/0/*')].route"
	configs := RouteConfigNames(s)
	var patches []any
	for _, config := range configs {
		patch := func(op, jsonPath, path string, value any) map[string]any {
			operation := map[string]any{"op": op, "jsonPath": jsonPath, "path": path}
			if value != nil {
				operation["value"] = value
			}
			return map[string]any{"type": RouteConfigType, "name": config, "operation": operation}
		}
		patches = append(patches,
			// "add" also replaces a value that is there, so the patch still
			// works if Envoy Gateway stops setting its own.
			patch("add", route+".retry_policy", "host_selection_retry_max_attempts", fleetHostAttempts),
			// Only one way of setting the Host is allowed on a route, so the
			// automatic one goes first. Setting it to false does not remove
			// it for Envoy.
			patch("remove", route, "auto_host_rewrite", nil),
			patch("add", route, "host_rewrite_literal", s.Fleet.PeerSNI),
		)
	}
	u := object(egAPI, "EnvoyPatchPolicy", s.Namespace, name+"-retry")
	if len(configs) > 1 {
		u.SetAnnotations(map[string]string{RouteConfigsAnnotation: strings.Join(configs, ",")})
	}
	u.Object["spec"] = map[string]any{
		"targetRef":   map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": s.GatewayName},
		"type":        "JSONPatch",
		"jsonPatches": patches,
	}
	return u
}

const (
	// RouteConfigType is the xDS type the retry patches change.
	RouteConfigType = "type.googleapis.com/envoy.config.route.v3.RouteConfiguration"
	// RouteConfigsAnnotation lists, on a retry patch written for both names
	// of the route configuration, those names: only one of them exists on
	// a gateway, and Envoy Gateway reports the others as not found.
	RouteConfigsAnnotation = "aigw-ui.io/route-configurations"
)

// RouteConfigNames returns the names the client listener's route
// configuration has under Envoy Gateway's xDS name schemes: the listener's
// own, and with a known plain-HTTP port the one after the port.
func RouteConfigNames(s State) []string {
	names := []string{s.Namespace + "/" + s.GatewayName + "/" + s.ClientListener}
	if s.ClientPort > 0 {
		names = append(names, "http-"+strconv.FormatInt(s.ClientPort, 10))
	}
	return names
}
