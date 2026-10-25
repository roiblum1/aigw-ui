package sso

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"aigw-ui/internal/store"
)

const (
	saToken   = "sa-token"
	userToken = "sha256~alice"
	clientID  = "system:serviceaccount:aigw-ui:aigw-ui"
)

// cluster is a stand-in for an OpenShift cluster: its API (the OAuth
// metadata, users/~ and groups) and its OAuth server's token endpoint.
// setDown changes whether the groups can be listed.
type cluster struct {
	srv *httptest.Server
	mu  sync.Mutex
	// groups is what the API lists; down makes the list fail.
	groups map[string][]string
	down   bool
	// challenge is the last authorization's code_challenge, as the OAuth
	// server would keep it with the code.
	challenge string
	tokenHits int
}

func (c *cluster) setDown(down bool) {
	c.mu.Lock()
	c.down = down
	c.mu.Unlock()
}

func newCluster(t *testing.T) (*cluster, Config) {
	t.Helper()
	c := &cluster{groups: map[string][]string{"ldap-team-a": {"alice", "bob"}, "ldap-team-b": {"bob"}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           c.srv.URL,
			"authorization_endpoint":           c.srv.URL + "/oauth/authorize",
			"token_endpoint":                   c.srv.URL + "/oauth/token",
			"code_challenge_methods_supported": []string{"plain", "S256"},
		})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.tokenHits++
		challenge := c.challenge
		c.mu.Unlock()
		r.ParseForm()
		f := r.PostForm
		if f.Get("grant_type") != "authorization_code" || f.Get("client_id") != clientID || f.Get("client_secret") != saToken ||
			f.Get("code") != "the-code" || f.Get("redirect_uri") != "https://hub.example.com/sso/callback" ||
			(challenge != "" && Challenge(f.Get("code_verifier")) != challenge) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "no"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": userToken, "token_type": "Bearer", "expires_in": 86400})
	})
	mux.HandleFunc("GET /apis/user.openshift.io/v1/users/~", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+userToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"kind": "User", "metadata": map[string]string{"name": "alice"}})
	})
	mux.HandleFunc("GET /apis/user.openshift.io/v1/groups", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.down || r.Header.Get("Authorization") != "Bearer "+saToken {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"groups.user.openshift.io is forbidden"}`))
			return
		}
		var items []any
		for name, users := range c.groups {
			items = append(items, map[string]any{"metadata": map[string]string{"name": name}, "users": users})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	c.srv = httptest.NewTLSServer(mux)
	t.Cleanup(c.srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(saToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := c.srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return c, Config{APIServer: c.srv.URL, ClientID: clientID, TokenFile: tokenFile, RootCAs: pool}
}

// A sign-in: the browser is sent to the OAuth server for user:info with
// PKCE, the code comes back and is traded with the ServiceAccount's token as
// the client's secret, and the access token names the person.
func TestSignIn(t *testing.T) {
	c, cfg := newCluster(t)
	o := New(cfg)
	ctx := context.Background()
	redirect := "https://hub.example.com/sso/callback"
	verifier := "a-verifier-of-enough-length-for-pkce-0123456789"
	u, pkce, err := o.AuthorizeURL(ctx, redirect, "the-state", verifier)
	if err != nil || !pkce {
		t.Fatalf("%s %v %v", u, pkce, err)
	}
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	if parsed.Path != "/oauth/authorize" || q.Get("client_id") != clientID || q.Get("scope") != "user:info" || q.Get("response_type") != "code" ||
		q.Get("redirect_uri") != redirect || q.Get("state") != "the-state" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != Challenge(verifier) {
		t.Fatalf("authorize URL %s", u)
	}
	c.mu.Lock()
	c.challenge = q.Get("code_challenge")
	c.mu.Unlock()
	if _, err := o.Exchange(ctx, "the-code", redirect, "another-verifier"); err == nil {
		t.Fatal("a wrong verifier was taken")
	}
	if _, err := o.Exchange(ctx, "a-wrong-code", redirect, verifier); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("a wrong code: %v", err)
	}
	token, err := o.Exchange(ctx, "the-code", redirect, verifier)
	if err != nil || token != userToken {
		t.Fatalf("%q %v", token, err)
	}
	if user, err := o.User(ctx, token); err != nil || user != "alice" {
		t.Fatalf("%q %v", user, err)
	}
	if _, err := o.User(ctx, "sha256~nobody"); err == nil {
		t.Fatal("a token the cluster does not know named someone")
	}

	// A token URL of its own replaces the one the metadata names.
	cfg.TokenURL = c.srv.URL + "/elsewhere/token"
	if _, err := New(cfg).Exchange(ctx, "the-code", redirect, verifier); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("the token URL of its own was not used: %v", err)
	}
}

// The Groups are read with the hub's account. Before a read worked the hub
// knows nobody's groups; a read that fails keeps what the last one found.
func TestGroups(t *testing.T) {
	c, cfg := newCluster(t)
	o := New(cfg)
	ctx := context.Background()
	c.setDown(true)
	if err := o.RefreshGroups(ctx); err == nil {
		t.Fatal("a refused read worked")
	}
	if _, err := o.GroupsOf("bob"); !errors.Is(err, ErrNoGroups) || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("before any read: %v", err)
	}
	c.setDown(false)
	if err := o.RefreshGroups(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := o.GroupsOf("bob"); !reflect.DeepEqual(got, []string{"ldap-team-a", "ldap-team-b"}) {
		t.Errorf("bob's groups = %v", got)
	}
	if got, _ := o.GroupsOf("carol"); len(got) != 0 {
		t.Errorf("carol's groups = %v", got)
	}
	c.setDown(true)
	if err := o.RefreshGroups(ctx); err == nil || o.LastGroupsError() == nil {
		t.Fatal("the failed read is not told")
	}
	if got, _ := o.GroupsOf("alice"); !reflect.DeepEqual(got, []string{"ldap-team-a"}) {
		t.Errorf("after a failed read, alice's groups = %v", got)
	}
}

// A sealed value comes back as it was, and not when changed, sealed for
// another purpose, expired, or sealed with another key.
func TestSealer(t *testing.T) {
	s := NewSealer([]byte("an encryption key of 32 bytes..."))
	now := time.Now()
	v, err := s.Seal("session", map[string]string{"user": "alice"}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if !s.Open("session", v, &got, now) || got["user"] != "alice" {
		t.Fatalf("%v", got)
	}
	body, sig, _ := strings.Cut(v, ".")
	other, _ := s.Seal("session", map[string]string{"user": "mallory"}, now.Add(time.Hour))
	otherBody, _, _ := strings.Cut(other, ".")
	for name, bad := range map[string]bool{
		"another body":    s.Open("session", otherBody+"."+sig, &got, now),
		"no signature":    s.Open("session", body, &got, now),
		"another purpose": s.Open("state", v, &got, now),
		"expired":         s.Open("session", v, &got, now.Add(2*time.Hour)),
		"another key":     NewSealer([]byte("another key")).Open("session", v, &got, now),
	} {
		if bad {
			t.Errorf("%s: opened", name)
		}
	}
}

type fakeStore struct {
	owned   []store.OwnedKeyInGroup
	revoked []string
}

func (f *fakeStore) OwnedKeysInGroups(context.Context) ([]store.OwnedKeyInGroup, error) {
	var live []store.OwnedKeyInGroup
	for _, o := range f.owned {
		gone := false
		for _, id := range f.revoked {
			gone = gone || id == o.Key.ID
		}
		if !gone {
			live = append(live, o)
		}
	}
	return live, nil
}

func (f *fakeStore) RevokeKey(_ context.Context, id string) (store.APIKey, error) {
	f.revoked = append(f.revoked, id)
	for _, o := range f.owned {
		if o.Key.ID == id {
			return o.Key, nil
		}
	}
	return store.APIKey{}, store.ErrNotFound
}

// Someone who left the tenant's group loses their key, once; one whose
// group the cluster no longer lists keeps it, and nothing is revoked while
// the Groups cannot be read.
func TestWatcherRevokesWhoLeft(t *testing.T) {
	c, cfg := newCluster(t)
	owned := func(id, owner, tenant, group string) store.OwnedKeyInGroup {
		return store.OwnedKeyInGroup{Key: store.APIKey{ID: id, Owner: owner, ClientID: tenant + "." + id}, TenantSlug: tenant, Group: group}
	}
	st := &fakeStore{owned: []store.OwnedKeyInGroup{
		owned("k1", "alice", "team-a", "ldap-team-a"),
		owned("k2", "bob", "team-b", "ldap-team-b"),
		owned("k3", "carol", "team-c", "ldap-gone"),
	}}
	var syncs []string
	w := NewWatcher(New(cfg), st, func(_ context.Context, action, summary string) { syncs = append(syncs, action+": "+summary) }, time.Minute)
	ctx := context.Background()
	if got, err := w.Tick(ctx); err != nil || len(got) != 0 || len(syncs) != 0 {
		t.Fatalf("everyone in their group: %v %v %v", got, err, syncs)
	}
	c.mu.Lock()
	c.groups["ldap-team-b"] = []string{"dave"}
	c.mu.Unlock()
	got, err := w.Tick(ctx)
	if err != nil || !reflect.DeepEqual(got, []string{"team-b.k2"}) || !reflect.DeepEqual(st.revoked, []string{"k2"}) {
		t.Fatalf("revoked %v (%v), store %v", got, err, st.revoked)
	}
	if !reflect.DeepEqual(syncs, []string{"key.revoke: Revoked the personal keys of bob (team-b, left ldap-team-b)"}) {
		t.Errorf("syncs = %v", syncs)
	}
	if got, _ := w.Tick(ctx); len(got) != 0 {
		t.Errorf("revoked again: %v", got)
	}
	c.mu.Lock()
	c.down = true
	c.groups["ldap-team-a"] = nil
	c.mu.Unlock()
	if got, err := w.Tick(ctx); err == nil || len(got) != 0 {
		t.Errorf("the Groups could not be read, yet: %v %v", got, err)
	}
}

// Outside a pod the API server, the token and the namespace are given; the
// client ID is the ServiceAccount's in that namespace. A CA file without a
// certificate is refused.
func TestInClusterWithOptions(t *testing.T) {
	cfg, err := InCluster(Options{ServiceAccount: "aigw-ui", Namespace: "hub", APIServer: "https://api.example.com:6443/", TokenFile: "/tmp/token"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != "system:serviceaccount:hub:aigw-ui" || cfg.APIServer != "https://api.example.com:6443" || cfg.TokenFile != "/tmp/token" || cfg.RootCAs == nil {
		t.Fatalf("%+v", cfg)
	}
	bad := filepath.Join(t.TempDir(), "ca.crt")
	os.WriteFile(bad, []byte("not a certificate"), 0o600)
	if _, err := InCluster(Options{ServiceAccount: "aigw-ui", Namespace: "hub", CAFile: bad}); err == nil {
		t.Fatal("a CA file without a certificate was taken")
	}
}
