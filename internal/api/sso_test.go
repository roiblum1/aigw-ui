package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aigw-ui/internal/sso"
	"aigw-ui/internal/store"
	"aigw-ui/internal/syncer"
)

// openShift is a stand-in for the hub's cluster: the OAuth server, which
// takes the code "the-code" and names alice, and the Groups.
type openShift struct {
	srv    *httptest.Server
	mu     sync.Mutex
	groups map[string][]string
}

func newOpenShift(t *testing.T) (*openShift, sso.Config) {
	t.Helper()
	o := &openShift{groups: map[string][]string{"ldap-team-a": {"alice", "bob"}, "ldap-team-c": {"carol"}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"authorization_endpoint": o.srv.URL + "/oauth/authorize",
			"token_endpoint": o.srv.URL + "/oauth/token", "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != "the-code" || r.FormValue("code_verifier") == "" || r.FormValue("client_secret") != "sa-token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "sha256~alice"})
	})
	mux.HandleFunc("GET /apis/user.openshift.io/v1/users/~", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"name": "alice"}})
	})
	mux.HandleFunc("GET /apis/user.openshift.io/v1/groups", func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		var items []any
		for name, users := range o.groups {
			items = append(items, map[string]any{"metadata": map[string]string{"name": name}, "users": users})
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	o.srv = httptest.NewTLSServer(mux)
	t.Cleanup(o.srv.Close)
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("sa-token"), 0o600)
	return o, sso.Config{APIServer: o.srv.URL, ClientID: "system:serviceaccount:aigw-ui:aigw-ui", TokenFile: token,
		RootCAs: o.srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
}

type browser struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]*http.Cookie
}

func (b *browser) do(method, path, team string, body string) *httptest.ResponseRecorder {
	b.t.Helper()
	r := httptest.NewRequest(method, "https://hub.example.com"+path, strings.NewReader(body))
	r.Header.Set("X-Forwarded-Proto", "https")
	if team != "" {
		r.Header.Set(teamHeader, team)
	}
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return w
}

