package kube

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/render"
)

// readGatewayStatus asks the cluster what the gateway's controller made of
// the AI gateway objects. Applying an object only proves the API server stored
// it; the controller can still refuse it. This is informational: a failure to
// read a status never fails the sync.
func (c *Client) readGatewayStatus(ctx context.Context, namespace string, desired []*unstructured.Unstructured, res *SyncResult) {
	if len(res.Changes) > 0 && c.settle > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.settle):
		}
	}
	changed := map[string]int{}
	for i, ch := range res.Changes {
		changed[ch.Kind+"/"+ch.Namespace+"/"+ch.Name] = i
	}
	var classes []*unstructured.Unstructured
	for _, obj := range desired {
		if obj.GetKind() == "EnvoyPatchPolicy" || obj.GetKind() == "BackendTrafficPolicy" {
			c.readPatchStatus(ctx, namespace, obj, changed, res)
			continue
		}
		if obj.GetKind() == "SecurityPolicy" {
			c.readSecurityStatus(ctx, namespace, obj, changed, res)
			continue
		}
		if obj.GetKind() == render.ObjectiveKind {
			classes = append(classes, obj)
			continue
		}
		if obj.GroupVersionKind().Group != "aigateway.envoyproxy.io" {
			continue
		}
		gvr, ok := gvrFor(obj.GetKind())
		if !ok {
			continue
		}
		ns := obj.GetNamespace()
		if ns == "" {
			ns = namespace
		}
		live, err := c.dyn.Resource(gvr).Namespace(ns).Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(live.Object, "status", "conditions")
		if len(conditions) == 0 {
			continue
		}
		cond, _ := conditions[0].(map[string]any)
		kind, _ := cond["type"].(string)
		message, _ := cond["message"].(string)
		if i, ok := changed[obj.GetKind()+"/"+ns+"/"+obj.GetName()]; ok {
			res.Changes[i].Gateway, res.Changes[i].GatewayMessage = kind, message
		}
		if kind == "NotAccepted" {
			res.Rejected = append(res.Rejected, Change{Kind: obj.GetKind(), Namespace: ns, Name: obj.GetName(), Gateway: kind, GatewayMessage: message})
		}
	}
	c.readClasses(ctx, classes, res)
}

// readPatchStatus reports an EnvoyPatchPolicy or BackendTrafficPolicy the
// gateway did not put into effect. For a patch that happens when patches
// are not enabled in the Envoy Gateway configuration, or when a patch finds
// nothing to change; for the tenants' budget policy when global rate
// limiting is not enabled there, or when a policy on a route replaces it
// for that route (Overridden: the route's requests are not charged). Envoy Gateway carries on without the
// policy, so nothing else is affected, but what the policy was for is
// missing.
func (c *Client) readPatchStatus(ctx context.Context, namespace string, obj *unstructured.Unstructured, changed map[string]int, res *SyncResult) {
	gvr, _ := gvrFor(obj.GetKind())
	ns := obj.GetNamespace()
	if ns == "" {
		ns = namespace
	}
	live, err := c.dyn.Resource(gvr).Namespace(ns).Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		return
	}
	// A patch is in effect once Programmed; a traffic policy reports
	// Accepted and nothing more.
	want := "Programmed"
	if obj.GetKind() == "BackendTrafficPolicy" {
		want = "Accepted"
	}
	verdict, message := patchVerdict(live, want)
	if verdict == "" {
		return
	}
	if verdict == "NotProgrammed" && otherSchemeOnly(obj, message) {
		verdict, message = want, ""
	}
	if i, ok := changed[obj.GetKind()+"/"+ns+"/"+obj.GetName()]; ok {
		res.Changes[i].Gateway, res.Changes[i].GatewayMessage = verdict, message
	}
	if verdict != want {
		res.Rejected = append(res.Rejected, Change{Kind: obj.GetKind(), Namespace: ns, Name: obj.GetName(), Gateway: verdict, GatewayMessage: message})
	}
}

