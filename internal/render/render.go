// Package render turns the desired state of one cluster into the Kubernetes
// objects that are applied to it. It is pure: no database, no cluster access.
//
// Object names and the namespace feed into the rate limit counter keys, so
// they must come out identical on every cluster for quotas to add up across
// sites.
package render

import (
	"regexp"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	ManagedLabel = "app.kubernetes.io/managed-by"
	ManagedValue = "aigw-ui"

	// ClientIDHeader carries "<tenant-slug>.<key-id>" once the gateway has
	// authenticated the API key.
	ClientIDHeader = "x-aigw-client-id"
	KeysSecretName = "aigw-ui-api-keys"
	AuthPolicyName = "aigw-ui-api-key-auth"

	aigwAPI = "aigateway.envoyproxy.io/v1alpha1"
	egAPI   = "gateway.envoyproxy.io/v1alpha1"
	// serviceQuotaLimit and serviceQuotaWindow fill spec.serviceQuota on every
	// QuotaPolicy. The field is optional and not enforced by the gateway, but
	// its controller writes the object back with an empty serviceQuota when
	// none is set, and the CRD rejects the empty duration, so every reconcile
	// fails to add its finalizer. A valid value makes that write succeed. The
	// limit is the largest the rate limit service can count per second, so it
	// stays "no limit" if the gateway ever starts enforcing the field.
	serviceQuotaLimit  int64 = 4294967295
	serviceQuotaWindow       = "1s"

	// ModelHeader is set by the AI gateway from the "model" field of the request body.
	ModelHeader = "x-ai-eg-model"
)

type TenantQuota struct {
	TenantSlug string
	// Slot is the fixed position of the tenant's rule in the model's
	// QuotaPolicy. It is unique per model and never changes, because the
	// position is part of the name of the rule's counter in Redis.
	Slot   int
	Limit  int64
	Window string
	// Shadow quotas are counted but never reject a request.
	Shadow bool
}

// Target is an AIServiceBackend and the model name requests carry when they
// reach it. QuotaPolicy matches on both.
type Target struct {
	// Namespace of the backend. Empty means the cluster's gateway namespace.
	Namespace string
	Backend   string
	Model     string
	// PeerOnly is set for a backend clients cannot reach through the
	// cluster's own routes: only other sites' gateways send to it.
	PeerOnly bool
}

type Model struct {
	Name          string
	Slug          string
	Host          string
	Port          int
	UpstreamModel string
	DefaultLimit  int64
	DefaultWindow string
	// CostExpression is CEL over the request's token counts. Empty leaves the
	// gateway's default, total_tokens. For a priced model it is the
	// expression of its prices.
	CostExpression string
	// DryRun counts every tenant of the model and refuses nobody. It is how
	// a model's prices are tried out before its quotas are enforced in money.
	DryRun bool
	Quotas []TenantQuota
	// Existing is set for a model discovered on the cluster: its route and
	// backends are already there and owned by someone else, so only the
	// QuotaPolicy is rendered and it is attached to these backends.
	Existing []Target
	// Fleet is set for a model whose entry route this tool renders: the
	// sites that serve it. Quotas attach to it, and to the Existing backends
	// that clients can still reach through a route of the cluster's own.
	Fleet []FleetSite
	// HeldReason is set for a model that has an entry route which cannot be
	// rendered right now, with the reason. Its entry objects on the cluster
	// are left as they are: removing the route would make the model
	// unreachable. Its quotas are still rendered.
	HeldReason string
	// BestEffort is set for a model that serves a tenant whose budget is
	// spent as the lowest class, on a second entry route, in place of
	// refusing it. It needs the entry route.
	BestEffort bool
	// Overage is the slugs of the tenants whose requests take that second
	// route right now, the ones that must not be left out first. The route
	// is rendered while there are any.
	Overage []string
	// BestEffortPools are the InferencePools that serve the model on this
	// cluster, set for a model in best-effort mode. Each gets the request
	// class the best-effort route names.
	BestEffortPools []Pool
}

