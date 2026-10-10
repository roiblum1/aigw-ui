package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"

	"aigw-ui/internal/render"
)

func service(t *testing.T, body string) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(body), &obj.Object); err != nil {
		t.Fatal(err)
	}
	return obj
}

// The pool is the one KServe reports, then the one the spec names, then the
// one KServe creates. A service without a scheduler has none.
func TestPoolOfAService(t *testing.T) {
	meta := `"metadata":{"name":"glm","namespace":"glm-5-3"}`
	for _, tc := range []struct {
		name, body string
		want       Pool
		ok         bool
	}{
		{"no scheduler", `{` + meta + `,"spec":{"router":{"route":{}}}}`, Pool{}, false},
		{"created by KServe", `{` + meta + `,"spec":{"router":{"scheduler":{}}}}`,
			Pool{Namespace: "glm-5-3", Name: "glm-inference-pool", Group: "inference.networking.k8s.io"}, true},
		{"named in the spec", `{` + meta + `,"spec":{"router":{"scheduler":{"pool":{"ref":{"name":"shared"}}}}}}`,
			Pool{Namespace: "glm-5-3", Name: "shared", Group: "inference.networking.k8s.io"}, true},
		{"reported in the status", `{` + meta + `,"spec":{"router":{"scheduler":{"pool":{"ref":{"name":"shared"}}}}},
			"status":{"router":{"scheduler":{"inferencePool":{"group":"inference.networking.x-k8s.io","kind":"InferencePool","name":"observed"}}}}}`,
			Pool{Namespace: "glm-5-3", Name: "observed", Group: "inference.networking.x-k8s.io"}, true},
	} {
		got, ok := pool(service(t, tc.body))
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: pool = %+v, %v, want %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// A cluster without the InferenceObjective kind: the class is left out and
// reported, and the sync does not fail over it.
func TestSyncSkipsAClassTheClusterHasNoKindFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/inferenceobjectives") || strings.Contains(r.URL.Path, "/secrets") {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
			return
		}
		w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`))
	}))
	defer srv.Close()
	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	state := render.State{Namespace: "ai-gateway", Models: []render.Model{{
		Name: "glm", Slug: "glm", BestEffortPools: []render.Pool{{Namespace: "glm-5-3", Name: "glm-inference-pool", Group: "inference.networking.k8s.io"}},
	}}}
	var class []*unstructured.Unstructured
	for _, o := range render.Objects(state) {
		if o.GetKind() == render.ObjectiveKind {
			class = append(class, o)
		}
	}
	res, err := client.Sync(context.Background(), "ai-gateway", class, nil)
	if err != nil {
		t.Fatalf("the sync failed over a kind the cluster does not have: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "InferenceObjective glm-5-3/best-effort" || res.Applied != 0 {
		t.Errorf("skipped = %v, applied = %d", res.Skipped, res.Applied)
	}
}
