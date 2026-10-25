package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"aigw-ui/internal/api"
	"aigw-ui/internal/benchmark"
	"aigw-ui/internal/config"
	"aigw-ui/internal/gitops"
	"aigw-ui/internal/history"
	"aigw-ui/internal/overage"
	"aigw-ui/internal/secretbox"
	"aigw-ui/internal/selftest"
	"aigw-ui/internal/sso"
	"aigw-ui/internal/store"
	"aigw-ui/internal/syncer"
	"aigw-ui/internal/usage"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// The collector works harder as the heap nears the container's limit,
	// rather than the platform killing the pod there. A GOMEMLIMIT of the
	// operator's own wins.
	if cfg.MemoryLimit > 0 && os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(cfg.MemoryLimit / 10 * 9)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	box, err := secretbox.New(cfg.EncryptionKey)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, box)
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetFleet(cfg.Fleet)
	// The token counters are only worth counting where the hub reads them.
	st.SetTokenCounts(cfg.RedisURL != "" && cfg.RedisTokenCounts)
	st.SetTokens(store.TokenSettings{Issuer: cfg.TokenIssuer, Audience: cfg.TokenAudience, Lifetime: cfg.TokenLifetime,
		RenewedLifetime: cfg.TokenRenewedLifetime})
	if names, err := st.FleetModels(ctx); err == nil && len(names) > 0 && !st.FleetConfigured() {
		// Not fatal: keys and quotas must still reach the clusters. The
		// entry routes stay on the clusters as they are until this is fixed.
		slog.Error("FLEET_DOMAIN or FLEET_PEER_SNI is not set, but models have an entry route; their routes are left as they are and get no weight changes", "models", names)
	}

	sy := syncer.New(st, cfg.AutoSync, cfg.DiscoverEvery, cfg.SyncEvery)
	go sy.Run(ctx)
	go sy.RunPrices(ctx)

	// Usage monitoring is optional and must not keep the server from starting:
	// Redis being down only makes the usage page report an error.
	var usageService *usage.Service
	if cfg.RedisURL != "" {
		reader, err := usage.New(usage.Options{URL: cfg.RedisURL, CAFile: cfg.RedisCAFile, TLSInsecure: cfg.RedisTLSInsecure, KeyPrefix: cfg.RedisKeyPrefix, AllowReset: cfg.RedisAllowReset})
		if err != nil {
			return err
		}
		defer reader.Close()
		if err := reader.Ping(ctx); err != nil {
			slog.Warn("redis is not reachable; usage will be unavailable until it is", "err", err)
		}
		usageService = usage.NewService(st, reader)
	}
	// Moving tenants to best-effort needs the counters, so it runs only
	// with Redis. Without it a spent budget is refused, as before.
	var overageService *overage.Service
	var hist *history.Service
	if usageService != nil {
		overageService = overage.New(st, usageService, sy, cfg.OverageEvery, cfg.OverageThreshold)
		go overageService.Run(ctx)
		if cfg.HistoryEvery > 0 {
			hist = history.New(st, usageService, cfg.HistoryEvery)
			go hist.Run(ctx)
		}
	}

	// Measurements share each cluster's client with the sync.
	bench := benchmark.New(st, func(ctx context.Context, clusterID string) (benchmark.Cluster, error) {
		client, err := sy.Client(ctx, clusterID)
		if err != nil {
			return nil, err
		}
		return client, nil
	}, benchmark.Options{Image: cfg.CapacityImage, Every: cfg.CapacityEvery, Window: cfg.CapacityWindow, PeerMeasurements: cfg.CapacityPeer,
		Peer: benchmark.Peer{SNI: cfg.Fleet.PeerSNI, ClientSecret: cfg.Fleet.ClientSecret, CAConfigMap: cfg.Fleet.CAConfigMap}})
	go bench.Run(ctx)

	repo, apps, err := deployments(cfg)
	if err != nil {
		return err
	}

	// Sign-in with OpenShift for tenants linked to a Group: the Groups are
	// read every minute, and a person who left loses their key.
	var cluster *sso.OpenShift
	if cfg.SSOOpenShift {
		c, err := sso.InCluster(sso.Options{ServiceAccount: cfg.SSOServiceAccount, CAFile: cfg.SSOCAFile, TokenURL: cfg.SSOTokenURL,
			APIServer: cfg.SSOAPIServer, TokenFile: cfg.SSOTokenFile, Namespace: cfg.SSONamespace})
		if err != nil {
			return err
		}
		cluster = sso.New(c)
		go sso.NewWatcher(cluster, st, sy.Changed, time.Minute).Run(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.New(st, sy, usageService, selftest.New(ctx, st, sy, usageService, overageService), bench, cfg.AdminToken, cfg.UIDir, cfg.TenantPage).WithUpstreamDocs(cfg.UpstreamDocsDir).WithGatewayURL(cfg.GatewayURL).WithClaudeCode(cfg.ClaudeCodeModel, cfg.ClaudeCodeSmallModel).WithHistory(historyOf(hist)).WithGitOps(deployerOf(repo), appsOf(apps), cfg.ArgoCDAppName).WithSSO(cluster, sso.NewSealer(cfg.EncryptionKey)).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	slog.Info("listening", "addr", cfg.ListenAddr, "auto_sync", cfg.AutoSync, "discovery_interval", cfg.DiscoverEvery.String(), "sync_interval", cfg.SyncEvery.String(), "usage_monitoring", usageService != nil, "openshift_sign_in", cluster != nil)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// historyOf hands the API the history, or none: a nil *history.Service in
// the interface would not compare equal to nil.
func historyOf(h *history.Service) interface {
	Tick(ctx context.Context, now time.Time) error
} {
	if h == nil {
		return nil
	}
	return h
}

// deployerOf and appsOf keep a nil pointer from becoming an interface that
// is not nil.
func deployerOf(r *gitops.Repo) interface {
	Read(ctx context.Context) (gitops.Snapshot, error)
	Commit(ctx context.Context, changes []gitops.Change, message string) (string, error)
	Where() (repo, branch, dir string)
} {
	if r == nil {
		return nil
	}
	return r
}

func appsOf(a *gitops.ArgoCD) interface {
	Apps(ctx context.Context) (map[string]gitops.App, error)
} {
	if a == nil {
		return nil
	}
	return a
}

// deployments opens the git repository of the models the hub deploys, and
// Argo CD's Applications for their status. Both nil when GITOPS_REPO is not
// set. Argo CD's status is optional: without access to it the page shows
// what git and discovery say.
func deployments(cfg *config.Config) (repo *gitops.Repo, apps *gitops.ArgoCD, err error) {
	if cfg.GitOps.Repo == "" {
		return nil, nil, nil
	}
	if repo, err = gitops.New(cfg.GitOps); err != nil {
		return nil, nil, err
	}
	if cfg.ArgoCDNamespace != "" {
		if apps, err = gitops.NewArgoCD(cfg.ArgoCDNamespace, cfg.ArgoCDKubeconfig); err != nil {
			slog.Warn("deployments show no Argo CD status", "err", err)
			apps, err = nil, nil
		}
	}
	return repo, apps, nil
}
