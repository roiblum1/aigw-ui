package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"aigw-ui/internal/sso"
	"aigw-ui/internal/store"
)

// Signing in with OpenShift (0.37.0). A tenant linked to an OpenShift Group
// (store.Tenant.SSOGroup) lets its members sign in on the tenants' page with
// the hub cluster's own login, make a personal key of their own, and see
// what every member's keys used. The session is a signed cookie with the
// person's user name; which tenants they may see is read from the Groups
// each time, so leaving a group takes effect within a minute. A tenant
// without a group is not reachable this way at all.
const (
	sessionCookie = "aigw-session"
	signInCookie  = "aigw-sign-in"
	sessionLife   = 12 * time.Hour
	// teamHeader names, by slug, the tenant a person who signed in looks at.
	teamHeader = "X-Aigw-Team"
)

// personKey holds, in a request's context, the user name of a person who
// signed in with OpenShift; absent for a key sign-in.
type personKey struct{}

// WithSSO turns on the sign-in with OpenShift; cluster nil leaves it off.
func (s *Server) WithSSO(cluster *sso.OpenShift, sealer *sso.Sealer) *Server {
	s.sso, s.sealer = cluster, sealer
	return s
}

type session struct {
	User string `json:"u"`
}

type signIn struct {
	State    string `json:"s"`
	Verifier string `json:"v,omitempty"`
}

func randomText(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// secure tells whether the browser reached the hub over HTTPS: directly,
// or through the OpenShift router, which says so in X-Forwarded-Proto.
func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// callbackURL is where the OAuth server sends the browser back: this hub's
// address as the browser knows it. The ServiceAccount's redirect reference
// to the hub's Route is what lets the OAuth server take it.
func callbackURL(r *http.Request) string {
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/sso/callback"
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, name, value, path string, expires time.Time) {
	c := &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode}
	if value == "" {
		c.MaxAge = -1
	} else {
		c.Expires = expires
	}
	http.SetCookie(w, c)
}

// ssoLogin sends the browser to the cluster's OAuth server.
func (s *Server) ssoLogin(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		writeError(w, http.StatusNotFound, "signing in with OpenShift is turned off")
		return
	}
	in := signIn{State: randomText(24), Verifier: randomText(48)}
	u, pkce, err := s.sso.AuthorizeURL(r.Context(), callbackURL(r), in.State, in.Verifier)
	if err != nil {
		slog.Warn("openshift sign-in: no authorization endpoint", "err", err)
		signInFailed(w, r, err)
		return
	}
	if !pkce {
		in.Verifier = ""
	}
	now := time.Now()
	value, err := s.sealer.Seal("sign-in", in, now.Add(10*time.Minute))
	if err != nil {
		fail(w, err)
		return
	}
	s.setCookie(w, r, signInCookie, value, "/sso/", now.Add(10*time.Minute))
	http.Redirect(w, r, u, http.StatusFound)
}

// signInFailed sends the browser back to the tenants' page with the reason.
func signInFailed(w http.ResponseWriter, r *http.Request, err error) {
	http.Redirect(w, r, "/my-usage?sso_error="+url.QueryEscape(err.Error()), http.StatusFound)
}

// ssoCallback takes the code the OAuth server sent back, finds who signed
// in, and gives the browser its session.
func (s *Server) ssoCallback(w http.ResponseWriter, r *http.Request) {
	if s.sso == nil {
		writeError(w, http.StatusNotFound, "signing in with OpenShift is turned off")
		return
	}
	now := time.Now()
	q := r.URL.Query()
	c, cerr := r.Cookie(signInCookie)
	s.setCookie(w, r, signInCookie, "", "/sso/", now)
	if e := q.Get("error"); e != "" {
		signInFailed(w, r, errors.New("OpenShift refused the sign-in: "+e+" "+q.Get("error_description")))
		return
	}
	var in signIn
	if cerr != nil || !s.sealer.Open("sign-in", c.Value, &in, now) ||
		subtle.ConstantTimeCompare([]byte(in.State), []byte(q.Get("state"))) != 1 || q.Get("code") == "" {
		signInFailed(w, r, errors.New("this sign-in was not started here, or took longer than 10 minutes: sign in again"))
		return
	}
	token, err := s.sso.Exchange(r.Context(), q.Get("code"), callbackURL(r), in.Verifier)
	if err != nil {
		slog.Warn("openshift sign-in: code not taken", "err", err)
		signInFailed(w, r, err)
		return
	}
	user, err := s.sso.User(r.Context(), token)
	if err != nil {
		slog.Warn("openshift sign-in: nobody named", "err", err)
		signInFailed(w, r, err)
		return
	}
	value, err := s.sealer.Seal("session", session{User: user}, now.Add(sessionLife))
	if err != nil {
		fail(w, err)
		return
	}
	s.setCookie(w, r, sessionCookie, value, "/", now.Add(sessionLife))
	slog.Info("openshift sign-in", "user", user, "remote_addr", clientAddr(r))
	http.Redirect(w, r, "/my-usage", http.StatusFound)
}

