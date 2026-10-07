package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// A server that ignores label selectors returns objects this tool does not
// own. Sync must delete only the ones carrying its label.
func TestSyncPrunesOnlyManagedObjects(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
		case strings.HasSuffix(r.URL.Path, "/aigatewayroutes"):
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[
				{"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"AIGatewayRoute","metadata":{"name":"theirs","namespace":"ns"}},
				{"apiVersion":"aigateway.envoyproxy.io/v1alpha1","kind":"AIGatewayRoute","metadata":{"name":"ours-stale","namespace":"ns","labels":{"app.kubernetes.io/managed-by":"aigw-ui"}}}
			]}`))
		case strings.Contains(r.URL.Path, "/secrets"):
			// The role on the cluster grants no list on Secrets, only get on this one name.
			if !strings.HasSuffix(r.URL.Path, "/secrets/aigw-ui-api-keys") {
				t.Errorf("unexpected Secret request %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		default:
			w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`))
		}
	}))
	defer srv.Close()

	client, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Sync(context.Background(), "ns", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/aigatewayroutes/ours-stale") || res.Pruned != 1 {
		t.Errorf("deleted %v (pruned %d), want only ours-stale", deleted, res.Pruned)
	}
}
