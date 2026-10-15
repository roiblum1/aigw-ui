// Package api is the HTTP API used by the UI and by external callers such as
// the self-service portal.
package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"aigw-ui/internal/benchmark"
	"aigw-ui/internal/selftest"
	"aigw-ui/internal/store"
	"aigw-ui/internal/syncer"
	"aigw-ui/internal/usage"
)

type Server struct {
	st         *store.Store
	sy         *syncer.Syncer
	adminToken string
	uiDir      string
	// usage is nil when no Redis is configured.
	usage    *usage.Service
	selftest *selftest.Runner
	bench    *benchmark.Service
	// tenantPage lets tenants sign in with an API key to see their own usage.
	tenantPage  bool
	keyFailures keyFailures
	// wallets is set when the hub enforces wallets: it adds them up from
	// the usage history, which needs Redis.
	wallets bool
}

// WithWallets says whether the hub enforces wallets.
func (s *Server) WithWallets(on bool) *Server {
	s.wallets = on
	return s
}

// New builds the API. usage may be nil, which turns usage monitoring off.
func New(st *store.Store, sy *syncer.Syncer, usage *usage.Service, selftest *selftest.Runner, bench *benchmark.Service, adminToken, uiDir string, tenantPage bool) *Server {
	return &Server{st: st, sy: sy, usage: usage, selftest: selftest, bench: bench, adminToken: adminToken, uiDir: uiDir, tenantPage: tenantPage}
}

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/overview", s.overview)
	api.HandleFunc("GET /api/v1/tasks", s.listTasks)
	api.HandleFunc("GET /api/v1/audit", s.listAudit)
	api.HandleFunc("GET /api/v1/docs/platform-architecture", s.platformArchitecture)
	api.HandleFunc("GET /api/v1/docs", s.listDocs)
	api.HandleFunc("GET /api/v1/docs/text/{name...}", s.readDoc)
	api.HandleFunc("GET /api/v1/usage", s.getUsage)
	api.HandleFunc("GET /api/v1/usage/history", s.usageHistory)
	api.HandleFunc("POST /api/v1/tenants/{id}/quotas/{model_id}/reset", s.resetUsage)

	api.HandleFunc("GET /api/v1/clusters", s.listClusters)
	api.HandleFunc("POST /api/v1/clusters", s.createCluster)
	api.HandleFunc("PUT /api/v1/clusters/{id}", s.updateCluster)
	api.HandleFunc("DELETE /api/v1/clusters/{id}", s.deleteCluster)
	api.HandleFunc("POST /api/v1/clusters/{id}/probe", s.probeCluster)
	api.HandleFunc("POST /api/v1/clusters/{id}/sync", s.syncCluster)
	api.HandleFunc("GET /api/v1/clusters/{id}/manifests", s.clusterManifests)
	api.HandleFunc("POST /api/v1/clusters/{id}/selftest", s.startSelfTest)
	api.HandleFunc("GET /api/v1/clusters/{id}/selftest", s.getSelfTest)
	api.HandleFunc("POST /api/v1/sync", s.syncAll)
	api.HandleFunc("POST /api/v1/clusters/{id}/discover", s.discoverCluster)
	api.HandleFunc("POST /api/v1/discover", s.discoverAll)

	api.HandleFunc("GET /api/v1/models", s.listModels)
	api.HandleFunc("POST /api/v1/models", s.createModel)
	api.HandleFunc("PUT /api/v1/models/{id}", s.updateModel)
	api.HandleFunc("DELETE /api/v1/models/{id}", s.deleteModel)
	api.HandleFunc("PUT /api/v1/models/{id}/sites/{cluster_id}/drain", s.drainSite)
	api.HandleFunc("PUT /api/v1/models/{id}/fleet", s.setModelFleet)
	api.HandleFunc("PUT /api/v1/models/{id}/spent", s.setModelSpentMode)
	api.HandleFunc("PUT /api/v1/models/{id}/standard", s.setStandard)
	api.HandleFunc("DELETE /api/v1/models/{id}/standard/pending", s.cancelStandard)
	api.HandleFunc("GET /api/v1/models/{id}/prices", s.listPrices)
	api.HandleFunc("PUT /api/v1/models/{id}/prices", s.setPrices)
	api.HandleFunc("DELETE /api/v1/models/{id}/prices/pending", s.deletePendingPrices)
	api.HandleFunc("PUT /api/v1/models/{id}/prices/dry-run", s.setPriceDryRun)
	api.HandleFunc("PUT /api/v1/models/{id}/market-id", s.setMarketID)
	api.HandleFunc("GET /api/v1/market", s.searchMarket)
	api.HandleFunc("GET /api/v1/overage", s.listOverage)
	api.HandleFunc("GET /api/v1/capacity", s.capacityPlans)
	api.HandleFunc("PUT /api/v1/models/{id}/target-utilization", s.setTargetUtilization)
	api.HandleFunc("PUT /api/v1/models/{id}/overcommit-limit", s.setOvercommitLimit)
	api.HandleFunc("GET /api/v1/budgets", s.budgets)
	api.HandleFunc("PUT /api/v1/gpu-prices/{gpu}", s.setGPUPrice)
	api.HandleFunc("DELETE /api/v1/gpu-prices/{gpu}", s.deleteGPUPrice)
	api.HandleFunc("GET /api/v1/clusters/{id}/deployments/{namespace}/{name}/pods", s.deploymentPods)
	api.HandleFunc("POST /api/v1/budgets/split", s.split)
	api.HandleFunc("GET /api/v1/benchmark-profiles", s.profiles)
	api.HandleFunc("PUT /api/v1/benchmark-profile", s.setProfile)
	api.HandleFunc("GET /api/v1/models/{id}/capacity-runs", s.listCapacityRuns)
	api.HandleFunc("POST /api/v1/models/{id}/capacity-runs", s.startCapacityRun)
	api.HandleFunc("GET /api/v1/capacity-runs/{id}", s.getCapacityRun)
	api.HandleFunc("POST /api/v1/capacity-runs/{id}/cancel", s.cancelCapacityRun)
	api.HandleFunc("DELETE /api/v1/capacity-runs/{id}", s.deleteCapacityRun)

	api.HandleFunc("GET /api/v1/tenants", s.listTenants)
	api.HandleFunc("POST /api/v1/tenants", s.createTenant)
	api.HandleFunc("GET /api/v1/tenants/{id}", s.getTenant)
	api.HandleFunc("PUT /api/v1/tenants/{id}", s.updateTenant)
	api.HandleFunc("DELETE /api/v1/tenants/{id}", s.deleteTenant)
	api.HandleFunc("POST /api/v1/tenants/{id}/keys", s.createKey)
	api.HandleFunc("DELETE /api/v1/keys/{id}", s.revokeKey)
	api.HandleFunc("PUT /api/v1/tenants/{id}/wallet", s.setWallet)
	api.HandleFunc("GET /api/v1/tenants/{id}/wallet", s.getWallet)
	api.HandleFunc("GET /api/v1/signing-keys", s.signingKeys)
	api.HandleFunc("POST /api/v1/signing-keys/rotate", s.rotateSigningKey)
	api.HandleFunc("PUT /api/v1/tenants/{id}/quotas", s.upsertQuota)
	api.HandleFunc("DELETE /api/v1/quotas/{id}", s.deleteQuota)

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	// A tenant's own usage, by one of its API keys and not the admin token.
	root.Handle("GET /api/v1/my/usage", s.tenantAuth(http.HandlerFunc(s.getMyUsage)))
	root.Handle("/api/", s.auth(s.audit(api)))
	root.HandleFunc("/", s.ui)
	return root
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.adminToken)) != 1 {
			if ok {
				// A wrong token is worth a line; a missing one is every probe and scanner.
				slog.Warn("request with a wrong token", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr, "forwarded_for", clip(r.Header.Get("X-Forwarded-For"), 200))
			}
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

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	o, err := s.st.Overview(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}