// sessionUser returns the user name of the session the request carries.
func (s *Server) sessionUser(r *http.Request) (string, bool) {
	if s.sso == nil {
		return "", false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	var sess session
	if !s.sealer.Open("session", c.Value, &sess, time.Now()) || sess.User == "" {
		return "", false
	}
	return sess.User, true
}

// teams returns the enabled tenants whose groups a person is in.
func (s *Server) teams(ctx context.Context, user string) ([]store.Tenant, error) {
	groups, err := s.sso.GroupsOf(user)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	return s.st.TenantsInGroups(ctx, groups)
}

// errNoTeam is a person who signed in but whose groups link no tenant.
var errNoTeam = errors.New("you are in no tenant's group: ask your admin to link your LDAP group to a tenant")

// errNotYourTeam is a tenant named in teamHeader that none of the person's
// groups link, while others do.
var errNotYourTeam = errors.New("that tenant is not linked to any of your groups")

// teamOf returns the tenant a person who signed in looks at: the one named
// by slug, or their only one.
func (s *Server) teamOf(ctx context.Context, user, slug string) (store.Tenant, error) {
	ts, err := s.teams(ctx, user)
	if err != nil {
		return store.Tenant{}, err
	}
	if len(ts) == 0 {
		return store.Tenant{}, errNoTeam
	}
	for _, t := range ts {
		if t.Slug == slug || (slug == "" && len(ts) == 1) {
			return t, nil
		}
	}
	if slug == "" {
		return store.Tenant{}, invalid("you are in several tenants' groups: name one in %s", teamHeader)
	}
	return store.Tenant{}, errNotYourTeam
}

// sessionTenant is tenantAuth for a person who signed in with OpenShift.
func (s *Server) sessionTenant(w http.ResponseWriter, r *http.Request) (context.Context, bool) {
	user, ok := s.sessionUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "sign in with a key, or with OpenShift")
		return nil, false
	}
	t, err := s.teamOf(r.Context(), user, r.Header.Get(teamHeader))
	switch {
	case errors.Is(err, sso.ErrNoGroups):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return nil, false
	case errors.Is(err, errNoTeam) || errors.Is(err, errNotYourTeam):
		writeError(w, http.StatusForbidden, err.Error())
		return nil, false
	case err != nil:
		fail(w, err)
		return nil, false
	}
	ctx := context.WithValue(r.Context(), tenantKey{}, t)
	return context.WithValue(ctx, personKey{}, user), true
}

type ssoTeam struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
}

// getSSO tells the tenants' page whether it can offer the sign-in, and who
// is signed in, with their teams.
func (s *Server) getSSO(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"enabled": s.sso != nil && s.tenantPage, "user": "", "teams": []ssoTeam{}}
	if user, ok := s.sessionUser(r); ok && s.tenantPage {
		out["user"] = user
		ts, err := s.teams(r.Context(), user)
		if err != nil {
			out["error"] = err.Error()
		}
		teams := []ssoTeam{}
		for _, t := range ts {
			teams = append(teams, ssoTeam{ID: t.ID, Slug: t.Slug, DisplayName: t.DisplayName})
		}
		out["teams"] = teams
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ssoLogout(w http.ResponseWriter, r *http.Request) {
	s.setCookie(w, r, sessionCookie, "", "/", time.Now())
	w.WriteHeader(http.StatusNoContent)
}

// person returns the user name of a person who signed in with OpenShift,
// and answers 403 for a key sign-in.
func person(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, ok := r.Context().Value(personKey{}).(string)
	if !ok {
		writeError(w, http.StatusForbidden, "this is for people who signed in with OpenShift")
	}
	return user, ok
}

// getMyKey returns the live personal key the person made in the tenant,
// without its secret, or null.
func (s *Server) getMyKey(w http.ResponseWriter, r *http.Request) {
	user, ok := person(w, r)
	if !ok {
		return
	}
	t := r.Context().Value(tenantKey{}).(store.Tenant)
	k, err := s.st.OwnedKey(r.Context(), t.ID, user)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"key": nil, "user": user})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k, "user": user})
}

