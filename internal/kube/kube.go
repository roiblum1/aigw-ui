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
	return &Client{dyn: dyn, disc: disc}, nil
}

type SyncResult struct {
	Applied int `json:"applied"`
	Pruned  int `json:"pruned"`
}

// Sync makes the managed objects in namespace match desired.
func (c *Client) Sync(ctx context.Context, namespace string, desired []*unstructured.Unstructured) (SyncResult, error) {
	var res SyncResult
	force := true
	want := map[string]bool{}

	for _, obj := range desired {
		gvr, ok := gvrFor(obj.GetKind())
		if !ok {
			return res, fmt.Errorf("unsupported kind %s", obj.GetKind())
		}
		body, err := json.Marshal(obj.Object)
		if err != nil {
			return res, err
		}
		_, err = c.dyn.Resource(gvr).Namespace(namespace).Patch(ctx, obj.GetName(), types.ApplyPatchType, body,
			metav1.PatchOptions{FieldManager: fieldManager, Force: &force})
		if err != nil {
			return res, fmt.Errorf("apply %s/%s: %w", obj.GetKind(), obj.GetName(), describe(err))
		}
		want[obj.GetKind()+"/"+obj.GetName()] = true
		res.Applied++
	}

	selector := render.ManagedLabel + "=" + render.ManagedValue
	for _, m := range managed {
		items, err := c.managedObjects(ctx, namespace, m.Kind, m.GVR, selector)
		if err != nil {
			return res, fmt.Errorf("list %s: %w", m.Kind, describe(err))
		}
		for _, item := range items {
			// Never rely on the server-side selector alone: deleting an object
			// this tool does not own would take down someone else's route.
			if item.GetLabels()[render.ManagedLabel] != render.ManagedValue {
				continue
			}
			if want[m.Kind+"/"+item.GetName()] {
				continue
			}
			err := c.dyn.Resource(m.GVR).Namespace(namespace).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return res, fmt.Errorf("delete %s/%s: %w", m.Kind, item.GetName(), describe(err))
			}
			res.Pruned++
		}
	}
	return res, nil
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
