package kube

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"aigw-ui/internal/render"
)

func TestPatchVerdict(t *testing.T) {
	policy := func(conditions ...map[string]any) *unstructured.Unstructured {
		list := make([]any, 0, len(conditions))
		for _, c := range conditions {
			list = append(list, c)
		}
		return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"ancestors": []any{map[string]any{"conditions": list}}}}}
	}
	cond := func(kind, status, message string) map[string]any {
		return map[string]any{"type": kind, "status": status, "message": message}
	}
	for name, tc := range map[string]struct {
		policy  *unstructured.Unstructured
		verdict string
	}{
		"in effect":           {policy(cond("Accepted", "True", ""), cond("Programmed", "True", "")), "Programmed"},
		"patches not enabled": {policy(cond("Accepted", "False", "EnvoyPatchPolicy is disabled")), "NotAccepted"},
		"nothing to patch":    {policy(cond("Accepted", "True", ""), cond("Programmed", "False", "no match")), "NotProgrammed"},
		"nothing reported":    {&unstructured.Unstructured{Object: map[string]any{}}, ""},
		"accepted only":       {policy(cond("Accepted", "True", "")), ""},
	} {
		if got, _ := patchVerdict(tc.policy, "Programmed"); got != tc.verdict {
			t.Errorf("%s: verdict %q, want %q", name, got, tc.verdict)
		}
	}

	// The tenants' budget policy as Envoy Gateway 1.9.2 reported it while a
	// fleet entry route had a policy of its own (lab, 2026-10-16).
	for name, tc := range map[string]struct {
		policy  *unstructured.Unstructured
		verdict string
	}{
		"accepted":   {policy(cond("Accepted", "True", "Policy has been accepted.")), "Accepted"},
		"merged":     {policy(cond("Accepted", "True", ""), cond("Merged", "True", "This policy is being merged by other backendTrafficPolicies")), "Accepted"},
		"overridden": {policy(cond("Accepted", "True", ""), cond("Overridden", "True", "This policy is being overridden by other backendTrafficPolicies for these routes: [ns/fleet-m]")), "Overridden"},
	} {
		got, message := patchVerdict(tc.policy, "Accepted")
		if got != tc.verdict {
			t.Errorf("%s: verdict %q, want %q", name, got, tc.verdict)
		}
		if got == "Overridden" && !strings.Contains(message, "fleet-m") {
			t.Errorf("%s: message %q does not name the route", name, message)
		}
	}
}

