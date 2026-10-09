package kube

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"aigw-ui/internal/render"
)

// realClient returns a client for the API server in KUBE_TEST_KUBECONFIG and
// a namespace made for this test, or skips the test. Server-side apply is
// done by the API server, so what it does cannot be tested against a
// stand-in.
//
//	hack/test-apiserver.sh
//
// starts a throwaway API server and runs these tests. The namespace is
// deleted when the test ends, so tests do not see each other's objects and
// the server can be reused.
func realClient(t *testing.T) (*Client, string) {
	path := os.Getenv("KUBE_TEST_KUBECONFIG")
	if path == "" {
		t.Skip("KUBE_TEST_KUBECONFIG is not set")
	}
	kubeconfig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	c.settle = 0

	ctx := context.Background()
	namespaces := c.dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"})
	ns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"generateName": "aigw-ui-test-"},
	}}
	created, err := namespaces.Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := namespaces.Delete(ctx, created.GetName(), metav1.DeleteOptions{}); err != nil {
			t.Errorf("delete namespace %s: %v", created.GetName(), err)
		}
	})
	return c, created.GetName()
}

func secretEntries(t *testing.T, c *Client, namespace string) string {
	gvr, _ := gvrFor("Secret")
	live, err := c.dyn.Resource(gvr).Namespace(namespace).Get(context.Background(), render.KeysSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, _, _ := unstructured.NestedMap(live.Object, "data")
	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

func keysState(namespace string, clientIDs ...string) render.State {
	s := render.State{Namespace: namespace, GatewayName: "llm", AuthEnabled: true}
	for _, id := range clientIDs {
		s.Keys = append(s.Keys, render.Key{ClientID: id, Value: "sk-" + id})
	}
	return s
}

// syncKeys applies only the key Secret; the SecurityPolicy needs a CRD.
func syncKeys(t *testing.T, c *Client, namespace string, clientIDs ...string) SyncResult {
	res, err := c.Sync(context.Background(), namespace, render.Objects(keysState(namespace, clientIDs...))[:1], nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A revoked key must leave the Secret: the gateway trusts every entry in it.
func TestRealAPIServerRevokedKeyLeavesSecret(t *testing.T) {
	c, ns := realClient(t)

	syncKeys(t, c, ns, "team-a.01", "team-a.02", "team-b.03")
	if got := secretEntries(t, c, ns); got != "team-a.01 team-a.02 team-b.03" {
		t.Fatalf("after the first sync the Secret holds %q", got)
	}
	res := syncKeys(t, c, ns, "team-a.01")
	if got := secretEntries(t, c, ns); got != "team-a.01" {
		t.Errorf("after revoking two keys the Secret holds %q, want only team-a.01", got)
	}
	if len(res.Changes) != 1 || res.Changes[0].Action != "updated" {
		t.Errorf("changes = %+v, want the Secret updated", res.Changes)
	}
	if res := syncKeys(t, c, ns, "team-a.01"); len(res.Changes) != 0 {
		t.Errorf("a sync that changes nothing reported %+v", res.Changes)
	}
}

// A Secret written by a version before 0.6.1 holds entries that came in
// through stringData, which no apply can remove. The first sync must.
func TestRealAPIServerCleansSecretOfOlderVersion(t *testing.T) {
	c, ns := realClient(t)
	gvr, _ := gvrFor("Secret")
	ctx := context.Background()

	old := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"` + render.KeysSecretName + `","labels":{"app.kubernetes.io/managed-by":"aigw-ui"}},
		"type":"Opaque","stringData":{"team-a.01":"sk-team-a.01","team-a.02":"sk-revoked","team-b.03":"sk-disabled"}}`
	force := true
	if _, err := c.dyn.Resource(gvr).Namespace(ns).Patch(ctx, render.KeysSecretName, types.ApplyPatchType, []byte(old),
		metav1.PatchOptions{FieldManager: fieldManager, Force: &force}); err != nil {
		t.Fatal(err)
	}

	res := syncKeys(t, c, ns, "team-a.01")
	if got := secretEntries(t, c, ns); got != "team-a.01" {
		t.Errorf("the Secret still holds %q, want only team-a.01", got)
	}
	if len(res.Changes) != 1 || res.Changes[0].Action != "updated" {
		t.Errorf("changes = %+v, want the Secret updated", res.Changes)
	}
}
