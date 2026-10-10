package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// RetryPatchName is the name of the EnvoyPatchPolicy that sets
// fleetHostAttempts on one model's entry route.
func RetryPatchName(slug string) string { return FleetName(slug) + "-retry" }

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