// The hub's SecurityPolicy as Envoy Gateway reported it on a serving site
// whose KServe route on the client listener had a SecurityPolicy of its own
// (site export, 2026-10-16): accepted, and overridden for that route. That
// is a warning in the sync, not a refusal. The gateway chart's health check
// overrides it wherever it is on (the lab, 2026-10-17) and reaches no model,
// so it is left out of the warning once the hub can see that.
func TestSecurityPolicyOverridden(t *testing.T) {
	gvr, _ := gvrFor("SecurityPolicy")
	status := func(conditions ...any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "gateway.envoyproxy.io/v1alpha1", "kind": "SecurityPolicy",
			"metadata": map[string]any{"name": "aigw-ui-api-key-auth", "namespace": "gw"},
			"status":   map[string]any{"ancestors": []any{map[string]any{"conditions": conditions}}},
		}}
	}
	accepted := map[string]any{"type": "Accepted", "status": "True", "message": "Policy has been accepted."}
	overridden := func(routes string) map[string]any {
		return map[string]any{"type": "Overridden", "status": "True",
			"message": "This policy is being overridden by other securityPolicies for " + routes}
	}
	refused := map[string]any{"type": "Accepted", "status": "False", "message": "no such listener"}
	// The gateway chart's health check answers itself; KServe's route goes
	// to the model's pool.
	route := func(ns, name string, rule map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
			"metadata": map[string]any{"name": name, "namespace": ns},
			"spec":     map[string]any{"rules": []any{rule}},
		}}
	}
	health := route("gw", "envoy-ai-gateway-health", map[string]any{
		"matches": []any{map[string]any{"path": map[string]any{"type": "Exact", "value": "/healthz/gateway"}}},
		"filters": []any{map[string]any{"type": "ExtensionRef", "extensionRef": map[string]any{"group": "gateway.envoyproxy.io", "kind": "HTTPRouteFilter", "name": "health"}}},
	})
	kserve := route("llm-glm53", "glm53-b200-kserve-route", map[string]any{
		"backendRefs": []any{map[string]any{"group": "inference.networking.k8s.io", "kind": "InferencePool", "name": "glm53-b200"}},
	})
	mirror := route("gw", "shadow", map[string]any{
		"filters": []any{map[string]any{"type": "RequestMirror", "requestMirror": map[string]any{"backendRef": map[string]any{"name": "glm53"}}}},
	})
	for name, tc := range map[string]struct {
		live               *unstructured.Unstructured
		routes             []runtime.Object
		warnings, rejected int
		names, omits       string
	}{
		"accepted": {live: status(accepted)},
		"overridden by a route the hub cannot read": {live: status(accepted, overridden("these routes: [llm-glm53/glm53-b200-kserve-route]")),
			warnings: 1, names: "glm53-b200-kserve-route"},
		"overridden by a model's route": {live: status(accepted, overridden("these routes: [llm-glm53/glm53-b200-kserve-route]")),
			routes: []runtime.Object{kserve}, warnings: 1, names: "glm53-b200-kserve-route"},
		"overridden by the health check": {live: status(accepted, overridden("these routes: [gw/envoy-ai-gateway-health]")),
			routes: []runtime.Object{health}},
		"overridden by the health check and a model's route": {live: status(accepted, overridden("these routes: [gw/envoy-ai-gateway-health llm-glm53/glm53-b200-kserve-route]")),
			routes: []runtime.Object{health, kserve}, warnings: 1, names: "these routes: [llm-glm53/glm53-b200-kserve-route]", omits: "health"},
		"overridden by a route that mirrors": {live: status(accepted, overridden("these routes: [gw/shadow]")),
			routes: []runtime.Object{mirror}, warnings: 1, names: "gw/shadow"},
		"overridden on a route rule": {live: status(accepted, overridden("these routes: [gw/envoy-ai-gateway-health] and these route rules: [gw/other/rule-0]")),
			routes: []runtime.Object{health}, warnings: 1, names: "gw/other/rule-0"},
		"refused": {live: status(refused), rejected: 1},
	} {
		objs := append([]runtime.Object{tc.live}, tc.routes...)
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
			map[schema.GroupVersionResource]string{gvr: "SecurityPolicyList", httpRouteGVR: "HTTPRouteList"}, objs...)
		var res SyncResult
		desired := tc.live.DeepCopy()
		delete(desired.Object, "status")
		(&Client{dyn: dyn}).readGatewayStatus(context.Background(), "gw", []*unstructured.Unstructured{desired}, nil, &res)
		if len(res.Warnings) != tc.warnings || len(res.Rejected) != tc.rejected {
			t.Errorf("%s: warnings %v, rejected %v", name, res.Warnings, res.Rejected)
			continue
		}
		if tc.warnings > 0 && !strings.Contains(res.Warnings[0], tc.names) {
			t.Errorf("%s: the warning does not name %q: %s", name, tc.names, res.Warnings[0])
		}
		if tc.omits != "" && strings.Contains(res.Warnings[0], tc.omits) {
			t.Errorf("%s: the warning names %q: %s", name, tc.omits, res.Warnings[0])
		}
	}
}

