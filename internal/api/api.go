// Package api is the HTTP API used by the UI and by external callers such as
// the self-service portal.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"aigw-ui/internal/gateway"
	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
	"aigw-ui/internal/syncer"
	"aigw-ui/internal/usage"
)

type Server struct {
	st         *store.Store
	sy         *syncer.Syncer
	adminToken string
	uiDir      string
	// usageReader is nil when no Redis is configured.
	usageReader *usage.Reader
}

func New(st *store.Store, sy *syncer.Syncer, usageReader *usage.Reader, adminToken, uiDir string) *Server {
	return &Server{st: st, sy: sy, usageReader: usageReader, adminToken: adminToken, uiDir: uiDir}
}

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/overview", s.overview)
	api.HandleFunc("GET /api/v1/tasks", s.listTasks)
	api.HandleFunc("GET /api/v1/usage", s.usage)
	api.HandleFunc("POST /api/v1/tenants/{id}/quotas/{model_id}/reset", s.resetUsage)

	api.HandleFunc("GET /api/v1/clusters", s.listClusters)
	api.HandleFunc("POST /api/v1/clusters", s.createCluster)
	api.HandleFunc("PUT /api/v1/clusters/{id}", s.updateCluster)
	api.HandleFunc("DELETE /api/v1/clusters/{id}", s.deleteCluster)
	api.HandleFunc("POST /api/v1/clusters/{id}/probe", s.probeCluster)
	api.HandleFunc("POST /api/v1/clusters/{id}/sync", s.syncCluster)
	api.HandleFunc("GET /api/v1/clusters/{id}/manifests", s.clusterManifests)
	api.HandleFunc("POST /api/v1/sync", s.syncAll)
	api.HandleFunc("POST /api/v1/clusters/{id}/discover", s.discoverCluster)
	api.HandleFunc("POST /api/v1/discover", s.discoverAll)

	api.HandleFunc("GET /api/v1/models", s.listModels)
	api.HandleFunc("POST /api/v1/models", s.createModel)
	api.HandleFunc("PUT /api/v1/models/{id}", s.updateModel)
	api.HandleFunc("DELETE /api/v1/models/{id}", s.deleteModel)

	api.HandleFunc("GET /api/v1/tenants", s.listTenants)
	api.HandleFunc("POST /api/v1/tenants", s.createTenant)
	api.HandleFunc("GET /api/v1/tenants/{id}", s.getTenant)
	api.HandleFunc("PUT /api/v1/tenants/{id}", s.updateTenant)
	api.HandleFunc("DELETE /api/v1/tenants/{id}", s.deleteTenant)
	api.HandleFunc("POST /api/v1/tenants/{id}/keys", s.createKey)
	api.HandleFunc("DELETE /api/v1/keys/{id}", s.revokeKey)
	api.HandleFunc("PUT /api/v1/tenants/{id}/quotas", s.upsertQuota)
	api.HandleFunc("DELETE /api/v1/quotas/{id}", s.deleteQuota)

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	root.Handle("/api/", s.auth(api))
	root.HandleFunc("/", s.ui)
	return root
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.adminToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid or missing token")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r)
	})
}

// ui serves the built single-page app, falling back to index.html for client routes.
func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.uiDir, filepath.Clean("/"+r.URL.Path))
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		path = filepath.Join(s.uiDir, "index.html")
	}
	http.ServeFile(w, r, path)
}

// ---- helpers ----

type validationError string

func (e validationError) Error() string { return string(e) }

func invalid(format string, args ...any) error { return validationError(fmt.Sprintf(format, args...)) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func fail(w http.ResponseWriter, err error) {
	var v validationError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &v):
		writeError(w, http.StatusBadRequest, v.Error())
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "an item with that name already exists")
	default:
		slog.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		return invalid("invalid JSON body: %v", err)
	}
	return nil
}

