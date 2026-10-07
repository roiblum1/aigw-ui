// Package kube applies rendered objects to one cluster and removes the ones
// this tool created earlier that are no longer wanted.
package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"aigw-ui/internal/render"
)

const fieldManager = "aigw-ui"

// managed lists every kind this tool owns, in prune order.
var managed = []struct {
	Kind string
	GVR  schema.GroupVersionResource
}{
	{"SecurityPolicy", schema.GroupVersionResource{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Resource: "securitypolicies"}},
	{"QuotaPolicy", schema.GroupVersionResource{Group: "aigateway.envoyproxy.io", Version: "v1alpha1", Resource: "quotapolicies"}},
	{"AIGatewayRoute", schema.GroupVersionResource{Group: "aigateway.envoyproxy.io", Version: "v1alpha1", Resource: "aigatewayroutes"}},
	{"AIServiceBackend", schema.GroupVersionResource{Group: "aigateway.envoyproxy.io", Version: "v1alpha1", Resource: "aiservicebackends"}},
	{"Backend", schema.GroupVersionResource{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Resource: "backends"}},
	{"Secret", schema.GroupVersionResource{Version: "v1", Resource: "secrets"}},
}

var gatewayGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}

func gvrFor(kind string) (schema.GroupVersionResource, bool) {
	for _, m := range managed {
		if m.Kind == kind {
			return m.GVR, true
		}
	}
	return schema.GroupVersionResource{}, false
}

type Client struct {
	dyn  dynamic.Interface
	disc discovery.DiscoveryInterface
	// settle is how long Sync waits after a change before it reads what the
	// gateway's controller made of the objects.
	settle time.Duration
}

func New(kubeconfig []byte) (*Client, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	return newFromConfig(cfg)
}

func newFromConfig(cfg *rest.Config) (*Client, error) {
	cfg.Timeout = 20 * time.Second
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{dyn: dyn, disc: disc, settle: 2 * time.Second}, nil
}

type SyncResult struct {
	Applied int `json:"applied"`
	Pruned  int `json:"pruned"`
	// Changes lists the objects this sync created, updated or deleted, as the
	// cluster's API server confirmed them. Objects that were already as
	// desired are counted in Applied but not listed.
	Changes []Change `json:"changes"`
	// Rejected lists objects the gateway's controller reports as not accepted.
	Rejected []Change `json:"rejected"`
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
// except QuotaPolicies, which sit next to the backends they target and so can
// be in any namespace.
func (c *Client) Sync(ctx context.Context, namespace string, desired []*unstructured.Unstructured) (SyncResult, error) {
	var res SyncResult
	force := true
	want := map[string]bool{}
	quotaNamespaces := map[string]bool{namespace: true}

	for _, obj := range desired {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = namespace
		}
		if obj.GetKind() == "QuotaPolicy" {
			quotaNamespaces[ns] = true
		}
		gvr, ok := gvrFor(obj.GetKind())
		if !ok {
			return res, fmt.Errorf("unsupported kind %s", obj.GetKind())
		}
		body, err := json.Marshal(obj.Object)
		if err != nil {
			return res, err
		}
		// Looking first is what tells a create from an update, and an update
		// from an apply that changed nothing.
		before, err := c.dyn.Resource(gvr).Namespace(ns).Get(ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return res, fmt.Errorf("read %s %s/%s: %w", obj.GetKind(), ns, obj.GetName(), describe(err))
		}
		existed := err == nil && before.GetName() == obj.GetName()
		after, err := c.dyn.Resource(gvr).Namespace(ns).Patch(ctx, obj.GetName(), types.ApplyPatchType, body,
			metav1.PatchOptions{FieldManager: fieldManager, Force: &force})
		if err != nil {
			return res, fmt.Errorf("apply %s %s/%s: %w", obj.GetKind(), ns, obj.GetName(), describe(err))
		}
		want[obj.GetKind()+"/"+ns+"/"+obj.GetName()] = true
		res.Applied++
		switch {
		case !existed:
			res.Changes = append(res.Changes, Change{Kind: obj.GetKind(), Namespace: ns, Name: obj.GetName(), Action: "created"})
		case version(before) != version(after):
			res.Changes = append(res.Changes, Change{Kind: obj.GetKind(), Namespace: ns, Name: obj.GetName(), Action: "updated"})
		}
	}

	selector := render.ManagedLabel + "=" + render.ManagedValue
	for _, m := range managed {
		var items []unstructured.Unstructured
		var err error
		if m.Kind == "QuotaPolicy" {
			items, err = c.managedQuotaPolicies(ctx, m.GVR, selector, quotaNamespaces)
		} else {
			items, err = c.managedObjects(ctx, namespace, m.Kind, m.GVR, selector)
		}
		if err != nil {
			return res, fmt.Errorf("list %s: %w", m.Kind, describe(err))
		}
		for _, item := range items {
			// Never rely on the server-side selector alone: deleting an object
			// this tool does not own would take down someone else's route.
			if item.GetLabels()[render.ManagedLabel] != render.ManagedValue {
				continue
			}
			if want[m.Kind+"/"+item.GetNamespace()+"/"+item.GetName()] {
				continue
			}
			err := c.dyn.Resource(m.GVR).Namespace(item.GetNamespace()).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return res, fmt.Errorf("delete %s %s/%s: %w", m.Kind, item.GetNamespace(), item.GetName(), describe(err))
			}
			res.Pruned++
			res.Changes = append(res.Changes, Change{Kind: m.Kind, Namespace: item.GetNamespace(), Name: item.GetName(), Action: "deleted"})
		}
	}
	c.readGatewayStatus(ctx, namespace, desired, &res)
	return res, nil
}

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

// managedQuotaPolicies returns this tool's QuotaPolicies in every namespace, so
// one left behind in a namespace that no longer has a backend is still found.
// Credentials that may not list across namespaces fall back to the namespaces
// in use now.
func (c *Client) managedQuotaPolicies(ctx context.Context, gvr schema.GroupVersionResource, selector string, namespaces map[string]bool) ([]unstructured.Unstructured, error) {
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

type Probe struct {
	KubernetesVersion string          `json:"kubernetes_version"`
	GatewayFound      bool            `json:"gateway_found"`
	Kinds             map[string]bool `json:"kinds"`
}

// Probe reports whether the cluster is reachable and has what a sync needs.
func (c *Client) Probe(ctx context.Context, namespace, gatewayName string) (Probe, error) {
	p := Probe{Kinds: map[string]bool{}}
	v, err := c.disc.ServerVersion()
	if err != nil {
		return p, describe(err)
	}
	p.KubernetesVersion = v.GitVersion

	for _, m := range managed {
		if m.GVR.Group == "" {
			continue
		}
		p.Kinds[m.Kind] = false
		list, err := c.disc.ServerResourcesForGroupVersion(m.GVR.GroupVersion().String())
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			if r.Name == m.GVR.Resource {
				p.Kinds[m.Kind] = true
			}
		}
	}

	_, err = c.dyn.Resource(gatewayGVR).Namespace(namespace).Get(ctx, gatewayName, metav1.GetOptions{})
	p.GatewayFound = err == nil
	return p, nil
}

func missingKind(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}

func describe(err error) error {
	if missingKind(err) {
		return fmt.Errorf("%w (is the CRD installed on this cluster?)", err)
	}
	return err
}