// A retry patch written for both names of the route configuration is in
// effect when Envoy Gateway reports only the other name as not found, as
// its translator words it (1.9.2, internal/xds/translator/jsonpatch.go).
// When both are missing, the route configuration is.
func TestOtherSchemeOnly(t *testing.T) {
	policy := &unstructured.Unstructured{Object: map[string]any{}}
	policy.SetAnnotations(map[string]string{render.RouteConfigsAnnotation: "gw/envoy-ai-gateway/http,http-80"})
	missing := func(names ...string) string {
		var list []string
		for _, n := range names {
			list = append(list, render.RouteConfigType+"/"+n)
		}
		return "Unable to find xds resources: " + strings.Join(list, ",")
	}
	for message, want := range map[string]bool{
		missing("http-80", "http-80", "http-80"):                        true,
		missing("gw/envoy-ai-gateway/http", "gw/envoy-ai-gateway/http"): true,
		missing("gw/envoy-ai-gateway/http", "http-80"):                  false,
		missing("http-8080"):                                            false,
		"jsonpath ..routes matched nothing":                             false,
	} {
		if got := otherSchemeOnly(policy, message); got != want {
			t.Errorf("%q: %v, want %v", message, got, want)
		}
	}
	if otherSchemeOnly(&unstructured.Unstructured{Object: map[string]any{}}, missing("http-80")) {
		t.Error("a patch with one name has no other")
	}
}

// The port is read from the client listener when it serves plain HTTP.
func TestClientPort(t *testing.T) {
	gw := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
		"metadata": map[string]any{"name": "envoy-ai-gateway", "namespace": "gw"},
		"spec": map[string]any{"listeners": []any{
			map[string]any{"name": "http", "port": int64(80), "protocol": "HTTP"},
			map[string]any{"name": "peers", "port": int64(8443), "protocol": "HTTPS"},
		}},
	}}
	// The fake would file a Gateway under "gatewaies", so it is added by its
	// resource.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gatewayGVR: "GatewayList"})
	if err := dyn.Tracker().Create(gatewayGVR, gw, "gw"); err != nil {
		t.Fatal(err)
	}
	c := &Client{dyn: dyn}
	for listener, want := range map[string]int64{"http": 80, "peers": 0, "none": 0} {
		if got, err := c.ClientPort(context.Background(), "gw", "envoy-ai-gateway", listener); err != nil || got != want {
			t.Errorf("%s: %d %v, want %d", listener, got, err, want)
		}
	}
	if got, err := c.ClientPort(context.Background(), "gw", "missing", "http"); err != nil || got != 0 {
		t.Errorf("no gateway: %d %v", got, err)
	}
}

// The hub's best-effort class has to rank below the site's own "standard"
// for the same pool, which regular tenants' requests name, and "standard"
// must not be below 0. A "standard" for another pool, or none, is 0 to the
// pool's scheduler.
func TestClassOrder(t *testing.T) {
	gvr, _ := gvrFor(render.ObjectiveKind)
	class := func(name string, priority any, pool string) *unstructured.Unstructured {
		spec := map[string]any{"poolRef": map[string]any{"group": "inference.networking.k8s.io", "kind": "InferencePool", "name": pool}}
		if priority != nil {
			spec["priority"] = priority
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "llm-d.ai/v1alpha2", "kind": render.ObjectiveKind,
			"metadata": map[string]any{"name": name, "namespace": "llm-glm53"},
			"spec":     spec,
		}}
	}
	for name, tc := range map[string]struct {
		standard   *unstructured.Unstructured
		bestEffort int64
		want       string
	}{
		"no standard":                {nil, -1, ""},
		"standard at 0":              {class("standard", int64(0), "glm53-b200-inference-pool"), -1, ""},
		"standard without priority":  {class("standard", nil, "glm53-b200-inference-pool"), -1, ""},
		"standard equal":             {class("standard", int64(-1), "glm53-b200-inference-pool"), -1, "not above best-effort's -1"},
		"standard below":             {class("standard", int64(-5), "glm53-b200-inference-pool"), -1, "has priority -5, not above"},
		"standard above but below 0": {class("standard", int64(-1), "glm53-b200-inference-pool"), -10, "below 0"},
		"standard of another pool":   {class("standard", int64(-5), "glm53-h200-inference-pool"), -1, ""},
	} {
		var objs []runtime.Object
		if tc.standard != nil {
			objs = append(objs, tc.standard)
		}
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds(gvr, "InferenceObjectiveList"), objs...)
		var res SyncResult
		desired := class("best-effort", tc.bestEffort, "glm53-b200-inference-pool")
		(&Client{dyn: dyn}).readGatewayStatus(context.Background(), "gw", []*unstructured.Unstructured{desired}, nil, &res)
		switch {
		case tc.want == "" && len(res.Warnings) != 0:
			t.Errorf("%s: warnings %v", name, res.Warnings)
		case tc.want != "" && (len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], tc.want) ||
			!strings.Contains(res.Warnings[0], "InferencePool llm-glm53/glm53-b200-inference-pool")):
			t.Errorf("%s: warnings %v, want %q", name, res.Warnings, tc.want)
		}
	}
}