var (
	dnsLabel   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tenantSlug = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	hostname   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	modelName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,99}$`)
)

func validWindow(w string) bool { return w == "1m" || w == "1h" || w == "1d" }

// log adds an entry to the task log for something that is already finished
// and needs no sync.
func (s *Server) log(r *http.Request, action, summary string, ok bool, message string) {
	if err := s.st.CreateFinishedTask(context.WithoutCancel(r.Context()), action, summary, ok, message); err != nil {
		slog.Error("record task", "err", err)
	}
}

// listTasks returns the newest tasks first. ?limit= defaults to 50, at most 500.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			fail(w, invalid("limit must be between 1 and 500"))
			return
		}
		limit = n
	}
	tasks, err := s.st.ListTasks(r.Context(), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

// ---- overview ----

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	o, err := s.st.Overview(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// ---- clusters ----

// clusterBody is the body of a cluster create or update. On an update every
// field that is left out keeps its stored value, so a caller that only knows
// some of the fields cannot wipe the others.
type clusterBody struct {
	Name        string  `json:"name"`
	Site        *string `json:"site"`
	Namespace   string  `json:"namespace"`
	GatewayName string  `json:"gateway_name"`
	AuthEnabled *bool   `json:"auth_enabled"`
	Kubeconfig  string  `json:"kubeconfig"`
	// GatewayURL is optional; an empty string removes it. DiscoveryToken is
	// write-only; empty keeps the stored one.
	GatewayURL     *string `json:"gateway_url"`
	DiscoveryToken string  `json:"discovery_token"`
}

// cluster validates the body and returns the cluster to store. current is the
// stored cluster on an update and nil on a create.
func (b *clusterBody) cluster(current *store.Cluster) (store.Cluster, error) {
	var c store.Cluster
	if current != nil {
		c = *current
	}
	if b.Name != "" {
		c.Name = b.Name
	}
	if b.Namespace != "" {
		c.Namespace = b.Namespace
	}
	if b.GatewayName != "" {
		c.GatewayName = b.GatewayName
	}
	if b.Site != nil {
		c.Site = *b.Site
	}
	if b.AuthEnabled != nil {
		c.AuthEnabled = *b.AuthEnabled
	}
	if b.GatewayURL != nil {
		c.GatewayURL = strings.TrimSpace(*b.GatewayURL)
		if c.GatewayURL != "" {
			normalized, err := gateway.NormalizeURL(c.GatewayURL)
			if err != nil {
				return c, invalid("gateway_url %v", err)
			}
			c.GatewayURL = normalized
		}
	}
	b.DiscoveryToken = strings.TrimSpace(b.DiscoveryToken)
	switch {
	case b.DiscoveryToken != "" && c.GatewayURL == "":
		return c, invalid("discovery_token needs a gateway_url")
	case !dnsLabel.MatchString(c.Name):
		return c, invalid("name must be lowercase letters, digits and dashes")
	case !dnsLabel.MatchString(c.Namespace):
		return c, invalid("namespace is not a valid Kubernetes namespace name")
	case !dnsLabel.MatchString(c.GatewayName):
		return c, invalid("gateway_name is not a valid Kubernetes object name")
	case current == nil && strings.TrimSpace(b.Kubeconfig) == "":
		return c, invalid("kubeconfig is required")
	}
	return c, nil
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	out, err := s.st.ListClusters(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request) {
	var b clusterBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	in, err := b.cluster(nil)
	if err != nil {
		fail(w, err)
		return
	}
	c, err := s.st.CreateCluster(r.Context(), in, []byte(b.Kubeconfig), b.DiscoveryToken)
	if err != nil {
		fail(w, err)
		return
	}
	// Pull the cluster's models right away instead of waiting for the next poll.
	// A cluster that cannot be reached is still saved; the failure shows in its row.
	if _, err := s.sy.DiscoverCluster(r.Context(), c.ID); err == nil {
		if fresh, err := s.st.GetCluster(r.Context(), c.ID); err == nil {
			c = fresh
		}
	}
	s.sy.Changed(r.Context(), "cluster.add", "Added cluster "+c.Name)
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateCluster(w http.ResponseWriter, r *http.Request) {
	var b clusterBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	current, err := s.st.GetCluster(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	in, err := b.cluster(&current)
	if err != nil {
		fail(w, err)
		return
	}
	c, err := s.st.UpdateCluster(r.Context(), in, []byte(strings.TrimSpace(b.Kubeconfig)), b.DiscoveryToken)
	if err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context(), "cluster.update", "Changed cluster "+c.Name)
	writeJSON(w, http.StatusOK, c)
}

// deleteCluster only forgets the cluster. Objects already applied to it stay
// there, because once the kubeconfig is gone they can no longer be removed.
func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteCluster(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) probeCluster(w http.ResponseWriter, r *http.Request) {
	p, err := s.sy.Probe(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"reachable": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reachable": true, "probe": p})
}

func (s *Server) syncCluster(w http.ResponseWriter, r *http.Request) {
	if c, err := s.st.GetCluster(r.Context(), r.PathValue("id")); err == nil {
		s.sy.Record(r.Context(), "sync", "Manual sync of "+c.Name, c.ID)
	}
	res, err := s.sy.SyncCluster(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Server) syncAll(w http.ResponseWriter, r *http.Request) {
	s.sy.Record(r.Context(), "sync", "Manual sync of all clusters", "")
	s.sy.SyncAll(r.Context())
	s.listClusters(w, r)
}

func (s *Server) discoverCluster(w http.ResponseWriter, r *http.Request) {
	n, err := s.sy.DiscoverCluster(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": n})
}

func (s *Server) discoverAll(w http.ResponseWriter, r *http.Request) {
	s.sy.DiscoverAll(r.Context())
	s.listClusters(w, r)
}

// clusterManifests shows what a sync would apply, with key values masked.
func (s *Server) clusterManifests(w http.ResponseWriter, r *http.Request) {
	state, err := s.st.RenderState(r.Context(), r.PathValue("id"), true)
	if err != nil {
		fail(w, err)
		return
	}
	var sb strings.Builder
	for i, obj := range render.Redacted(render.Objects(state)) {
		doc, err := yaml.Marshal(obj.Object)
		if err != nil {
			fail(w, err)
			return
		}
		if i > 0 {
			sb.WriteString("---\n")
		}
		sb.Write(doc)
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": sb.String()})
}

// ---- models ----

// modelBody is the body of a model create or update. On an update every field
// that is left out keeps its stored value; "endpoints": [] removes the manual
// endpoints, leaving the field out keeps them.
type modelBody struct {
	Name           string           `json:"name"`
	DefaultLimit   *int64           `json:"default_limit"`
	DefaultWindow  *string          `json:"default_window"`
	CostExpression *string          `json:"cost_expression"`
	Endpoints      []store.Endpoint `json:"endpoints"`
}

// input validates the body. current is the stored model on an update and nil
// on a create.
func (b *modelBody) input(name string, current *store.Model) (store.ModelInput, error) {
	in := store.ModelInput{Name: name, DefaultLimit: 1, DefaultWindow: "1d"}
	if current != nil {
		in.DefaultLimit, in.DefaultWindow, in.CostExpression = current.DefaultLimit, current.DefaultWindow, current.CostExpression
		in.KeepEndpoints = b.Endpoints == nil
	}
	if b.DefaultLimit != nil {
		in.DefaultLimit = *b.DefaultLimit
	}
	if b.DefaultWindow != nil {
		in.DefaultWindow = *b.DefaultWindow
	}
	if b.CostExpression != nil {
		in.CostExpression = strings.TrimSpace(*b.CostExpression)
	}
	switch {
	case !modelName.MatchString(name):
		return store.ModelInput{}, invalid("name must start with a letter or digit and use only letters, digits and . _ : / -")
	case render.Slug(name) == "":
		return store.ModelInput{}, invalid("name must contain a letter or digit")
	case in.DefaultLimit < 1:
		return store.ModelInput{}, invalid("default_limit must be at least 1")
	case !validWindow(in.DefaultWindow):
		return store.ModelInput{}, invalid("default_window must be 1m, 1h or 1d")
	}
	if err := validCostExpression(in.CostExpression); err != nil {
		return store.ModelInput{}, err
	}
	// Discovered endpoints come from the clusters and cannot be set here.
	manual := b.Endpoints[:0:0]
	for _, e := range b.Endpoints {
		if e.Source != store.SourceDiscovered {
			manual = append(manual, e)
		}
	}
	b.Endpoints = manual

	seen := map[string]bool{}
	for _, e := range b.Endpoints {
		switch {
		case e.ClusterID == "":
			return store.ModelInput{}, invalid("each endpoint needs a cluster_id")
		case seen[e.ClusterID]:
			return store.ModelInput{}, invalid("a model can have one endpoint per cluster")
		case !hostname.MatchString(e.Host):
			return store.ModelInput{}, invalid("endpoint host %q is not a valid hostname or IP", e.Host)
		case e.Port < 1 || e.Port > 65535:
			return store.ModelInput{}, invalid("endpoint port must be between 1 and 65535")
		case e.UpstreamModel != "" && !modelName.MatchString(e.UpstreamModel):
			return store.ModelInput{}, invalid("upstream_model %q is not a valid model name", e.UpstreamModel)
		}
		seen[e.ClusterID] = true
	}
	in.Endpoints = b.Endpoints
	return in, nil
}

var (
	costToken = regexp.MustCompile(`^(?:[a-z_]+|[0-9]+(?:\.[0-9]+)?u?|[-+*/()]|\s+)`)
	costWord  = regexp.MustCompile(`^[a-z_]+$`)
	costNames = map[string]bool{
		"input_tokens": true, "output_tokens": true, "total_tokens": true, "cached_input_tokens": true,
		"cache_creation_input_tokens": true, "reasoning_tokens": true, "uint": true, "double": true,
	}
)

// validCostExpression accepts arithmetic over the token counts the gateway
// exposes. It is a guard against typos, not a CEL parser: the gateway has the
// final say when the policy is applied.
func validCostExpression(expr string) error {
	if len(expr) > 200 {
		return invalid("cost_expression must be at most 200 characters")
	}
	depth := 0
	for rest := expr; rest != ""; {
		tok := costToken.FindString(rest)
		switch {
		case tok == "":
			return invalid("cost_expression has an unsupported character at %q", rest)
		case costWord.MatchString(tok) && !costNames[tok]:
			return invalid("cost_expression uses %q; the known names are input_tokens, output_tokens, total_tokens, cached_input_tokens, cache_creation_input_tokens, reasoning_tokens, uint and double", tok)
		case tok == "(":
			depth++
		case tok == ")":
			depth--
		}
		if depth < 0 {
			return invalid("cost_expression has an unmatched )")
		}
		rest = rest[len(tok):]
	}
	if depth != 0 {
		return invalid("cost_expression has an unmatched (")
	}
	return nil
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	out, err := s.st.ListModels(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createModel(w http.ResponseWriter, r *http.Request) {
	var b modelBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	in, err := b.input(b.Name, nil)
	if err != nil {
		fail(w, err)
		return
	}
	id, err := s.st.CreateModel(r.Context(), in)
	if err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context(), "model.add", "Added model "+in.Name)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// updateModel ignores the name in the body: a model cannot be renamed.
func (s *Server) updateModel(w http.ResponseWriter, r *http.Request) {
	var b modelBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	id := r.PathValue("id")
	current, err := s.st.GetModel(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	in, err := b.input(current.Name, &current)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.st.UpdateModel(r.Context(), id, in); err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context(), "model.update", "Changed model "+in.Name)
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	// The name is read first: after the delete it is gone.
	m, err := s.st.GetModel(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.st.DeleteModel(r.Context(), m.ID); err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context(), "model.delete", "Deleted model "+m.Name+" and its quotas")
	w.WriteHeader(http.StatusNoContent)
}

// ---- tenants ----

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	out, err := s.st.ListTenants(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	if !tenantSlug.MatchString(b.Slug) {
		fail(w, invalid("slug must be 1-40 lowercase letters, digits and dashes"))
		return
	}
	t, err := s.st.CreateTenant(r.Context(), b.Slug, b.DisplayName)
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, "tenant.add", "Added tenant "+t.Slug, true,
		"Saved. Nothing is applied to the clusters until the tenant has a key or a quota.")
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) getTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.st.GetTenant(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	keys, err := s.st.ListKeys(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	quotas, err := s.st.ListQuotas(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": t, "keys": keys, "quotas": quotas})
}

func (s *Server) updateTenant(w http.ResponseWriter, r *http.Request) {
	var b struct {
		DisplayName string `json:"display_name"`
		Enabled     bool   `json:"enabled"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	t, err := s.st.UpdateTenant(r.Context(), r.PathValue("id"), b.DisplayName, b.Enabled)
	if err != nil {
		fail(w, err)
		return
	}
	state := "Enabled"
	if !t.Enabled {
		state = "Disabled"
	}
	s.sy.Changed(r.Context(), "tenant.update", state+" tenant "+t.Slug)
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request) {
	t, err := s.st.GetTenant(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.st.DeleteTenant(r.Context(), t.ID); err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context(), "tenant.delete", "Deleted tenant "+t.Slug+" with its keys and quotas")
	w.WriteHeader(http.StatusNoContent)
}

