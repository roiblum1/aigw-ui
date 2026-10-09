package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aigw-ui/internal/api"
	"aigw-ui/internal/config"
	"aigw-ui/internal/secretbox"
	"aigw-ui/internal/selftest"
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
	if names, err := st.FleetModels(ctx); err == nil && len(names) > 0 && !st.FleetConfigured() {
		// Not fatal: keys and quotas must still reach the clusters. The
		// entry routes stay on the clusters as they are until this is fixed.
		slog.Error("FLEET_DOMAIN or FLEET_PEER_SNI is not set, but models have an entry route; their routes are left as they are and get no weight changes", "models", names)
	}

	sy := syncer.New(st, cfg.AutoSync, cfg.DiscoverEvery, cfg.SyncEvery)
	go sy.Run(ctx)

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

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.New(st, sy, usageService, selftest.New(ctx, st, sy, usageService), cfg.AdminToken, cfg.UIDir).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	slog.Info("listening", "addr", cfg.ListenAddr, "auto_sync", cfg.AutoSync, "discovery_interval", cfg.DiscoverEvery.String(), "sync_interval", cfg.SyncEvery.String(), "usage_monitoring", usageService != nil)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