// listKinds registers a list kind for the fake client, with the
// LLMInferenceServices and their configs the class checks list.
func listKinds(gvr schema.GroupVersionResource, kind string) map[schema.GroupVersionResource]string {
	lists := map[schema.GroupVersionResource]string{gvr: kind}
	for _, g := range llmServiceGVRs {
		lists[g] = "LLMInferenceServiceList"
	}
	for _, g := range llmConfigGVRs {
		lists[g] = "LLMInferenceServiceConfigList"
	}
	return lists
}

// A scheduler runs flow control when the config that applies has the gate:
// the service's own, or else the last config it is built on that has one.
// None at all is the default, without it. A config in a ConfigMap, or one
// that could not be read, says nothing.
func TestFlowControl(t *testing.T) {
	config := func(v any) map[string]any {
		return map[string]any{"router": map[string]any{"scheduler": map[string]any{"config": v}}}
	}
	gates := func(g ...any) map[string]any { return map[string]any{"inline": map[string]any{"featureGates": g}} }
	svc := func(spec map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	}
	preset := Base{Name: "kserve/kserve-config-llm-scheduler", Spec: map[string]any{"router": map[string]any{"scheduler": map[string]any{}}}}
	for name, tc := range map[string]struct {
		svc       *unstructured.Unstructured
		bases     []Base
		on, known bool
	}{
		"own config with the gate":      {svc(config(gates("flowControl"))), []Base{preset}, true, true},
		"own config without it":         {svc(config(gates())), []Base{preset}, false, true},
		"KServe's default":              {svc(map[string]any{}), []Base{preset}, false, true},
		"a base config with the gate":   {svc(map[string]any{}), []Base{preset, {Name: "llm/fc", Spec: config(gates("flowControl"))}}, true, true},
		"own config over a base config": {svc(config(gates())), []Base{{Name: "llm/fc", Spec: config(gates("flowControl"))}}, false, true},
		"in a ConfigMap":                {svc(config(map[string]any{"ref": map[string]any{"name": "epp"}})), nil, false, false},
		"inline as text":                {svc(config(map[string]any{"inline": "featureGates: [flowControl]"})), nil, false, false},
		"a base config not read":        {svc(map[string]any{}), []Base{{Name: "llm/unknown"}}, false, false},
	} {
		if on, known := flowControl(tc.svc, tc.bases); on != tc.on || known != tc.known {
			t.Errorf("%s: on %v known %v, want %v %v", name, on, known, tc.on, tc.known)
		}
	}
}