// createKey is the only response that ever contains the key itself.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name string `json:"name"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	k, plain, err := s.st.CreateKey(r.Context(), r.PathValue("id"), b.Name)
	if err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context(), "key.add", "Created API key "+k.ClientID)
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": plain})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	clientID, err := s.st.RevokeKey(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context(), "key.revoke", "Revoked API key "+clientID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) upsertQuota(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ModelID    string `json:"model_id"`
		TokenLimit int64  `json:"token_limit"`
		Window     string `json:"window"`
		// Shadow left out keeps the stored setting; a new quota is enforced.
		Shadow *bool `json:"shadow"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	switch {
	case b.ModelID == "":
		fail(w, invalid("model_id is required"))
		return
	case b.TokenLimit < 1:
		fail(w, invalid("token_limit must be at least 1"))
		return
	case !validWindow(b.Window):
		fail(w, invalid("window must be 1m, 1h or 1d"))
		return
	}
	// A quota on a model with nothing to attach it to would silently do nothing.
	capable, err := s.st.QuotaCapable(r.Context(), b.ModelID)
	if err != nil {
		fail(w, err)
		return
	}
	if !capable {
		fail(w, invalid("this model has no AIServiceBackend on any cluster, so a quota cannot be applied to it"))
		return
	}
	id := r.PathValue("id")
	if err := s.st.UpsertQuota(r.Context(), id, b.ModelID, b.TokenLimit, b.Window, b.Shadow); err != nil {
		fail(w, err)
		return
	}
	quotas, err := s.st.ListQuotas(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	summary := "Set a quota"
	if t, err := s.st.GetTenant(r.Context(), id); err == nil {
		for _, q := range quotas {
			if q.ModelID == b.ModelID {
				unit := map[string]string{"1m": "minute", "1h": "hour", "1d": "day"}[q.Window]
				summary = fmt.Sprintf("Set quota of %s on %s to %d tokens per %s", t.Slug, q.ModelName, q.TokenLimit, unit)
				if q.Shadow {
					summary += " (dry run)"
				}
			}
		}
	}
	s.sy.Changed(r.Context(), "quota.set", summary)
	writeJSON(w, http.StatusOK, quotas)
}

func (s *Server) deleteQuota(w http.ResponseWriter, r *http.Request) {
	label, err := s.st.DeleteQuota(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context(), "quota.delete", "Removed quota of "+label)
	w.WriteHeader(http.StatusNoContent)
}
