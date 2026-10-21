package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"

	"aigw-ui/internal/store"
)

// Claude Code gets the model an operator chose when the tenant may call it,
// whatever its case; otherwise the priciest of the tenant's models for its
// work and the cheapest for its background tasks, by output price, then
// input price. A model without prices comes after every priced one.
func TestChooseModel(t *testing.T) {
	priced := func(name string, input, output int64) store.Model {
		return store.Model{Name: name, Prices: &store.Price{Input: input, Output: output}}
	}
	models := []store.Model{
		{Name: "unpriced"},
		priced("glm", 60_000, 220_000),
		priced("qwen-small", 5_000, 20_000),
		priced("qwen-big", 80_000, 220_000),
	}
	for _, tc := range []struct {
		chosen string
		order  func(a, b store.Model) int
		want   string
	}{
		{"", priciest, "qwen-big"},
		{"", cheapest, "qwen-small"},
		{"GLM", priciest, "glm"},
		{"opus", priciest, "qwen-big"},
		{"unpriced", cheapest, "unpriced"},
	} {
		if got := chooseModel(models, tc.chosen, tc.order); got != tc.want {
			t.Errorf("chosen %q: %s, want %s", tc.chosen, got, tc.want)
		}
	}
	if got := chooseModel([]store.Model{{Name: "b"}, {Name: "a"}}, "", priciest); got != "a" {
		t.Errorf("no prices: %s, want the first by name", got)
	}
}

// The length is the least any deployment declares, a site listing several
// when its deployments differ; 0 when none declares one.
func TestDeclaredLength(t *testing.T) {
	m := store.Model{Endpoints: []store.Endpoint{
		{Capacity: store.EndpointCapacity{MaxModelLen: "131072, 65536"}},
		{Capacity: store.EndpointCapacity{MaxModelLen: ""}},
		{Capacity: store.EndpointCapacity{MaxModelLen: "98304"}},
	}}
	if got := declaredLength(m); got != 65536 {
		t.Errorf("length %d, want 65536", got)
	}
	if got := declaredLength(store.Model{Endpoints: []store.Endpoint{{}}}); got != 0 {
		t.Errorf("none declared: %d", got)
	}
}

// A tenant's clients get the gateway's addresses and the models it may call
// that are served somewhere, with prices in dollars per million tokens, and
// Claude Code's models among them. Without the gateway's address, none.
func TestMyModels(t *testing.T) {
	st, ctx := openStore(t)
	site, err := st.CreateCluster(ctx, store.Cluster{Name: "site-a", Namespace: "ai-gateway", GatewayName: "gw"}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, name := range []string{"big", "small", "unserved"} {
		in := store.ModelInput{Name: name, DefaultLimit: 1, DefaultWindow: "1d"}
		if name != "unserved" {
			in.Endpoints = []store.Endpoint{{ClusterID: site.ID, Host: name + ".models.svc", Port: 8000, UpstreamModel: name}}
		}
		id, err := st.CreateModel(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	for name, p := range map[string]store.Price{
		"big":   {Input: 300_000, Cached: 30_000, Output: 1_500_000},
		"small": {Input: 10_000, Cached: 1_000, Output: 40_000},
	} {
		if _, err := st.SetPendingPrice(ctx, ids[name], p, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.ApplyDuePrices(ctx); err != nil {
		t.Fatal(err)
	}
	tenant, err := st.CreateTenant(ctx, "team-a", "")
	if err != nil {
		t.Fatal(err)
	}
	get := func(s *Server, tenant store.Tenant) myClients {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/v1/my/models", nil)
		s.getMyModels(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, tenant)))
		var out myClients
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return out
	}

	s := (&Server{st: st}).WithGatewayURL("https://llm.example.com")
	out := get(s, tenant)
	want := myClients{
		GatewayURL: "https://llm.example.com", AnthropicBaseURL: "https://llm.example.com/anthropic", OpenAIBaseURL: "https://llm.example.com/v1",
		ClaudeCode: &myClaudeCode{Model: "big", SmallModel: "small"},
		Models: []myClientModel{
			{Name: "big", Prices: &myDollarPrices{Input: 3, Cached: 0.3, Output: 15}},
			{Name: "small", Prices: &myDollarPrices{Input: 0.1, Cached: 0.01, Output: 0.4}},
		},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("got %+v\nwant %+v", out, want)
	}

	// The operator's choice, where the tenant may call it.
	s.WithClaudeCode("small", "")
	if out := get(s, tenant); out.ClaudeCode == nil || *out.ClaudeCode != (myClaudeCode{Model: "small", SmallModel: "small"}) {
		t.Errorf("chosen: %+v", out.ClaudeCode)
	}
	held, err := st.SetTenantModels(ctx, tenant.ID, []string{ids["big"]})
	if err != nil {
		t.Fatal(err)
	}
	if out := get(s, held); len(out.Models) != 1 || out.Models[0].Name != "big" || *out.ClaudeCode != (myClaudeCode{Model: "big", SmallModel: "big"}) {
		t.Errorf("held to big: %+v %+v", out.Models, out.ClaudeCode)
	}
	none, err := st.SetTenantModels(ctx, tenant.ID, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if out := get(&Server{st: st}, none); out.ClaudeCode != nil || len(out.Models) != 0 || out.GatewayURL != "" || out.AnthropicBaseURL != "" {
		t.Errorf("no model, no address: %+v", out)
	}
}
