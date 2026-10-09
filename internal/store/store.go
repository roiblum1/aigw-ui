// Package store is the Postgres access layer. It holds the desired state:
// clusters, models, tenants, keys and quotas, plus the task log. Secrets are
// encrypted before they are written.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"aigw-ui/internal/render"
	"aigw-ui/internal/secretbox"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

type Store struct {
	db  *pgxpool.Pool
	box *secretbox.Box
	// fleet is what every site's entry route has in common.
	fleet render.FleetConfig
}

// SetFleet sets the shared settings of the entry routes. Call it before the
// store is used.
func (s *Store) SetFleet(cfg render.FleetConfig) { s.fleet = cfg }

// SessionHeader is the first of the request headers that carry the
// conversation key, or "" when none is set.
func (s *Store) SessionHeader() string {
	if len(s.fleet.SessionHeaders) == 0 {
		return ""
	}
	return s.fleet.SessionHeaders[0]
}

// FleetConfigured reports whether entry routes can be rendered at all.
func (s *Store) FleetConfigured() bool { return s.fleet.PeerSNI != "" }

func Open(ctx context.Context, url string, box *secretbox.Box) (*Store, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	// On a fresh install the database is often still starting. Wait for it
	// rather than exit and be restarted by the platform.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err = db.Ping(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			db.Close()
			return nil, fmt.Errorf("connect to postgres: %w", err)
		}
		slog.Info("waiting for postgres", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	s := &Store{db: db, box: box}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() { s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// mapErr translates driver errors into the package's sentinel errors.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return ErrConflict
		case "23503", "22P02": // missing reference, malformed uuid
			return ErrNotFound
		}
	}
	return err
}

func affected(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Overview(ctx context.Context) (Overview, error) {
	var o Overview
	err := s.db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM clusters),
		(SELECT count(*) FROM clusters WHERE sync_status = 'error'),
		(SELECT count(*) FROM models),
		(SELECT count(*) FROM tenants),
		(SELECT count(*) FROM api_keys WHERE revoked_at IS NULL),
		(SELECT count(*) FROM quotas)`).
		Scan(&o.Clusters, &o.ClustersError, &o.Models, &o.Tenants, &o.ActiveKeys, &o.Quotas)
	return o, err
}
