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
	"aigw-ui/internal/store"
	"aigw-ui/internal/syncer"
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

	sy := syncer.New(st, cfg.AutoSync, cfg.DiscoverEvery, cfg.SyncEvery)
	go sy.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.New(st, sy, cfg.AdminToken, cfg.UIDir).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	slog.Info("listening", "addr", cfg.ListenAddr, "auto_sync", cfg.AutoSync, "discovery_interval", cfg.DiscoverEvery.String(), "sync_interval", cfg.SyncEvery.String())
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