// readSecurityStatus reports the hub's SecurityPolicy when the gateway does
// not accept it, and warns when a SecurityPolicy on some route replaces it
// there (Overridden): requests on that route are not checked for a tenant's
// key or token, carry no tenant header, and so are charged to no budget or
// quota of a tenant. The route is the site's; whether it should be on the
// client listener at all is for its owner to say, so this does not fail the
// sync.
//
// A route that forwards nothing is left out of the warning: it reaches no
// model, so nothing is served there that a tenant should be charged for.
// The gateway chart's health check is such a route on the client listener,
// where a site turns it on for its global load balancer (health.enabled),
// and would otherwise be warned about on every sync.
func (c *Client) readSecurityStatus(ctx context.Context, namespace string, obj *unstructured.Unstructured, changed map[string]int, res *SyncResult) {
	gvr, _ := gvrFor(obj.GetKind())
	ns := obj.GetNamespace()
	if ns == "" {
		ns = namespace
	}
	live, err := c.dyn.Resource(gvr).Namespace(ns).Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		return
	}
	verdict, message := patchVerdict(live, "Accepted")
	if verdict == "" {
		return
	}
	if verdict == "Overridden" {
		if message = c.withoutDeadEnds(ctx, message); message == "" {
			verdict = "Accepted"
		}
	}
	if i, ok := changed[obj.GetKind()+"/"+ns+"/"+obj.GetName()]; ok {
		res.Changes[i].Gateway, res.Changes[i].GatewayMessage = verdict, message
	}
	switch verdict {
	case "Overridden":
		res.Warnings = append(res.Warnings, "SecurityPolicy "+ns+"/"+obj.GetName()+" does not hold on some routes of the client listener: "+message+
			" Requests there are not checked for a tenant's key or token and are charged to no tenant's budget or quota.")
	case "Accepted":
	default:
		res.Rejected = append(res.Rejected, Change{Kind: obj.GetKind(), Namespace: ns, Name: obj.GetName(), Gateway: verdict, GatewayMessage: message})
	}
}

// readClasses checks that the hub's best-effort classes rank below regular
// tenants' requests where they are served: the order of the classes
// (readClassOrder), and flow control in the pools' schedulers (readFlowControl).
func (c *Client) readClasses(ctx context.Context, classes []*unstructured.Unstructured, res *SyncResult) {
	if len(classes) == 0 {
		return
	}
	for _, obj := range classes {
		c.readClassOrder(ctx, obj, res)
	}
	svcs, err := c.llmServices(ctx)
	if err != nil {
		return
	}
	bases := c.baseConfigs(ctx)
	for _, obj := range classes {
		readFlowControl(obj, svcs, bases, res)
	}
}

// readFlowControl warns when an LLMInferenceService serves the pool of a
// best-effort class with a scheduler that runs no flow control. Without it
// the scheduler holds a best-effort request back only while it counts the
// pool full (llm-d router v0.10.0, requestcontrol/admission.go), which its
// default detector did not do in a lab test with 28 requests waiting in vLLM
// (docs/optimization.md): regular tenants' requests then wait in vLLM behind
// best-effort ones. Nothing is said when it cannot be told.
func readFlowControl(obj *unstructured.Unstructured, svcs []unstructured.Unstructured, bases baseConfigs, res *SyncResult) {
	group, _, _ := unstructured.NestedString(obj.Object, "spec", "poolRef", "group")
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "poolRef", "name")
	for i := range svcs {
		svc := &svcs[i]
		if p, ok := pool(svc); !ok || p.Namespace != obj.GetNamespace() || p.Name != name || p.Group != group {
			continue
		}
		configs, ok := bases.of(svc)
		if !ok {
			continue
		}
		if on, known := flowControl(svc, configs); known && !on {
			res.Warnings = append(res.Warnings, "Best-effort requests are not held back on InferencePool "+obj.GetNamespace()+"/"+name+
				" until its scheduler counts the pool full, which it may not: LLMInferenceService "+svc.GetNamespace()+"/"+svc.GetName()+
				" runs it without flow control. Turn on featureGates: [flowControl] with the concurrency-detector (see the optimization guide).")
		}
	}
}

