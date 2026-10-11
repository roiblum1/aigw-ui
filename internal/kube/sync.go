package kube

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"aigw-ui/internal/render"
)

type SyncResult struct {
	Applied int `json:"applied"`
	Pruned  int `json:"pruned"`
	// Changes lists the objects this sync created, updated or deleted, as the
	// cluster's API server confirmed them. Objects that were already as
	// desired are counted in Applied but not listed.
	Changes []Change `json:"changes"`
	// Rejected lists objects the gateway's controller reports as not accepted.
	Rejected []Change `json:"rejected"`
	// Held says which models' objects were left as they are, and why.
	Held []string `json:"held,omitempty"`
	// Skipped lists the objects that were not written because the cluster
	// does not have their kind, as "<kind> <namespace>/<name>".
	Skipped []string `json:"skipped,omitempty"`
}

// Change is one object on the cluster and what happened to it.
type Change struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Action is "created", "updated" or "deleted".
	Action string `json:"action,omitempty"`
	// Gateway is the condition the gateway's controller set on the object,
	// such as "Accepted", with its message. Empty when it has set none.
	Gateway        string `json:"gateway,omitempty"`
	GatewayMessage string `json:"gateway_message,omitempty"`
}

// version identifies the content of an object: the generation, which only
// moves when the spec changes, or the resource version for kinds without one.
func version(obj *unstructured.Unstructured) string {
	if obj == nil {
		return ""
	}
	if g := obj.GetGeneration(); g != 0 {
		return fmt.Sprintf("g%d", g)
	}
	return obj.GetResourceVersion()
}

// Sync makes the managed objects match desired. Everything lives in namespace
// except the kinds in anyNamespace, which sit next to what they belong to.
//
// An object of a kind the cluster does not have fails the sync, unless the
// kind is optional: then it is left out and listed in Skipped.
//
// held names objects of this tool that are not in desired and must stay as
// they are all the same.
func (c *Client) Sync(ctx context.Context, namespace string, desired []*unstructured.Unstructured, held map[string]bool) (SyncResult, error) {
	var res SyncResult
	// kept holds "<kind>/<namespace>/<name>" of every desired object, so the
	// prune step knows what to leave alone.
	kept := map[string]bool{}
	// elsewhere holds the namespaces the anyNamespace kinds are in now.
	elsewhere := map[string]bool{namespace: true}

	// Before any policy is written: the gateway's controller looks at a
	// route when its policy changes, and the mark has to be there by then.
	for _, obj := range desired {
		if err := c.markRoutes(ctx, namespace, obj, &res); err != nil {
			return res, err
		}
	}

	for _, obj := range desired {
		if obj.GetNamespace() == "" {
			obj = obj.DeepCopy()
			obj.SetNamespace(namespace)
		}
		if anyNamespace[obj.GetKind()] {
			elsewhere[obj.GetNamespace()] = true
		}
		action, err := c.apply(ctx, obj)
		if err != nil && optional[obj.GetKind()] && missingKind(err) {
			res.Skipped = append(res.Skipped, obj.GetKind()+" "+obj.GetNamespace()+"/"+obj.GetName())
			continue
		}
		if err != nil {
			return res, err
		}
		kept[objectKey(obj.GetKind(), obj.GetNamespace(), obj.GetName())] = true
		res.Applied++
		if action != "" {
			res.Changes = append(res.Changes, Change{Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(), Action: action})
		}
	}

	if err := c.prune(ctx, namespace, elsewhere, kept, held, &res); err != nil {
		return res, err
	}
	c.readGatewayStatus(ctx, namespace, desired, &res)
	return res, nil
}

func objectKey(kind, namespace, name string) string { return kind + "/" + namespace + "/" + name }

