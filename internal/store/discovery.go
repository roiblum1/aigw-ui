package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/render"
)

// ApplyDiscovery records the models found on one cluster. It creates models
// that are new, updates this cluster's discovered endpoints and drops the ones
// that are gone. It never deletes a model, because that would also delete the
// tenants' quotas on it, and it never touches manual endpoints. A model may
// have no backends: it is then listed, but a quota cannot be attached to it.
// It reports whether anything that affects rendering changed.
func (s *Store) ApplyDiscovery(ctx context.Context, clusterID string, found []DiscoveredModel) (bool, error) {
	changed := false
	var skipped []string
	names := make([]string, 0, len(found))

	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		for _, d := range found {
			slug := render.Slug(d.Name)
			if slug == "" {
				skipped = append(skipped, d.Name)
				continue
			}

			var modelID string
			err := tx.QueryRow(ctx, `SELECT id FROM models WHERE name = $1`, d.Name).Scan(&modelID)
			if errors.Is(err, pgx.ErrNoRows) {
				// ON CONFLICT covers a different model whose name gives the same slug.
				err = tx.QueryRow(ctx,
					`INSERT INTO models (name, slug) VALUES ($1, $2) ON CONFLICT DO NOTHING RETURNING id`,
					d.Name, slug).Scan(&modelID)
				if errors.Is(err, pgx.ErrNoRows) {
					skipped = append(skipped, d.Name)
					continue
				}
				changed = true
			}
			if err != nil {
				return err
			}
			names = append(names, d.Name)

			if d.Backends == nil {
				d.Backends = []BackendRef{}
			}
			backends, err := json.Marshal(d.Backends)
			if err != nil {
				return err
			}
			upstream := d.Name
			if len(d.Backends) > 0 {
				upstream = d.Backends[0].Model
			}
			tag, err := tx.Exec(ctx,
				`INSERT INTO model_endpoints (model_id, cluster_id, upstream_model, source, backends)
				 VALUES ($1, $2, $3, 'discovered', $4::jsonb)
				 ON CONFLICT (model_id, cluster_id) DO UPDATE
				     SET upstream_model = EXCLUDED.upstream_model, backends = EXCLUDED.backends
				     WHERE model_endpoints.source = 'discovered'
				       AND (model_endpoints.backends, model_endpoints.upstream_model) IS DISTINCT FROM (EXCLUDED.backends, EXCLUDED.upstream_model)`,
				modelID, clusterID, upstream, string(backends))
			if err != nil {
				return err
			}
			changed = changed || tag.RowsAffected() > 0
		}

		tag, err := tx.Exec(ctx,
			`DELETE FROM model_endpoints e USING models m
			 WHERE m.id = e.model_id AND e.cluster_id = $1 AND e.source = 'discovered' AND NOT (m.name = ANY($2))`,
			clusterID, names)
		if err != nil {
			return err
		}
		changed = changed || tag.RowsAffected() > 0

		msg := fmt.Sprintf("%d models found", len(names))
		if len(skipped) > 0 {
			msg += fmt.Sprintf("; skipped %q: the name cannot be used or clashes with another model", skipped)
		}
		_, err = tx.Exec(ctx, `UPDATE clusters SET discovery_message = $2, discovered_at = now() WHERE id = $1`, clusterID, msg)
		return err
	})
	return changed, mapErr(err)
}

func (s *Store) SetDiscoveryError(ctx context.Context, clusterID, message string) error {
	_, err := s.db.Exec(ctx, `UPDATE clusters SET discovery_message = $2, discovered_at = now() WHERE id = $1`,
		clusterID, "failed: "+message)
	return err
}

// QuotaCapable reports whether any cluster has something a quota on this model
// can attach to: a manual endpoint, or a discovered AIServiceBackend.
func (s *Store) QuotaCapable(ctx context.Context, modelID string) (bool, error) {
	var exists, capable bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM models WHERE id = $1),
		        EXISTS (SELECT 1 FROM model_endpoints WHERE model_id = $1 AND (source = 'manual' OR jsonb_array_length(backends) > 0))`,
		modelID).Scan(&exists, &capable)
	if err != nil {
		return false, mapErr(err)
	}
	if !exists {
		return false, ErrNotFound
	}
	return capable, nil
}