// flowControl reports whether an LLMInferenceService's scheduler runs flow
// control: the feature gate of the scheduler config that applies, the
// service's own or else that of the last config it is built on that has one,
// as KServe applies them in order and the service last. None at all is the
// scheduler's default, without flow control (KServe 0.21's preset sets
// none). known is false when that config is in a ConfigMap, is not an
// object, or a config could not be read.
func flowControl(svc *unstructured.Unstructured, bases []Base) (on, known bool) {
	own, _, _ := unstructured.NestedMap(svc.Object, "spec")
	specs := []map[string]any{own}
	for i := len(bases) - 1; i >= 0; i-- {
		specs = append(specs, bases[i].Spec)
	}
	for _, spec := range specs {
		if spec == nil {
			return false, false
		}
		config, ok, _ := unstructured.NestedMap(spec, "router", "scheduler", "config")
		if !ok {
			continue
		}
		inline, ok := config["inline"].(map[string]any)
		if !ok {
			return false, false
		}
		gates, _, _ := unstructured.NestedStringSlice(inline, "featureGates")
		return slices.Contains(gates, "flowControl"), true
	}
	return false, true
}

// readClassOrder warns when the site's own request class "standard" for the
// pool of the hub's "best-effort" does not rank above it. Regular tenants'
// requests name "standard": its scheduler then serves them with or after
// best-effort ones, and below 0 refuses them as it does best-effort ones when
// the pool is full (llm-d router v0.10.0, requestcontrol/admission.go). A
// "standard" for another pool, or none, is priority 0 to this pool's
// scheduler, and the hub's best-effort priority is below 0.
func (c *Client) readClassOrder(ctx context.Context, obj *unstructured.Unstructured, res *SyncResult) {
	gvr, _ := gvrFor(obj.GetKind())
	live, err := c.dyn.Resource(gvr).Namespace(obj.GetNamespace()).Get(ctx, render.ObjectiveStandard, metav1.GetOptions{})
	if err != nil {
		return
	}
	pool := func(u *unstructured.Unstructured) string {
		group, _, _ := unstructured.NestedString(u.Object, "spec", "poolRef", "group")
		name, _, _ := unstructured.NestedString(u.Object, "spec", "poolRef", "name")
		return group + "/" + name
	}
	if pool(live) != pool(obj) {
		return
	}
	standard, _, _ := unstructured.NestedInt64(live.Object, "spec", "priority")
	bestEffort, _, _ := unstructured.NestedInt64(obj.Object, "spec", "priority")
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "poolRef", "name")
	where := "The request class standard of InferencePool " + obj.GetNamespace() + "/" + name + " has priority " + strconv.FormatInt(standard, 10)
	switch {
	case standard <= bestEffort:
		res.Warnings = append(res.Warnings, where+", not above best-effort's "+strconv.FormatInt(bestEffort, 10)+
			": regular tenants' requests are not served before best-effort ones there.")
	case standard < 0:
		res.Warnings = append(res.Warnings, where+
			", below 0: when the pool is full, its scheduler refuses regular tenants' requests as it does best-effort ones.")
	}
}

// overriddenPrefix starts Envoy Gateway's report that other SecurityPolicies
// replace this one somewhere (1.9.2, internal/gatewayapi/securitypolicy.go).
const overriddenPrefix = "This policy is being overridden by other securityPolicies for "

// withoutDeadEnds returns an Overridden message without the routes that
// forward nothing, and "" when no route is left. A message that names
// anything but whole routes, or a route the hub may not read, is kept as
// it is: what the hub cannot see is warned about.
func (c *Client) withoutDeadEnds(ctx context.Context, message string) string {
	routes, ok := overridingRoutes(message)
	if !ok {
		return message
	}
	var left []string
	for _, r := range routes {
		if !c.forwardsNothing(ctx, r) {
			left = append(left, r)
		}
	}
	if len(left) == 0 {
		return ""
	}
	return overriddenPrefix + "these routes: [" + strings.Join(left, " ") + "]"
}