// A person signs in with OpenShift, sees the tenant their group is linked
// to and no other, makes a key of their own, which a second one replaces,
// and sees every key's use; a key sign-in sees none of that. Without the
// Groups read, nobody is let in.
func TestSignInWithOpenShift(t *testing.T) {
	st, ctx := openStore(t)
	cluster, cfg := newOpenShift(t)
	o := sso.New(cfg)
	s := (&Server{st: st, sy: syncer.New(st, false, time.Hour, time.Hour), tenantPage: true}).WithSSO(o, sso.NewSealer(make([]byte, 32)))
	h := s.Handler()
	a, err := st.CreateTenantInGroup(ctx, "team-a", "Team A", "ldap-team-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTenantInGroup(ctx, "team-c", "Team C", "ldap-team-c"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTenant(ctx, "plain", "No group"); err != nil {
		t.Fatal(err)
	}
	_, adminKey, err := st.CreatePersonalKey(ctx, a.ID, "build server")
	if err != nil {
		t.Fatal(err)
	}
	b := &browser{t: t, h: h, cookies: map[string]*http.Cookie{}}

	if w := b.do("GET", "/api/v1/sso", "", ""); !strings.Contains(w.Body.String(), `"enabled":true`) || !strings.Contains(w.Body.String(), `"user":""`) {
		t.Fatalf("before signing in: %s", w.Body)
	}
	w := b.do("GET", "/sso/login", "", "")
	loc, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusFound || !strings.HasPrefix(loc.String(), cluster.srv.URL+"/oauth/authorize") ||
		loc.Query().Get("redirect_uri") != "https://hub.example.com/sso/callback" || b.cookies[signInCookie] == nil {
		t.Fatalf("login: %d %s %v", w.Code, loc, b.cookies)
	}
	state := loc.Query().Get("state")
	// Another state is not this sign-in.
	saved := b.cookies[signInCookie]
	if w := b.do("GET", "/sso/callback?code=the-code&state=other", "", ""); !strings.Contains(w.Header().Get("Location"), "sso_error=") || b.cookies[sessionCookie] != nil {
		t.Fatalf("another state: %s", w.Header().Get("Location"))
	}
	b.cookies[signInCookie] = saved
	if w := b.do("GET", "/sso/callback?code=the-code&state="+url.QueryEscape(state), "", ""); w.Header().Get("Location") != "/my-usage" || b.cookies[sessionCookie] == nil {
		t.Fatalf("callback: %d %s", w.Code, w.Header().Get("Location"))
	}
	if c := b.cookies[sessionCookie]; !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie %+v", c)
	}

	// The Groups not read yet: nobody is let in.
	if w := b.do("GET", "/api/v1/my/key", "", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("before reading the Groups: %d %s", w.Code, w.Body)
	}
	if err := o.RefreshGroups(ctx); err != nil {
		t.Fatal(err)
	}
	w = b.do("GET", "/api/v1/sso", "", "")
	var status struct {
		User  string    `json:"user"`
		Teams []ssoTeam `json:"teams"`
	}
	json.Unmarshal(w.Body.Bytes(), &status)
	if status.User != "alice" || len(status.Teams) != 1 || status.Teams[0].Slug != "team-a" {
		t.Fatalf("signed in: %s", w.Body)
	}
	if w := b.do("GET", "/api/v1/my/key", "", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"key":null`) {
		t.Fatalf("no key yet: %d %s", w.Code, w.Body)
	}
	// alice is in a tenant's group, just not this one's: the answer says so
	// (seen live: it said she was in no tenant's group at all).
	if w := b.do("GET", "/api/v1/my/key", "team-c", ""); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not linked to any of your groups") {
		t.Fatalf("another group's tenant: %d %s", w.Code, w.Body)
	}
	if w := b.do("GET", "/api/v1/my/key", "plain", ""); w.Code != http.StatusForbidden {
		t.Fatalf("a tenant without a group: %d %s", w.Code, w.Body)
	}
	if w := b.do("POST", "/api/v1/my/key", "", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("a key without naming the tenant: %d %s", w.Code, w.Body)
	}
	var made struct {
		Key    store.APIKey `json:"key"`
		Secret string       `json:"secret"`
	}
	w = b.do("POST", "/api/v1/my/key", "team-a", "")
	json.Unmarshal(w.Body.Bytes(), &made)
	if w.Code != http.StatusCreated || made.Key.Owner != "alice" || !strings.HasPrefix(made.Secret, store.PersonalKeyPrefix) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("my key: %d %s", w.Code, w.Body)
	}
	first := made.Key
	w = b.do("POST", "/api/v1/my/key", "team-a", "")
	json.Unmarshal(w.Body.Bytes(), &made)
	if made.Key.ID == first.ID {
		t.Fatal("the second key is the first")
	}
	if w := b.do("GET", "/api/v1/my/key", "team-a", ""); !strings.Contains(w.Body.String(), made.Key.ID) {
		t.Fatalf("my key now: %s", w.Body)
	}
	// One task-log entry per key made (seen live: two for each).
	tasks, err := st.ListTasks(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	added := 0
	for _, task := range tasks {
		if task.Action == "key.add" {
			added++
		}
	}
	if added != 2 {
		t.Errorf("%d key.add tasks for two keys", added)
	}

	// Every key's use: alice's live key, marked hers, and the admin's.
	if err := st.RecordUsage(ctx, func(store.Before) (store.Look, error) {
		return store.Look{Tokens: []store.TokenDelta{{UsageKey: store.UsageKey{TenantID: a.ID, ModelID: model(t, st, ctx), KeyID: made.Key.ID},
			Hour: time.Now(), Requests: 3, Input: 300, Output: 30}}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	w = b.do("GET", "/api/v1/my/people?days=1", "team-a", "")
	var people struct {
		Keys []keyUsePerson `json:"keys"`
	}
	json.Unmarshal(w.Body.Bytes(), &people)
	if w.Code != http.StatusOK || len(people.Keys) != 2 {
		t.Fatalf("people: %d %s", w.Code, w.Body)
	}
	for _, p := range people.Keys {
		switch p.Name {
		case "alice":
			if !p.Me || p.Totals.Requests != 3 || p.Totals.Output != 30 || len(p.Models) != 1 {
				t.Errorf("alice: %+v", p)
			}
		case "build server":
			if p.Me || p.Totals.Requests != 0 {
				t.Errorf("the admin's key: %+v", p)
			}
		default:
			t.Errorf("a key listed: %+v", p)
		}
	}
	if w := b.do("GET", "/api/v1/my/people?days=0", "team-a", ""); w.Code != http.StatusBadRequest {
		t.Errorf("days 0: %d", w.Code)
	}

	// A key sign-in sees its tenant's usage, but no one's keys.
	r := httptest.NewRequest("GET", "/api/v1/my/people", nil)
	r.Header.Set("Authorization", "Bearer "+adminKey)
	kw := httptest.NewRecorder()
	h.ServeHTTP(kw, r)
	if kw.Code != http.StatusForbidden {
		t.Fatalf("a key sign-in: %d %s", kw.Code, kw.Body)
	}

	// Signed out: nothing.
	if w := b.do("POST", "/api/v1/sso/logout", "", ""); w.Code != http.StatusNoContent || b.cookies[sessionCookie] != nil {
		t.Fatalf("logout: %d %v", w.Code, b.cookies)
	}
	if w := b.do("GET", "/api/v1/my/key", "team-a", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("after signing out: %d", w.Code)
	}
}

func model(t *testing.T, st *store.Store, ctx context.Context) string {
	t.Helper()
	ms, _ := st.ListModels(ctx)
	if len(ms) > 0 {
		return ms[0].ID
	}
	id, err := st.CreateModel(ctx, store.ModelInput{Name: "glm", DefaultLimit: 1, DefaultWindow: "1d"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A tenant is linked to a group when made or later, to one no other tenant
// has, and unlinked with "". An admin sees what each of its keys used.
func TestTenantGroupByAdmin(t *testing.T) {
	st, ctx := openStore(t)
	s := &Server{st: st, sy: syncer.New(st, false, time.Hour, time.Hour), adminToken: "admin-token-of-length"}
	h := s.Handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer admin-token-of-length")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := call("POST", "/api/v1/tenants", `{"slug":"team-a","display_name":"A","sso_group":" Data Science "}`)
	var a store.Tenant
	json.Unmarshal(w.Body.Bytes(), &a)
	if w.Code != http.StatusCreated || a.SSOGroup != "Data Science" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := call("POST", "/api/v1/tenants", `{"slug":"team-b","sso_group":"Data Science"}`); w.Code != http.StatusConflict {
		t.Fatalf("the same group twice: %d %s", w.Code, w.Body)
	}
	if w := call("POST", "/api/v1/tenants", `{"slug":"team-b","sso_group":"a/b"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a group name OpenShift does not take: %d %s", w.Code, w.Body)
	}
	w = call("POST", "/api/v1/tenants", `{"slug":"team-b"}`)
	var b store.Tenant
	json.Unmarshal(w.Body.Bytes(), &b)
	if b.SSOGroup != "" {
		t.Fatalf("no group: %s", w.Body)
	}
	if w := call("PUT", "/api/v1/tenants/"+b.ID, `{"display_name":"B","enabled":true,"sso_group":"Data Science"}`); w.Code != http.StatusConflict {
		t.Fatalf("taking a's group: %d %s", w.Code, w.Body)
	}
	if got, _ := st.GetTenant(ctx, b.ID); got.DisplayName != "" {
		t.Fatalf("a refused change changed the name: %+v", got)
	}
	if w := call("PUT", "/api/v1/tenants/"+a.ID, `{"display_name":"A","enabled":true}`); !strings.Contains(w.Body.String(), `"sso_group":"Data Science"`) {
		t.Fatalf("left out, the group stays: %s", w.Body)
	}
	if w := call("PUT", "/api/v1/tenants/"+a.ID, `{"display_name":"A","enabled":true,"sso_group":""}`); !strings.Contains(w.Body.String(), `"sso_group":""`) {
		t.Fatalf("unlinked: %s", w.Body)
	}
	if _, _, err := st.CreatePersonalKey(ctx, a.ID, "laptop"); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/api/v1/tenants/"+a.ID+"/key-usage?days=30", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"laptop"`) {
		t.Fatalf("key usage: %d %s", w.Code, w.Body)
	}
	if w := call("GET", "/api/v1/sso/groups", ""); !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("groups without sign-in: %s", w.Body)
	}
}
