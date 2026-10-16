package kube

import (
	"context"
	"slices"
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
	for _, obj := range desired {
		if obj.GetKind() == "EnvoyPatchPolicy" || obj.GetKind() == "BackendTrafficPolicy" {
			c.readPatchStatus(ctx, namespace, obj, changed, res)
			continue
		}
		if obj.GetKind() == "SecurityPolicy" {
			c.readSecurityStatus(ctx, namespace, obj, changed, res)
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
