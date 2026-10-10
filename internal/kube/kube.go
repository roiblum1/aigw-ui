// Package kube applies rendered objects to one cluster and removes the ones
// this tool created earlier that are no longer wanted.
package kube

import (
	"aigw-ui/internal/render"
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const fieldManager = "aigw-ui"

// managed lists every kind this tool owns, in prune order.
var managed = []struct {
	Kind string
	GVR  schema.GroupVersionResource
}{
	{"EnvoyPatchPolicy", schema.GroupVersionResource{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Resource: "envoypatchpolicies"}},
	{"BackendTrafficPolicy", schema.GroupVersionResource{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Resource: "backendtrafficpolicies"}},
	{"SecurityPolicy", schema.GroupVersionResource{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Resource: "securitypolicies"}},
	{"QuotaPolicy", schema.GroupVersionResource{Group: "aigateway.envoyproxy.io", Version: "v1alpha1", Resource: "quotapolicies"}},
	{"AIGatewayRoute", schema.GroupVersionResource{Group: "aigateway.envoyproxy.io", Version: "v1alpha1", Resource: "aigatewayroutes"}},
	{"AIServiceBackend", schema.GroupVersionResource{Group: "aigateway.envoyproxy.io", Version: "v1alpha1", Resource: "aiservicebackends"}},
	{"Backend", schema.GroupVersionResource{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Resource: "backends"}},
	{"Secret", schema.GroupVersionResource{Version: "v1", Resource: "secrets"}},
	{render.ObjectiveKind, schema.GroupVersionResource{Group: "llm-d.ai", Version: "v1alpha2", Resource: "inferenceobjectives"}},
}

// anyNamespace holds the kinds that sit next to what they belong to, not in
// the gateway's namespace: a QuotaPolicy next to the backends it targets, a
// request class next to the model's pool.
var anyNamespace = map[string]bool{"QuotaPolicy": true, render.ObjectiveKind: true}

// optional holds the kinds a cluster may not have. An object of such a kind
// is left out there, and everything else is still synced.
var optional = map[string]bool{render.ObjectiveKind: true}

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
