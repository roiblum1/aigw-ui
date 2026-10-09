package kube

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"aigw-ui/internal/render"
)

// realClient returns a client for the API server in KUBE_TEST_KUBECONFIG, or
// skips the test. Server-side apply is done by the API server, so what it
// does to a Secret cannot be tested against a stand-in.
//
//	KUBE_TEST_KUBECONFIG=/path/to/kubeconfig go test ./internal/kube/ -run RealAPIServer
//
// The namespace "aigw-ui-test" must exist. Use a throwaway cluster.
func realClient(t *testing.T) *Client {
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
	return c
}

const testNamespace = "aigw-ui-test"

func secretEntries(t *testing.T, c *Client) string {
	gvr, _ := gvrFor("Secret")
	live, err := c.dyn.Resource(gvr).Namespace(testNamespace).Get(context.Background(), render.KeysSecretName, metav1.GetOptions{})
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

func keysState(clientIDs ...string) render.State {
	s := render.State{Namespace: testNamespace, GatewayName: "llm", AuthEnabled: true}
	for _, id := range clientIDs {
		s.Keys = append(s.Keys, render.Key{ClientID: id, Value: "sk-" + id})
	}
	return s
}

// syncKeys applies only the key Secret; the SecurityPolicy needs a CRD.
func syncKeys(t *testing.T, c *Client, clientIDs ...string) SyncResult {
	res, err := c.Sync(context.Background(), testNamespace, render.Objects(keysState(clientIDs...))[:1])
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// A revoked key must leave the Secret: the gateway trusts every entry in it.
func TestRealAPIServerRevokedKeyLeavesSecret(t *testing.T) {
	c := realClient(t)
	gvr, _ := gvrFor("Secret")
	c.dyn.Resource(gvr).Namespace(testNamespace).Delete(context.Background(), render.KeysSecretName, metav1.DeleteOptions{})

	syncKeys(t, c, "team-a.01", "team-a.02", "team-b.03")
	if got := secretEntries(t, c); got != "team-a.01 team-a.02 team-b.03" {
		t.Fatalf("after the first sync the Secret holds %q", got)
	}
	res := syncKeys(t, c, "team-a.01")
	if got := secretEntries(t, c); got != "team-a.01" {
		t.Errorf("after revoking two keys the Secret holds %q, want only team-a.01", got)
	}
	if len(res.Changes) != 1 || res.Changes[0].Action != "updated" {
		t.Errorf("changes = %+v, want the Secret updated", res.Changes)
	}
	if res := syncKeys(t, c, "team-a.01"); len(res.Changes) != 0 {
		t.Errorf("a sync that changes nothing reported %+v", res.Changes)
	}
}

// A Secret written by a version before 0.6.1 holds entries that came in
// through stringData, which no apply can remove. The first sync must.
func TestRealAPIServerCleansSecretOfOlderVersion(t *testing.T) {
	c := realClient(t)
	gvr, _ := gvrFor("Secret")
	ctx := context.Background()
	c.dyn.Resource(gvr).Namespace(testNamespace).Delete(ctx, render.KeysSecretName, metav1.DeleteOptions{})

	old := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"` + render.KeysSecretName + `","labels":{"app.kubernetes.io/managed-by":"aigw-ui"}},
		"type":"Opaque","stringData":{"team-a.01":"sk-team-a.01","team-a.02":"sk-revoked","team-b.03":"sk-disabled"}}`
	force := true
	if _, err := c.dyn.Resource(gvr).Namespace(testNamespace).Patch(ctx, render.KeysSecretName, types.ApplyPatchType, []byte(old),
		metav1.PatchOptions{FieldManager: fieldManager, Force: &force}); err != nil {
		t.Fatal(err)
	}

	res := syncKeys(t, c, "team-a.01")
	if got := secretEntries(t, c); got != "team-a.01" {
		t.Errorf("the Secret still holds %q, want only team-a.01", got)
	}
	if len(res.Changes) != 1 || res.Changes[0].Action != "updated" {
		t.Errorf("changes = %+v, want the Secret updated", res.Changes)
	}
}