// markRoutes copies the quota revision of a QuotaPolicy to the routes of the
// cluster's own that send to the policy's backends. A new tenant rule only
// reaches the proxy when the route is built again, and a changed policy
// alone does not make that happen. This tool's own routes carry the revision
// already. On a route somebody else renders, the annotation is the one thing
// this tool writes.
func (c *Client) markRoutes(ctx context.Context, namespace string, policy *unstructured.Unstructured, res *SyncResult) error {
	revision := policy.GetAnnotations()[render.QuotaRevisionAnnotation]
	if policy.GetKind() != "QuotaPolicy" || revision == "" {
		return nil
	}
	if ns := policy.GetNamespace(); ns != "" {
		namespace = ns
	}
	backends := map[string]bool{}
	targets, _, _ := unstructured.NestedSlice(policy.Object, "spec", "targetRefs")
	for _, t := range targets {
		if ref, ok := t.(map[string]any); ok {
			name, _ := ref["name"].(string)
			backends[name] = true
		}
	}
	gvr, _ := gvrFor("AIGatewayRoute")
	routes, err := c.dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if missingKind(err) {
			return nil
		}
		return fmt.Errorf("list AIGatewayRoute in %s: %w", namespace, describe(err))
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{render.QuotaRevisionAnnotation: revision}}})
	if err != nil {
		return err
	}
	for _, route := range routes.Items {
		if route.GetLabels()[render.ManagedLabel] == render.ManagedValue ||
			route.GetAnnotations()[render.QuotaRevisionAnnotation] == revision || !sendsTo(route, backends) {
			continue
		}
		_, err := c.dyn.Resource(gvr).Namespace(namespace).Patch(ctx, route.GetName(), types.MergePatchType, patch,
			metav1.PatchOptions{FieldManager: fieldManager})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("mark AIGatewayRoute %s/%s for its new quota rules: %w", namespace, route.GetName(), describe(err))
		}
		res.Changes = append(res.Changes, Change{Kind: "AIGatewayRoute", Namespace: namespace, Name: route.GetName(), Action: "updated"})
	}
	return nil
}

// sendsTo reports whether a rule of the route names one of the backends.
func sendsTo(route unstructured.Unstructured, backends map[string]bool) bool {
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		refs, _, _ := unstructured.NestedSlice(rule, "backendRefs")
		for _, b := range refs {
			if ref, ok := b.(map[string]any); ok {
				if name, _ := ref["name"].(string); backends[name] {
					return true
				}
			}
		}
	}
	return false
}

// apply writes one object with server-side apply and reports what that did:
// "created", "updated", or "" when the object was already as desired.
func (c *Client) apply(ctx context.Context, obj *unstructured.Unstructured) (string, error) {
	kind, ns, name := obj.GetKind(), obj.GetNamespace(), obj.GetName()
	gvr, ok := gvrFor(kind)
	if !ok {
		return "", fmt.Errorf("unsupported kind %s", kind)
	}
	body, err := json.Marshal(obj.Object)
	if err != nil {
		return "", err
	}
	// Looking first is what tells a create from an update, and an update
	// from an apply that changed nothing.
	before, err := c.dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("read %s %s/%s: %w", kind, ns, name, describe(err))
	}
	existed := err == nil && before.GetName() == name
	force := true
	after, err := c.dyn.Resource(gvr).Namespace(ns).Patch(ctx, name, types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: fieldManager, Force: &force})
	if err != nil {
		return "", fmt.Errorf("apply %s %s/%s: %w", kind, ns, name, describe(err))
	}
	dropped, err := c.dropUnownedSecretEntries(ctx, gvr, obj, after)
	if err != nil {
		return "", fmt.Errorf("remove old entries from %s %s/%s: %w", kind, ns, name, describe(err))
	}
	switch {
	case !existed:
		return "created", nil
	case dropped || version(before) != version(after):
		return "updated", nil
	}
	return "", nil
}

