// Package sso signs people in on the tenants' page with the hub cluster's
// own OpenShift login, which the cluster's OAuth server does against LDAP,
// and reads the cluster's Groups (synced from LDAP with oc adm groups sync)
// to tell which tenants a person belongs to. The hub's ServiceAccount is the
// OAuth client, so there is no OAuthClient object and no cluster-admin to
// set it up, and the hub never sees a password.
//
// A tenant linked to a group (store.Tenant.SSOGroup) lets each member sign
// in, make a personal key of their own and see every member's use. Who is
// in a group is read from the Groups with the hub's own account, every
// minute, and a person who left loses their key (Watcher).
package sso

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// ServiceAccountDir is where a pod finds its ServiceAccount's token, its
// namespace and the cluster's CAs.
const ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Config is how the hub reaches its own cluster.
type Config struct {
	// APIServer is the cluster's API: https://kubernetes.default.svc in a
	// pod. Its /.well-known/oauth-authorization-server names the OAuth
	// server, its users/~ the person, its groups the members.
	APIServer string
	// ClientID is the OAuth client: system:serviceaccount:<namespace>:<name>,
	// whose redirect reference to the hub's Route allows the callback.
	ClientID string
	// TokenFile holds the ServiceAccount's token: the OAuth client's secret
	// and the hub's credentials for reading Groups. It is read at each use,
	// as the kubelet rotates it.
	TokenFile string
	// TokenURL, when set, replaces the token endpoint the OAuth server's
	// metadata names: from a pod, the OAuth server's Service
	// (https://oauth-openshift.openshift-authentication.svc/oauth/token),
	// whose certificate the service CA signs, can be reached and trusted
	// where the Route's may not be.
	TokenURL string
	// RootCAs trusts the API server and the OAuth server; nil is the
	// system's pool.
	RootCAs *x509.CertPool
}

// Options say how the hub reaches its cluster; what is empty is the pod's.
type Options struct {
	// ServiceAccount is the hub's ServiceAccount, the OAuth client.
	ServiceAccount string
	// CAFile adds a CA bundle; TokenURL replaces the token endpoint.
	CAFile, TokenURL string
	// APIServer, TokenFile and Namespace are the pod's when empty: set them
	// to run the hub outside a pod, with a token of the ServiceAccount.
	APIServer, TokenFile, Namespace string
}

// InCluster is the Config of a pod that runs as the ServiceAccount: the API
// server by its Service, the client ID from the pod's namespace, and the
// system's CAs with the cluster's own (ca.crt, and service-ca.crt on
// OpenShift) and the CA file, if set. Options set replace the pod's.
func InCluster(o Options) (Config, error) {
	ns := []byte(o.Namespace)
	if o.Namespace == "" {
		var err error
		if ns, err = os.ReadFile(filepath.Join(ServiceAccountDir, "namespace")); err != nil {
			return Config{}, fmt.Errorf("openshift sign-in: the pod's namespace: %w", err)
		}
	}
	caFile, tokenURL, serviceAccount := o.CAFile, o.TokenURL, o.ServiceAccount
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	files := []string{filepath.Join(ServiceAccountDir, "ca.crt"), filepath.Join(ServiceAccountDir, "service-ca.crt")}
	for _, f := range files {
		if pem, err := os.ReadFile(f); err == nil {
			pool.AppendCertsFromPEM(pem)
		}
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			return Config{}, fmt.Errorf("openshift sign-in: no certificate in %s: %v", caFile, err)
		}
	}
	api := strings.TrimSuffix(o.APIServer, "/")
	if api == "" {
		host := "kubernetes.default.svc"
		if h, p := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT"); h != "" && p != "" {
			host = h + ":" + p
			if strings.Contains(h, ":") {
				host = "[" + h + "]:" + p
			}
		}
		api = "https://" + host
	}
	tokenFile := o.TokenFile
	if tokenFile == "" {
		tokenFile = filepath.Join(ServiceAccountDir, "token")
	}
	return Config{
		APIServer: api,
		ClientID:  "system:serviceaccount:" + strings.TrimSpace(string(ns)) + ":" + serviceAccount,
		TokenFile: tokenFile,
		TokenURL:  tokenURL,
		RootCAs:   pool,
	}, nil
}

// OpenShift talks to the cluster's OAuth server and API.
type OpenShift struct {
	cfg  Config
	http *http.Client

	mu     sync.Mutex
	meta   metadata
	metaAt time.Time
	// groups maps each Group's name to its members, as last read; nil
	// until a read worked. groupsErr is the last read's error.
	groups    map[string][]string
	groupsAt  time.Time
	groupsErr error
}

// metadata is what the OAuth server says of itself (RFC 8414).
type metadata struct {
	Authorize string   `json:"authorization_endpoint"`
	Token     string   `json:"token_endpoint"`
	Methods   []string `json:"code_challenge_methods_supported"`
}

// New returns a client for the cluster cfg names.
func New(cfg Config) *OpenShift {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.RootCAs != nil {
		tr.TLSClientConfig.RootCAs = cfg.RootCAs
	}
	return &OpenShift{cfg: cfg, http: &http.Client{Transport: tr, Timeout: 15 * time.Second}}
}

