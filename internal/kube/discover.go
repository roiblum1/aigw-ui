package kube

import (
	"context"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/render"
)

// ModelBackend is an AIServiceBackend a route sends a model to, and the model
// name the request carries when it gets there.
type ModelBackend struct {
	Name  string
	Model string
}

type DiscoveredModel struct {
	Name     string
	Backends []ModelBackend
}

// Discover lists the models that the AIGatewayRoutes in namespace already
// expose. Routes created by this tool are skipped: they are desired state,
// not something to learn from the cluster.
func (c *Client) Discover(ctx context.Context, namespace string) ([]DiscoveredModel, error) {
	gvr, _ := gvrFor("AIGatewayRoute")
	list, err := c.dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, describe(err)
	}
	return modelsFromRoutes(list.Items), nil
}

// modelsFromRoutes reads the models out of route rules. A rule exposes a model
// when it matches the model header exactly; other match types cannot be mapped
// to one model name and are ignored.
func modelsFromRoutes(routes []unstructured.Unstructured) []DiscoveredModel {
	byName := map[string]*DiscoveredModel{}
	for _, route := range routes {
		if route.GetLabels()[render.ManagedLabel] == render.ManagedValue {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
		for _, r := range rules {
			rule, ok := r.(map[string]any)
			if !ok {
				continue
			}
			refs := backendRefs(rule)
			if len(refs) == 0 {
				continue
			}
			for _, model := range matchedModels(rule) {
				d := byName[model]
				if d == nil {
					d = &DiscoveredModel{Name: model}
					byName[model] = d
				}
				for _, ref := range refs {
					b := ModelBackend{Name: ref.name, Model: ref.override}
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
			return d.Backends[i].Name+"\x00"+d.Backends[i].Model < d.Backends[j].Name+"\x00"+d.Backends[j].Model
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

type backendRef struct{ name, override string }

func backendRefs(rule map[string]any) []backendRef {
	var out []backendRef
	refs, _, _ := unstructured.NestedSlice(rule, "backendRefs")
	for _, r := range refs {
		ref, ok := r.(map[string]any)
		if !ok {
			continue
		}
		// QuotaPolicy can only target AIServiceBackends in its own namespace.
		if kind, _ := ref["kind"].(string); kind != "" && kind != "AIServiceBackend" {
			continue
		}
		if ns, _ := ref["namespace"].(string); ns != "" {
			continue
		}
		name, _ := ref["name"].(string)
		override, _ := ref["modelNameOverride"].(string)
		if name != "" {
			out = append(out, backendRef{name, override})
		}
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
