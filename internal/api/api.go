// Package api is the HTTP API used by the UI and by external callers such as
// the self-service portal.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
	"aigw-ui/internal/syncer"
)

type Server struct {
	st         *store.Store
	sy         *syncer.Syncer
	adminToken string
	uiDir      string
}

func New(st *store.Store, sy *syncer.Syncer, adminToken, uiDir string) *Server {
	return &Server{st: st, sy: sy, adminToken: adminToken, uiDir: uiDir}
}

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/overview", s.overview)

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

type clusterBody struct {
	Name        string `json:"name"`
	Site        string `json:"site"`
	Namespace   string `json:"namespace"`
	GatewayName string `json:"gateway_name"`
	AuthEnabled bool   `json:"auth_enabled"`
	Kubeconfig  string `json:"kubeconfig"`
}

func (b clusterBody) validate(needKubeconfig bool) error {
	switch {
	case !dnsLabel.MatchString(b.Name):
		return invalid("name must be lowercase letters, digits and dashes")
	case !dnsLabel.MatchString(b.Namespace):
		return invalid("namespace is not a valid Kubernetes namespace name")
	case !dnsLabel.MatchString(b.GatewayName):
		return invalid("gateway_name is not a valid Kubernetes object name")
	case needKubeconfig && strings.TrimSpace(b.Kubeconfig) == "":
		return invalid("kubeconfig is required")
	}
	return nil
}

func (b clusterBody) cluster(id string) store.Cluster {
	return store.Cluster{ID: id, Name: b.Name, Site: b.Site, Namespace: b.Namespace, GatewayName: b.GatewayName, AuthEnabled: b.AuthEnabled}
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
	if err := b.validate(true); err != nil {
		fail(w, err)
		return
	}
	c, err := s.st.CreateCluster(r.Context(), b.cluster(""), []byte(b.Kubeconfig))
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
	s.sy.Changed(r.Context())
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateCluster(w http.ResponseWriter, r *http.Request) {
	var b clusterBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	if err := b.validate(false); err != nil {
		fail(w, err)
		return
	}
	c, err := s.st.UpdateCluster(r.Context(), b.cluster(r.PathValue("id")), []byte(strings.TrimSpace(b.Kubeconfig)))
	if err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context())
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
	state, err := s.st.RenderState(r.Context(), r.PathValue("id"))
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

type modelBody struct {
	Name          string           `json:"name"`
	DefaultLimit  int64            `json:"default_limit"`
	DefaultWindow string           `json:"default_window"`
	Endpoints     []store.Endpoint `json:"endpoints"`
}

func (b *modelBody) input(name string) (store.ModelInput, error) {
	if b.DefaultLimit == 0 {
		b.DefaultLimit = 1
	}
	if b.DefaultWindow == "" {
		b.DefaultWindow = "1d"
	}
	switch {
	case !modelName.MatchString(name):
		return store.ModelInput{}, invalid("name must start with a letter or digit and use only letters, digits and . _ : / -")
	case render.Slug(name) == "":
		return store.ModelInput{}, invalid("name must contain a letter or digit")
	case b.DefaultLimit < 1:
		return store.ModelInput{}, invalid("default_limit must be at least 1")
	case !validWindow(b.DefaultWindow):
		return store.ModelInput{}, invalid("default_window must be 1m, 1h or 1d")
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
	return store.ModelInput{Name: name, DefaultLimit: b.DefaultLimit, DefaultWindow: b.DefaultWindow, Endpoints: b.Endpoints}, nil
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
	in, err := b.input(b.Name)
	if err != nil {
		fail(w, err)
		return
	}
	id, err := s.st.CreateModel(r.Context(), in)
	if err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context())
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
	name, err := s.st.ModelName(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	in, err := b.input(name)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.st.UpdateModel(r.Context(), id, in); err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteModel(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context())
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
	s.sy.Changed(r.Context())
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) deleteTenant(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteTenant(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context())
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
	s.sy.Changed(r.Context())
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "secret": plain})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	if err := s.st.RevokeKey(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) upsertQuota(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ModelID    string `json:"model_id"`
		TokenLimit int64  `json:"token_limit"`
		Window     string `json:"window"`
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
	id := r.PathValue("id")
	if err := s.st.UpsertQuota(r.Context(), id, b.ModelID, b.TokenLimit, b.Window); err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context())
	quotas, err := s.st.ListQuotas(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, quotas)
}

func (s *Server) deleteQuota(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteQuota(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context())
	w.WriteHeader(http.StatusNoContent)
}
