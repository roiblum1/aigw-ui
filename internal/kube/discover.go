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
	// PeerOnly is true when no route that sends the model to this backend
	// can be reached from the listener clients come in on. Such a backend
	// only sees requests another site's gateway has already charged.
	PeerOnly bool
}

// DiscoveredModel is a model that routes expose. Backends is empty when none
// of its backends can carry a quota, for example a model served only from an
// InferencePool.
type DiscoveredModel struct {
	Name     string
	Backends []ModelBackend
}

// Gateway names a cluster's gateway and the listener clients come in on.
// ClientListener is empty when the gateway does not tell clients and other
// sites apart.
type Gateway struct {
	Namespace, Name, ClientListener string
}

// Discover lists the models that the AIGatewayRoutes in the gateway's
// namespace expose. Routes created by this tool are skipped: they are desired
// state, not something to learn from the cluster.
func (c *Client) Discover(ctx context.Context, gw Gateway) ([]DiscoveredModel, error) {
	gvr, _ := gvrFor("AIGatewayRoute")
	list, err := c.dyn.Resource(gvr).Namespace(gw.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, describe(err)
	}
	return modelsFromRoutes(list.Items, gw, false), nil
}

// DiscoverAttached lists the models exposed by routes in any namespace that
// are attached to the given gateway.
func (c *Client) DiscoverAttached(ctx context.Context, gw Gateway) ([]DiscoveredModel, error) {
	gvr, _ := gvrFor("AIGatewayRoute")
	list, err := c.dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, describe(err)
	}
	return modelsFromRoutes(list.Items, gw, true), nil
}

// listeners returns the listeners of the gateway a route is attached to, and
// whether it is attached at all. An empty name stands for every listener.
func (gw Gateway) listeners(route *unstructured.Unstructured) ([]string, bool) {
	var sections []string
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
		if name, _ := parent["name"].(string); name == gw.Name && ns == gw.Namespace {
			section, _ := parent["sectionName"].(string)
			sections = append(sections, section)
		}
	}
	return sections, len(sections) > 0
}

// peerOnly reports whether clients cannot reach the route: it is attached to
// the gateway, and only to listeners other than the client one. Anything
// that is not certain counts as reachable, because a backend clients can
// reach must keep its quota.
func (gw Gateway) peerOnly(route *unstructured.Unstructured) bool {
	sections, attached := gw.listeners(route)
	if gw.ClientListener == "" || !attached {
		return false
	}
	for _, section := range sections {
		if section == "" || section == gw.ClientListener {
			return false
		}
	}
	return true
}

// modelsFromRoutes reads the models out of route rules. A rule exposes a model
// when it matches the model header exactly; other match types cannot be mapped
// to one model name and are ignored.
//
// With attachedOnly, routes that are not attached to gw are left out.
func modelsFromRoutes(routes []unstructured.Unstructured, gw Gateway, attachedOnly bool) []DiscoveredModel {
	byName := map[string]*DiscoveredModel{}
	for i := range routes {
		route := &routes[i]
		if route.GetLabels()[render.ManagedLabel] == render.ManagedValue {
			continue
		}
		if _, attached := gw.listeners(route); attachedOnly && !attached {
			continue
		}
		peerOnly := gw.peerOnly(route)
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
					b.PeerOnly = peerOnly
					if i := indexBackend(d.Backends, b); i < 0 {
						d.Backends = append(d.Backends, b)
					} else if !peerOnly {
						// One route clients can reach is enough.
						d.Backends[i].PeerOnly = false
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

// indexBackend finds a backend whatever its PeerOnly, or returns -1.
func indexBackend(list []ModelBackend, b ModelBackend) int {
	for i, x := range list {
		x.PeerOnly = b.PeerOnly
		if x == b {
			return i
		}
	}
	return -1
}