// dropUnownedSecretEntries removes the entries of a Secret that the desired
// object does not have and that the apply left in place. Server-side apply
// only removes what it knows this tool owns, and it does not know that for
// entries written through stringData, which is how versions before 0.6.1
// wrote API keys. Without this, a key revoked back then would stay valid.
func (c *Client) dropUnownedSecretEntries(ctx context.Context, gvr schema.GroupVersionResource, desired, live *unstructured.Unstructured) (bool, error) {
	if desired.GetKind() != "Secret" || live == nil {
		return false, nil
	}
	want, _, _ := unstructured.NestedMap(desired.Object, "data")
	have, _, _ := unstructured.NestedMap(live.Object, "data")
	remove := map[string]any{}
	for name := range have {
		if _, ok := want[name]; !ok {
			remove[name] = nil // null deletes the entry in a merge patch
		}
	}
	if len(remove) == 0 {
		return false, nil
	}
	patch, err := json.Marshal(map[string]any{"data": remove})
	if err != nil {
		return false, err
	}
	_, err = c.dyn.Resource(gvr).Namespace(desired.GetNamespace()).Patch(ctx, desired.GetName(), types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: fieldManager})
	return err == nil, err
}

// prune deletes the objects this tool created earlier that are not in kept
// and whose name is not in held.
func (c *Client) prune(ctx context.Context, namespace string, elsewhere, kept, held map[string]bool, res *SyncResult) error {
	selector := render.ManagedLabel + "=" + render.ManagedValue
	for _, m := range managed {
		var items []unstructured.Unstructured
		var err error
		if anyNamespace[m.Kind] {
			items, err = c.managedAnywhere(ctx, m.GVR, selector, elsewhere)
		} else {
			items, err = c.managedObjects(ctx, namespace, m.Kind, m.GVR, selector)
		}
		if err != nil {
			return fmt.Errorf("list %s: %w", m.Kind, describe(err))
		}
		for _, item := range items {
			// Never rely on the server-side selector alone: deleting an object
			// this tool does not own would take down someone else's route.
			if item.GetLabels()[render.ManagedLabel] != render.ManagedValue {
				continue
			}
			if kept[objectKey(m.Kind, item.GetNamespace(), item.GetName())] || held[item.GetName()] {
				continue
			}
			err := c.dyn.Resource(m.GVR).Namespace(item.GetNamespace()).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete %s %s/%s: %w", m.Kind, item.GetNamespace(), item.GetName(), describe(err))
			}
			res.Pruned++
			res.Changes = append(res.Changes, Change{Kind: m.Kind, Namespace: item.GetNamespace(), Name: item.GetName(), Action: "deleted"})
		}
	}
	return nil
}

// managedAnywhere returns this tool's objects of one kind in every
// namespace, so one left behind in a namespace that no longer has what it
// belonged to is still found. Credentials that may not list across
// namespaces fall back to the namespaces in use now.
func (c *Client) managedAnywhere(ctx context.Context, gvr schema.GroupVersionResource, selector string, namespaces map[string]bool) ([]unstructured.Unstructured, error) {
	list, err := c.dyn.Resource(gvr).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if missingKind(err) {
		return nil, nil
	}
	if err == nil {
		return list.Items, nil
	}
	if !apierrors.IsForbidden(err) {
		return nil, err
	}
	var items []unstructured.Unstructured
	for ns := range namespaces {
		list, err := c.dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if missingKind(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		items = append(items, list.Items...)
	}
	return items, nil
}

// managedObjects returns the candidates for pruning. Secrets are looked up by
// their one known name instead of listed, so the hub's credentials never need
// permission to read the other Secrets in the namespace.
func (c *Client) managedObjects(ctx context.Context, namespace, kind string, gvr schema.GroupVersionResource, selector string) ([]unstructured.Unstructured, error) {
	if kind == "Secret" {
		obj, err := c.dyn.Resource(gvr).Namespace(namespace).Get(ctx, render.KeysSecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return []unstructured.Unstructured{*obj}, nil
	}
	list, err := c.dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if missingKind(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}