// The sync warns about a pool of a best-effort class whose service runs
// without flow control, or with it but takes its regular tenants oldest
// first, and only about that pool.
func TestFlowControlWarning(t *testing.T) {
	gvr, _ := gvrFor(render.ObjectiveKind)
	fair := map[string]any{"defaultPriorityBand": map[string]any{"fairnessPolicyRef": "round-robin-fairness-policy"}}
	service := func(name, pool string, flow map[string]any, gates ...any) *unstructured.Unstructured {
		inline := map[string]any{"featureGates": gates, "plugins": []any{map[string]any{"type": "round-robin-fairness-policy"}}}
		if flow != nil {
			inline["flowControl"] = flow
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": llmServiceGVRs[0].Group + "/" + llmServiceGVRs[0].Version, "kind": "LLMInferenceService",
			"metadata": map[string]any{"name": name, "namespace": "llm-glm53"},
			"spec": map[string]any{"router": map[string]any{"scheduler": map[string]any{
				"config": map[string]any{"inline": inline}}}},
			"status": map[string]any{"router": map[string]any{"scheduler": map[string]any{"inferencePool": map[string]any{
				"group": "inference.networking.k8s.io", "name": pool}}}},
		}}
	}
	class := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "llm-d.ai/v1alpha2", "kind": render.ObjectiveKind,
		"metadata": map[string]any{"name": "best-effort", "namespace": "llm-glm53"},
		"spec": map[string]any{"priority": int64(-1),
			"poolRef": map[string]any{"group": "inference.networking.k8s.io", "kind": "InferencePool", "name": "glm53-b200-inference-pool"}},
	}}
	for name, tc := range map[string]struct {
		svc  *unstructured.Unstructured
		warn string
	}{
		"without flow control":            {service("glm53-b200", "glm53-b200-inference-pool", nil), "LLMInferenceService llm-glm53/glm53-b200 runs it without flow control"},
		"with flow control, oldest first": {service("glm53-b200", "glm53-b200-inference-pool", nil, "flowControl"), "queues priority 0 under global-strict-fairness-policy"},
		"with flow control, in turn":      {service("glm53-b200", "glm53-b200-inference-pool", fair, "flowControl"), ""},
		"another pool's service":          {service("glm53-h200", "glm53-h200-inference-pool", nil), ""},
	} {
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds(gvr, "InferenceObjectiveList"), tc.svc)
		var res SyncResult
		(&Client{dyn: dyn}).readGatewayStatus(context.Background(), "gw", []*unstructured.Unstructured{class}, nil, &res)
		ok := len(res.Warnings) == 0
		if tc.warn != "" {
			ok = len(res.Warnings) == 1 && strings.Contains(res.Warnings[0], tc.warn)
		}
		if !ok {
			t.Errorf("%s: warnings %v", name, res.Warnings)
		}
	}
}

// A scheduler takes the regular tenants in turn when the band of priority 0,
// its own or the default one, names a round-robin fairness plugin. A band
// without a fairness policy runs the default, which serves the oldest
// request first.
func TestTenantFairness(t *testing.T) {
	plugins := []any{map[string]any{"type": "round-robin-fairness-policy"}, map[string]any{"type": "round-robin-fairness-policy", "name": "fair"},
		map[string]any{"type": "global-strict-fairness-policy"}}
	config := func(flow map[string]any) map[string]any {
		return map[string]any{"plugins": plugins, "flowControl": flow}
	}
	band := func(priority int64, ref string) map[string]any {
		b := map[string]any{"priority": priority}
		if ref != "" {
			b["fairnessPolicyRef"] = ref
		}
		return b
	}
	for name, tc := range map[string]struct {
		config map[string]any
		fair   bool
	}{
		"no flow control section": {map[string]any{"plugins": plugins}, false},
		"default band in turn":    {config(map[string]any{"defaultPriorityBand": band(0, "round-robin-fairness-policy")}), true},
		"band 0 by plugin name":   {config(map[string]any{"priorityBands": []any{band(10, ""), band(0, "fair")}}), true},
		"band 0 strict":           {config(map[string]any{"priorityBands": []any{band(0, "global-strict-fairness-policy")}}), false},
		"band 0 without a policy": {config(map[string]any{"defaultPriorityBand": band(0, "fair"), "priorityBands": []any{band(0, "")}}), false},
		"only another band":       {config(map[string]any{"priorityBands": []any{band(-1, "fair")}}), false},
		"a plugin not declared":   {config(map[string]any{"defaultPriorityBand": band(0, "missing")}), false},
	} {
		if got := tenantFairness(tc.config); got != tc.fair {
			t.Errorf("%s: %v, want %v", name, got, tc.fair)
		}
	}
}
