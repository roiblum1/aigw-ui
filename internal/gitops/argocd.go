package gitops

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var applicationsGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// App is what Argo CD says of a deployment's Application.
type App struct {
	Name string `json:"name"`
	// Sync is Synced, OutOfSync or Unknown; Health is Healthy, Progressing,
	// Degraded, Missing, Suspended or Unknown.
	Sync   string `json:"sync"`
	Health string `json:"health"`
	// Revision is the commit of the values Argo CD last compared with.
	Revision string `json:"revision"`
	// Operation is the phase of the last sync (Running, Succeeded, Failed,
	// Error) and Message what it said, or an error condition of the
	// Application.
	Operation string `json:"operation"`
	Message   string `json:"message"`
}

// statusEvery is how long a list of Applications is used before Argo CD's
// API is asked again.
const statusEvery = 10 * time.Second

// ArgoCD reads the Applications in Argo CD's namespace, to show each
// deployment's by its name (AppName). It only reads.
type ArgoCD struct {
	client    dynamic.Interface
	namespace string

	mu      sync.Mutex
	at      time.Time
	apps    map[string]App
	lastErr error
}

// NewArgoCD reads Applications in namespace, with the pod's own account
// when kubeconfig is "" or with that kubeconfig file.
func NewArgoCD(namespace, kubeconfig string) (*ArgoCD, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("argo cd client: %w", err)
	}
	cfg.Timeout = 10 * time.Second
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("argo cd client: %w", err)
	}
	return newArgoCD(client, namespace), nil
}

func newArgoCD(client dynamic.Interface, namespace string) *ArgoCD {
	return &ArgoCD{client: client, namespace: namespace}
}

// Apps returns the Applications by name.
func (a *ArgoCD) Apps(ctx context.Context) (map[string]App, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.at) < statusEvery {
		return a.apps, a.lastErr
	}
	a.at = time.Now()
	list, err := a.client.Resource(applicationsGVR).Namespace(a.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		a.apps, a.lastErr = nil, fmt.Errorf("list Argo CD Applications in %s: %w", a.namespace, err)
		return nil, a.lastErr
	}
	apps := make(map[string]App, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		apps[item.GetName()] = appOf(item)
	}
	a.apps, a.lastErr = apps, nil
	return apps, nil
}

func appOf(item *unstructured.Unstructured) App {
	str := func(path ...string) string {
		s, _, _ := unstructured.NestedString(item.Object, path...)
		return s
	}
	app := App{
		Name:      item.GetName(),
		Sync:      str("status", "sync", "status"),
		Health:    str("status", "health", "status"),
		Revision:  str("status", "sync", "revision"),
		Operation: str("status", "operationState", "phase"),
		Message:   str("status", "operationState", "message"),
	}
	if app.Revision == "" {
		// A multi-source Application has one revision per source; the
		// values source is the last.
		if revs, ok, _ := unstructured.NestedStringSlice(item.Object, "status", "sync", "revisions"); ok && len(revs) > 0 {
			app.Revision = revs[len(revs)-1]
		}
	}
	// An error condition (a values file the chart refuses, an unknown
	// cluster) says more than the last sync's message.
	conds, _, _ := unstructured.NestedSlice(item.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if t, _ := m["type"].(string); len(t) > 5 && t[len(t)-5:] == "Error" {
			msg, _ := m["message"].(string)
			app.Message = t + ": " + msg
			break
		}
	}
	return app
}