// createMyKey makes the person a personal key in the tenant, in place of
// the one they had, and shows it once. It must name the tenant in
// teamHeader, which no form of another site can send.
func (s *Server) createMyKey(w http.ResponseWriter, r *http.Request) {
	user, ok := person(w, r)
	if !ok {
		return
	}
	t := r.Context().Value(tenantKey{}).(store.Tenant)
	if r.Header.Get(teamHeader) != t.Slug {
		fail(w, invalid("name the tenant in %s", teamHeader))
		return
	}
	k, secret, err := s.st.CreateOwnedPersonalKey(r.Context(), t.ID, user)
	if err != nil {
		fail(w, err)
		return
	}
	summary := "Created personal key " + k.ClientID + " for " + user + ", who signed in with OpenShift"
	// The sync's task is the one entry: a finished task beside it showed
	// every key twice in the task log.
	s.changed(r, "key.add", summary)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": secret})
}

// keyUsePerson is what one key of a tenant used over a period, by model.
type keyUsePerson struct {
	KeyID   string         `json:"key_id"`
	Name    string         `json:"name"`
	Owner   string         `json:"owner"`
	Kind    string         `json:"kind"`
	Revoked *time.Time     `json:"revoked_at"`
	Me      bool           `json:"me"`
	Models  []store.KeyUse `json:"models"`
	Totals  keyUseTotals   `json:"totals"`
}

type keyUseTotals struct {
	Requests int64 `json:"requests"`
	Input    int64 `json:"input_tokens"`
	Cached   int64 `json:"cached_tokens"`
	Output   int64 `json:"output_tokens"`
}

// keyUses is what each key of a tenant used since from: every live key,
// and every revoked one that used something.
func (s *Server) keyUses(ctx context.Context, tenantID, me string, from time.Time) ([]keyUsePerson, error) {
	uses, err := s.st.KeyUses(ctx, tenantID, from)
	if err != nil {
		return nil, err
	}
	keys, err := s.st.ListKeys(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*keyUsePerson{}
	var order []string
	add := func(k store.APIKey) *keyUsePerson {
		if p, ok := byKey[k.ID]; ok {
			return p
		}
		p := &keyUsePerson{KeyID: k.ID, Name: k.Name, Owner: k.Owner, Kind: k.Kind, Revoked: k.RevokedAt, Me: me != "" && k.Owner == me, Models: []store.KeyUse{}}
		byKey[k.ID] = p
		order = append(order, k.ID)
		return p
	}
	known := map[string]store.APIKey{}
	for _, k := range keys {
		known[k.ID] = k
		if k.RevokedAt == nil && k.Kind != store.KindKey {
			add(k)
		}
	}
	for _, u := range uses {
		k, ok := known[u.KeyID]
		if !ok {
			continue
		}
		p := add(k)
		p.Models = append(p.Models, u)
		p.Totals.Requests += u.Requests
		p.Totals.Input += u.Input
		p.Totals.Cached += u.Cached
		p.Totals.Output += u.Output
	}
	out := make([]keyUsePerson, 0, len(order))
	for _, id := range order {
		out = append(out, *byKey[id])
	}
	return out, nil
}

// usageDays reads the days parameter: 1 to 90, 7 by default.
func usageDays(r *http.Request) (int, error) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 90 {
			return 0, invalid("days must be a whole number from 1 to 90")
		}
		days = n
	}
	return days, nil
}

// getMyPeople returns what every key of the tenant used, for a person who
// signed in with OpenShift: the members of a group see each other's use.
func (s *Server) getMyPeople(w http.ResponseWriter, r *http.Request) {
	user, ok := person(w, r)
	if !ok {
		return
	}
	s.writeKeyUses(w, r, r.Context().Value(tenantKey{}).(store.Tenant).ID, user)
}

// getTenantKeyUsage is the same for an admin, on any tenant.
func (s *Server) getTenantKeyUsage(w http.ResponseWriter, r *http.Request) {
	t, err := s.st.GetTenant(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.writeKeyUses(w, r, t.ID, "")
}

func (s *Server) writeKeyUses(w http.ResponseWriter, r *http.Request, tenantID, me string) {
	days, err := usageDays(r)
	if err != nil {
		fail(w, err)
		return
	}
	from := time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(days)*24*time.Hour + time.Hour)
	people, err := s.keyUses(r.Context(), tenantID, me, from)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "days": days, "keys": people})
}

// getSSOGroups lists the cluster's Groups for the tenant form, as last read.
func (s *Server) getSSOGroups(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"enabled": s.sso != nil, "groups": []string{}}
	if s.sso != nil {
		groups, at, ok := s.sso.Members()
		if ok {
			names := make([]string, 0, len(groups))
			for name := range groups {
				names = append(names, name)
			}
			slices.Sort(names)
			out["groups"], out["read_at"] = names, at
		}
		if err := s.sso.LastGroupsError(); err != nil {
			out["error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
}
