package api

import (
	"encoding/json"
	"testing"

	"aigw-ui/internal/store"
)

// A PUT that leaves fields out must keep what is stored. Wiping gateway_url
// made discovery fall back to one namespace without any error.
func TestClusterUpdateKeepsOmittedFields(t *testing.T) {
	current := store.Cluster{
		ID: "c1", Name: "site1-a", Site: "site1", Namespace: "ai-gateway", GatewayName: "llm",
		AuthEnabled: true, GatewayURL: "https://gw.site1.example",
	}
	for name, tc := range map[string]struct {
		body string
		want store.Cluster
	}{
		"only site":         {`{"site":"site2"}`, with(current, func(c *store.Cluster) { c.Site = "site2" })},
		"required only":     {`{"name":"site1-a","namespace":"ai-gateway","gateway_name":"llm"}`, current},
		"clear url":         {`{"gateway_url":""}`, with(current, func(c *store.Cluster) { c.GatewayURL = "" })},
		"auth off":          {`{"auth_enabled":false}`, with(current, func(c *store.Cluster) { c.AuthEnabled = false })},
		"token without url": {`{"discovery_token":"t"}`, current},
	} {
		var b clusterBody
		if err := json.Unmarshal([]byte(tc.body), &b); err != nil {
			t.Fatal(err)
		}
		got, err := b.cluster(&current)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, tc.want)
		}
	}

	var b clusterBody
	json.Unmarshal([]byte(`{"gateway_url":"","discovery_token":"t"}`), &b)
	if _, err := b.cluster(&current); err == nil {
		t.Error("a token with the gateway URL removed should be rejected")
	}
	b = clusterBody{Name: "x", Namespace: "ns", GatewayName: "gw"}
	if _, err := b.cluster(nil); err == nil {
		t.Error("a new cluster without a kubeconfig should be rejected")
	}
}

func with(c store.Cluster, change func(*store.Cluster)) store.Cluster {
	change(&c)
	return c
}

// Changing a model's limit must not remove its manual endpoints.
func TestModelUpdateKeepsOmittedFields(t *testing.T) {
	current := store.Model{Name: "glm", DefaultLimit: 500, DefaultWindow: "1h", CostExpression: "output_tokens * 4u"}

	var b modelBody
	json.Unmarshal([]byte(`{"default_limit":900}`), &b)
	in, err := b.input(current.Name, &current)
	if err != nil {
		t.Fatal(err)
	}
	if !in.KeepEndpoints || in.DefaultLimit != 900 || in.DefaultWindow != "1h" || in.CostExpression != "output_tokens * 4u" {
		t.Errorf("got %+v, want the limit changed and everything else kept", in)
	}

	b = modelBody{}
	json.Unmarshal([]byte(`{"endpoints":[],"cost_expression":""}`), &b)
	in, err = b.input(current.Name, &current)
	if err != nil {
		t.Fatal(err)
	}
	if in.KeepEndpoints || in.CostExpression != "" || in.DefaultLimit != 500 {
		t.Errorf("got %+v, want endpoints and expression cleared and the limit kept", in)
	}

	b = modelBody{}
	in, err = b.input("new", nil)
	if err != nil || in.KeepEndpoints || in.DefaultLimit != 1 || in.DefaultWindow != "1d" {
		t.Errorf("create: got %+v, %v", in, err)
	}
}

func TestValidCostExpression(t *testing.T) {
	for _, ok := range []string{
		"", "total_tokens", "input_tokens + cached_input_tokens / 10u + output_tokens * 6u",
		"uint(double(cached_input_tokens) * 0.1) + (output_tokens*4u)",
	} {
		if err := validCostExpression(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"tokens", "input_tokens; drop", "(input_tokens", "input_tokens)", `model == "x"`, "Input_tokens"} {
		if err := validCostExpression(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// A fleet cluster without key enforcement would let a client name its own
// tenant, so the two settings cannot be combined either way round.
func TestFleetClusterMustEnforceKeys(t *testing.T) {
	enforcing := store.Cluster{ID: "c1", Name: "site1-a", Namespace: "ai-gateway", GatewayName: "llm", AuthEnabled: true}
	open := with(enforcing, func(c *store.Cluster) { c.AuthEnabled = false })
	fleet := with(enforcing, func(c *store.Cluster) { c.FleetEnabled = true })
	for name, tc := range map[string]struct {
		current store.Cluster
		body    string
		ok      bool
	}{
		"join the fleet with keys enforced": {enforcing, `{"fleet_enabled":true,"client_listener":"https"}`, true},
		"join the fleet without":            {open, `{"fleet_enabled":true}`, false},
		"join and enforce in one change":    {open, `{"fleet_enabled":true,"auth_enabled":true}`, true},
		"stop enforcing while in the fleet": {fleet, `{"auth_enabled":false}`, false},
		"leave the fleet and stop":          {fleet, `{"auth_enabled":false,"fleet_enabled":false}`, true},
		"bad listener name":                 {enforcing, `{"client_listener":"HTTPS listener"}`, false},
	} {
		var b clusterBody
		if err := json.Unmarshal([]byte(tc.body), &b); err != nil {
			t.Fatal(err)
		}
		if _, err := b.cluster(&tc.current); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok = %v", name, err, tc.ok)
		}
	}
}

func TestRecipeWarnings(t *testing.T) {
	site := func(name, revision, length string, serving bool) store.Endpoint {
		return store.Endpoint{ClusterName: name, Capacity: store.EndpointCapacity{Serving: serving, Revision: revision, MaxModelLen: length}}
	}
	same := store.Model{Endpoints: []store.Endpoint{site("a", "r1", "262144", true), site("b", "r1", "262144", true), site("c", "", "", true)}}
	if got := recipeWarnings(same); len(got) != 0 {
		t.Errorf("same recipe: %v", got)
	}
	// A site that no longer serves the model does not count.
	gone := store.Model{Endpoints: []store.Endpoint{site("a", "r1", "262144", true), site("b", "r0", "131072", false)}}
	if got := recipeWarnings(gone); len(got) != 0 {
		t.Errorf("site not serving: %v", got)
	}
	differ := store.Model{Endpoints: []store.Endpoint{site("a", "r1", "262144", true), site("b", "r2", "131072", true)}}
	if got := recipeWarnings(differ); len(got) != 2 {
		t.Errorf("different recipes: %v", got)
	}
}