// overridingRoutes returns the routes ("<namespace>/<name>") an Overridden
// message names, when it names whole routes only. Envoy Gateway lists them
// as "these routes: [a b]" and listeners or route rules in sections of their
// own (formatPolicyScopes, internal/gatewayapi/helpers.go); those, or other
// wording, give false.
func overridingRoutes(message string) ([]string, bool) {
	list, ok := strings.CutPrefix(message, overriddenPrefix+"these routes: [")
	if !ok {
		return nil, false
	}
	list, ok = strings.CutSuffix(list, "]")
	if !ok || strings.ContainsAny(list, "[]") {
		return nil, false
	}
	routes := strings.Fields(list)
	for _, r := range routes {
		if strings.Count(r, "/") != 1 {
			return nil, false
		}
	}
	return routes, len(routes) > 0
}

// forwardsNothing reports whether the HTTPRoute "<namespace>/<name>" sends
// no request anywhere: none of its rules has a backend or mirrors to one.
// The gateway chart's health check answers /healthz/gateway itself, with a
// filter and no backend. A route the hub may not read, or that is not an
// HTTPRoute, gives false.
func (c *Client) forwardsNothing(ctx context.Context, route string) bool {
	ns, name, _ := strings.Cut(route, "/")
	live, err := c.dyn.Resource(httpRouteGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	rules, _, _ := unstructured.NestedSlice(live.Object, "spec", "rules")
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		if refs, _ := rule["backendRefs"].([]any); len(refs) > 0 {
			return false
		}
		filters, _ := rule["filters"].([]any)
		for _, f := range filters {
			if filter, _ := f.(map[string]any); filter["type"] == "RequestMirror" {
				return false
			}
		}
	}
	return true
}

// otherSchemeOnly reports whether a retry patch written for both names of
// the route configuration (render.RouteConfigsAnnotation) is reported as not
// programmed only because the configuration is missing under the name the
// gateway does not use. Envoy Gateway then still applied the patches for the
// other name. When every name is missing, the route configuration itself is.
//
// Envoy Gateway writes that report over any other error of the same policy,
// so an error in the patches of the name that exists is not seen here; the
// self-test's stickiness step, which then gets no answer, sees it.
func otherSchemeOnly(desired *unstructured.Unstructured, message string) bool {
	names := strings.Split(desired.GetAnnotations()[render.RouteConfigsAnnotation], ",")
	const prefix = "Unable to find xds resources: "
	if len(names) < 2 || !strings.HasPrefix(message, prefix) {
		return false
	}
	missing := map[string]bool{}
	for _, r := range strings.Split(strings.TrimPrefix(message, prefix), ",") {
		name, ok := strings.CutPrefix(strings.TrimSpace(r), render.RouteConfigType+"/")
		if !ok || !slices.Contains(names, name) {
			return false
		}
		missing[name] = true
	}
	return len(missing) < len(names)
}

// patchVerdict returns want ("Programmed" or "Accepted") when every gateway
// the policy is for reports that condition true, and otherwise the first
// condition that says why not, with its message. Overridden says that a
// policy on some route replaces this one there; Envoy Gateway still reports
// the policy Accepted. It returns "" while the controller has reported
// nothing.
func patchVerdict(policy *unstructured.Unstructured, want string) (string, string) {
	ancestors, _, _ := unstructured.NestedSlice(policy.Object, "status", "ancestors")
	found := false
	for _, a := range ancestors {
		ancestor, _ := a.(map[string]any)
		conditions, _, _ := unstructured.NestedSlice(ancestor, "conditions")
		for _, cnd := range conditions {
			cond, _ := cnd.(map[string]any)
			kind, _ := cond["type"].(string)
			status, _ := cond["status"].(string)
			message, _ := cond["message"].(string)
			switch {
			case (kind == "Accepted" || kind == "Programmed") && status != "True":
				return "Not" + kind, message
			case kind == "Overridden" && status == "True":
				return kind, message
			case kind == want:
				found = true
			}
		}
	}
	if found {
		return want, ""
	}
	return "", ""
}