// Held reports whether the model's objects on the cluster are left alone.
func (m Model) Held() bool { return m.HeldReason != "" }

func (m Model) targets() []Target {
	if len(m.Fleet) > 0 || m.Held() {
		out := []Target{{Backend: FleetName(m.Slug), Model: m.Name}}
		// A route the cluster already has for the model on the client
		// listener is older than the entry route and wins the match, so
		// clients still reach its backends. Without a quota there, turning
		// the entry route on would lift every tenant's limit. A backend
		// that only other sites reach gets none: the entry gateway has
		// charged the request already.
		for _, t := range m.Existing {
			if !t.PeerOnly {
				out = append(out, t)
			}
		}
		return out
	}
	if m.Existing != nil {
		return m.Existing
	}
	return []Target{{Backend: m.Slug, Model: m.UpstreamModel}}
}

// overageTarget returns the backend of the model's best-effort route, which
// has a QuotaPolicy of its own, and whether the model has one.
func (m Model) overageTarget() (Target, bool) {
	if !m.BestEffort || (len(m.Fleet) == 0 && !m.Held()) {
		return Target{}, false
	}
	return Target{Backend: BestEffortName(m.Slug), Model: m.Name}, true
}

type Key struct {
	ClientID string
	Value    string
}

type State struct {
	Namespace   string
	GatewayName string
	// ClientListener is the Gateway listener the API-key policy attaches
	// to. Empty attaches it to the whole Gateway.
	ClientListener string
	AuthEnabled    bool
	// Fleet is used by the models that have Fleet sites.
	Fleet  FleetConfig
	Models []Model
	Keys   []Key
}

// Objects returns the desired objects in the order they should be applied.
//
// A model's routes come before its QuotaPolicy, and that order matters. The
// gateway's controller takes a route's annotation over to the proxy only
// when something makes it look at the route again, and a changed
// QuotaPolicy is what does. See quotaRevision.
func Objects(s State) []*unstructured.Unstructured {
	models := append([]Model(nil), s.Models...)
	sort.Slice(models, func(i, j int) bool { return models[i].Slug < models[j].Slug })

	var out []*unstructured.Unstructured
	if s.AuthEnabled {
		out = append(out, keysSecret(s))
	}
	for _, m := range models {
		switch {
		case m.Held():
			// The entry objects stay on the cluster as they are. The quotas
			// are still rendered, against the entry backend that is there:
			// a model can be held for days, and a limit that was lowered
			// or a tenant that left must not wait for that.
		case len(m.Fleet) > 0:
			out = append(out, fleetObjects(s, m)...)
		case m.Existing == nil:
			out = append(out, backend(s, m), aiServiceBackend(s, m), route(s, m))
		}
		if len(m.Quotas) > 0 {
			out = append(out, quotaPolicies(s, m)...)
		}
		if t, ok := m.overageTarget(); ok {
			// Also without any tenant quota: the tenants that have none can
			// be the ones on the best-effort route.
			out = append(out, quotaPolicy(s.Namespace, m, []Target{t}, true))
		}
	}
	if s.AuthEnabled {
		out = append(out, authPolicy(s))
	}
	return append(out, objectives(s)...)
}

func object(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetLabels(map[string]string{ManagedLabel: ManagedValue})
	return u
}

// Redacted returns a copy with secret values masked, for display.
func Redacted(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		c := o.DeepCopy()
		if c.GetKind() == "Secret" {
			if data, ok := c.Object["data"].(map[string]any); ok {
				for k := range data {
					data[k] = "<redacted>"
				}
			}
		}
		out = append(out, c)
	}
	return out
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a model name such as "GLM5.3" into a Kubernetes object name.
func Slug(name string) string {
	b := []byte(name)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	s := slugUnsafe.ReplaceAllString(string(b), "-")
	for len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	if len(s) > 50 {
		s = s[:50]
	}
	for len(s) > 0 && s[len(s)-1] == '-' {
		s = s[:len(s)-1]
	}
	return s
}