// ErrNoGroups is returned while the Groups have never been read.
var ErrNoGroups = errors.New("the hub cannot read the cluster's Groups yet")

func (o *OpenShift) metadata(ctx context.Context) (metadata, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.meta.Authorize != "" && time.Since(o.metaAt) < 10*time.Minute {
		return o.meta, nil
	}
	var m metadata
	if err := o.getJSON(ctx, o.cfg.APIServer+"/.well-known/oauth-authorization-server", "", &m); err != nil {
		return metadata{}, fmt.Errorf("the cluster's OAuth server: %w", err)
	}
	if m.Authorize == "" || m.Token == "" {
		return metadata{}, errors.New("the cluster's OAuth server names no authorization or token endpoint")
	}
	o.meta, o.metaAt = m, time.Now()
	return m, nil
}

// Challenge is the PKCE challenge (S256) of a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL is where the person's browser is sent to sign in: the OAuth
// server's authorization endpoint, asking for user:info only. With PKCE when
// the server offers S256 (pkce reports it), else without.
func (o *OpenShift) AuthorizeURL(ctx context.Context, redirect, state, verifier string) (u string, pkce bool, err error) {
	m, err := o.metadata(ctx)
	if err != nil {
		return "", false, err
	}
	q := url.Values{
		"client_id":     {o.cfg.ClientID},
		"response_type": {"code"},
		"scope":         {"user:info"},
		"redirect_uri":  {redirect},
		"state":         {state},
	}
	if slices.Contains(m.Methods, "S256") {
		q.Set("code_challenge", Challenge(verifier))
		q.Set("code_challenge_method", "S256")
		pkce = true
	}
	sep := "?"
	if strings.Contains(m.Authorize, "?") {
		sep = "&"
	}
	return m.Authorize + sep + q.Encode(), pkce, nil
}

// Exchange trades the code the OAuth server sent back for an access token.
// verifier is "" when the request used no PKCE.
func (o *OpenShift) Exchange(ctx context.Context, code, redirect, verifier string) (string, error) {
	tokenURL := o.cfg.TokenURL
	if tokenURL == "" {
		m, err := o.metadata(ctx)
		if err != nil {
			return "", err
		}
		tokenURL = m.Token
	}
	secret, err := o.serviceAccountToken()
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {o.cfg.ClientID},
		"client_secret": {secret},
	}
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("the OAuth server's token endpoint: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &out)
	if res.StatusCode != http.StatusOK || out.AccessToken == "" {
		return "", fmt.Errorf("the OAuth server took no code (HTTP %d): %s %s", res.StatusCode, out.Error, out.Description)
	}
	return out.AccessToken, nil
}

// User returns the name of the person an access token is for (users/~).
func (o *OpenShift) User(ctx context.Context, accessToken string) (string, error) {
	var u struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := o.getJSON(ctx, o.cfg.APIServer+"/apis/user.openshift.io/v1/users/~", accessToken, &u); err != nil {
		return "", fmt.Errorf("who signed in: %w", err)
	}
	if u.Metadata.Name == "" {
		return "", errors.New("who signed in: the cluster named nobody")
	}
	return u.Metadata.Name, nil
}

// RefreshGroups reads every Group of the cluster with the hub's account. A
// read that fails keeps the Groups read before, and says so.
func (o *OpenShift) RefreshGroups(ctx context.Context) error {
	token, err := o.serviceAccountToken()
	if err == nil {
		var list struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Users []string `json:"users"`
			} `json:"items"`
		}
		err = o.getJSON(ctx, o.cfg.APIServer+"/apis/user.openshift.io/v1/groups", token, &list)
		if err == nil {
			groups := make(map[string][]string, len(list.Items))
			for _, g := range list.Items {
				groups[g.Metadata.Name] = slices.Clone(g.Users)
			}
			o.mu.Lock()
			o.groups, o.groupsAt, o.groupsErr = groups, time.Now(), nil
			o.mu.Unlock()
			return nil
		}
	}
	err = fmt.Errorf("read the cluster's Groups: %w", err)
	o.mu.Lock()
	o.groupsErr = err
	o.mu.Unlock()
	return err
}

// Members returns each Group's members as last read, and when; ok is false
// while no read has worked.
func (o *OpenShift) Members() (groups map[string][]string, at time.Time, ok bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.groups, o.groupsAt, o.groups != nil
}

// GroupsOf returns the Groups a person is in, by name, as last read.
func (o *OpenShift) GroupsOf(user string) ([]string, error) {
	groups, _, ok := o.Members()
	if !ok {
		o.mu.Lock()
		err := o.groupsErr
		o.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNoGroups, err)
		}
		return nil, ErrNoGroups
	}
	var out []string
	for name, users := range groups {
		if slices.Contains(users, user) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// LastGroupsError is the last read of the Groups' error, nil when it worked.
func (o *OpenShift) LastGroupsError() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.groupsErr
}

func (o *OpenShift) serviceAccountToken() (string, error) {
	b, err := os.ReadFile(o.cfg.TokenFile)
	if err != nil {
		return "", fmt.Errorf("the hub's ServiceAccount token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (o *OpenShift) getJSON(ctx context.Context, u, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d %s", req.URL.Path, res.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
	}
	return json.Unmarshal(body, out)
}
