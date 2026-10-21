// Package kube applies rendered objects to one cluster and removes the ones
// this tool created earlier that are no longer wanted.
package kube

import (
	"aigw-ui/internal/render"
	"context"
	"fmt"
	"sync"
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
	{"AIGatewayRoute", schema.GroupVersionResource{Group: "aigateway.envoyproxy.io", Version: "v1beta1", Resource: "aigatewayroutes"}},
	{"AIServiceBackend", schema.GroupVersionResource{Group: "aigateway.envoyproxy.io", Version: "v1beta1", Resource: "aiservicebackends"}},
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

var httpRouteGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}

func gvrFor(kind string) (schema.GroupVersionResource, bool) {
	for _, m := range managed {
		if m.Kind == kind {
			return m.GVR, true
		}
	}
	return schema.GroupVersionResource{}, false
}

// Client talks to one cluster's API server. It is safe for concurrent use,
// and one Client per cluster shares its request rate between the sync,
// discovery and the measurements.
type Client struct {
	dyn  dynamic.Interface
	disc discovery.DiscoveryInterface
	// settle is how long Sync waits after a change before it reads what the
	// gateway's controller made of the objects.
	settle time.Duration

	// seen holds each object as this client's last sync left it, by
	// objectKey, so the next sync tells what its apply did without reading
	// the object first.
	mu   sync.Mutex
	seen map[string]seenObject

	// lengths holds what each workload pod's model server said its length
	// is, by pod (servedLength).
	lengthMu sync.Mutex
	lengths  map[types.UID]servedLength
}

// The requests a second a Client sends at most, after a burst. client-go's
// default of 5 after 10 made a sync of the lab's few models take 10 to 13 s
// (0.29.0), and a revoke wait behind it.
const (
	maxQPS   = 50
	maxBurst = 100
)

func New(kubeconfig []byte) (*Client, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	return newFromConfig(cfg)
}

func newFromConfig(cfg *rest.Config) (*Client, error) {
	cfg.Timeout = 20 * time.Second
	cfg.QPS, cfg.Burst = maxQPS, maxBurst
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

// ClientPort returns the port of the gateway's listener when it serves plain
// HTTP: Envoy Gateway's xDS names of version 2 name its route configuration
// after it. 0 for an HTTPS listener, and for a gateway or listener that is
// not there, which has no route configuration to patch. Any other failure is
// an error: a sync without the port would take the patches of a gateway
// that uses that name away.
func (c *Client) ClientPort(ctx context.Context, namespace, gateway, listener string) (int64, error) {
	gw, err := c.dyn.Resource(gatewayGVR).Namespace(namespace).Get(ctx, gateway, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read Gateway %s/%s: %w", namespace, gateway, describe(err))
	}
	listeners, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	for _, l := range listeners {
		m, _ := l.(map[string]any)
		if m["name"] != listener {
			continue
		}
		if protocol, _ := m["protocol"].(string); protocol != "HTTP" {
			return 0, nil
		}
		port, _, _ := unstructured.NestedInt64(m, "port")
		return port, nil
	}
	return 0, nil
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
