package kube

import (
	"context"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/render"
)

// ModelBackend is an AIServiceBackend a route sends a model to.
type ModelBackend struct {
	Name      string
	Namespace string
	// Model is the name a QuotaPolicy has to use for this backend: the
	// route's modelNameOverride, or the model name itself when there is none.
	Model string
	// Override reports whether the route sets modelNameOverride. The gateway
	// documents quota matching only for that case.
	Override bool
}

// DiscoveredModel is a model that routes expose. Backends is empty when none
// of its backends can carry a quota, for example a model served only from an
// InferencePool.
type DiscoveredModel struct {
	Name     string
	Backends []ModelBackend
}

// Discover lists the models that the AIGatewayRoutes in namespace expose.
// Routes created by this tool are skipped: they are desired state, not
// something to learn from the cluster.
func (c *Client) Discover(ctx context.Context, namespace string) ([]DiscoveredModel, error) {
	gvr, _ := gvrFor("AIGatewayRoute")
	list, err := c.dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, describe(err)
	}
	return modelsFromRoutes(list.Items, nil), nil
}

// DiscoverAttached lists the models exposed by routes in any namespace that
// are attached to the given gateway.
func (c *Client) DiscoverAttached(ctx context.Context, gatewayNamespace, gatewayName string) ([]DiscoveredModel, error) {
	gvr, _ := gvrFor("AIGatewayRoute")
	list, err := c.dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, describe(err)
	}
	return modelsFromRoutes(list.Items, func(route *unstructured.Unstructured) bool {
		return attachedTo(route, gatewayNamespace, gatewayName)
	}), nil
}

func attachedTo(route *unstructured.Unstructured, gatewayNamespace, gatewayName string) bool {
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	for _, p := range parents {
		parent, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := parent["kind"].(string); kind != "" && kind != "Gateway" {
			continue
		}
		ns, _ := parent["namespace"].(string)
		if ns == "" {
			ns = route.GetNamespace()
		}
		if name, _ := parent["name"].(string); name == gatewayName && ns == gatewayNamespace {
			return true
		}
	}
	return false
}

// modelsFromRoutes reads the models out of route rules. A rule exposes a model
// when it matches the model header exactly; other match types cannot be mapped
// to one model name and are ignored.
func modelsFromRoutes(routes []unstructured.Unstructured, keep func(*unstructured.Unstructured) bool) []DiscoveredModel {
	byName := map[string]*DiscoveredModel{}
	for i := range routes {
		route := &routes[i]
		if route.GetLabels()[render.ManagedLabel] == render.ManagedValue {
			continue
		}
		if keep != nil && !keep(route) {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
		for _, r := range rules {
			rule, ok := r.(map[string]any)
			if !ok {
				continue
			}
			refs := backendRefs(rule, route.GetNamespace())
			for _, model := range matchedModels(rule) {
				d := byName[model]
				if d == nil {
					d = &DiscoveredModel{Name: model}
					byName[model] = d
				}
				for _, b := range refs {
					if b.Model == "" {
						b.Model = model
					}
					if !containsBackend(d.Backends, b) {
						d.Backends = append(d.Backends, b)
					}
				}
			}
		}
	}

	out := make([]DiscoveredModel, 0, len(byName))
	for _, d := range byName {
		// A stable order keeps an unchanged cluster from looking changed.
		sort.Slice(d.Backends, func(i, j int) bool {
			a, b := d.Backends[i], d.Backends[j]
			return a.Namespace+"\x00"+a.Name+"\x00"+a.Model < b.Namespace+"\x00"+b.Name+"\x00"+b.Model
		})
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func matchedModels(rule map[string]any) []string {
	var models []string
	matches, _, _ := unstructured.NestedSlice(rule, "matches")
	for _, m := range matches {
		match, ok := m.(map[string]any)
		if !ok {
			continue
		}
		headers, _, _ := unstructured.NestedSlice(match, "headers")
		for _, h := range headers {
			header, ok := h.(map[string]any)
			if !ok {
				continue
			}
			name, _ := header["name"].(string)
			kind, _ := header["type"].(string)
			value, _ := header["value"].(string)
			// Exact is the default match type.
			if strings.EqualFold(name, render.ModelHeader) && (kind == "" || kind == "Exact") && value != "" {
				models = append(models, value)
			}
		}
	}
	return models
}

// backendRefs returns the rule's AIServiceBackends. Other kinds, such as
// InferencePool, are left out because a QuotaPolicy cannot target them.
func backendRefs(rule map[string]any, routeNamespace string) []ModelBackend {
	var out []ModelBackend
	refs, _, _ := unstructured.NestedSlice(rule, "backendRefs")
	for _, r := range refs {
		ref, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := ref["kind"].(string); kind != "" && kind != "AIServiceBackend" {
			continue
		}
		name, _ := ref["name"].(string)
		if name == "" {
			continue
		}
		ns, _ := ref["namespace"].(string)
		if ns == "" {
			ns = routeNamespace
		}
		override, _ := ref["modelNameOverride"].(string)
		out = append(out, ModelBackend{Name: name, Namespace: ns, Model: override, Override: override != ""})
	}
	return out
}

func containsBackend(list []ModelBackend, b ModelBackend) bool {
	for _, x := range list {
		if x == b {
			return true
		}
	}
	return false
}
