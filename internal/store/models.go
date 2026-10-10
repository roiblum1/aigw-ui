package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/render"
)

type ModelInput struct {
	Name           string
	DefaultLimit   int64
	DefaultWindow  string
	CostExpression string
	Endpoints      []Endpoint
	// KeepEndpoints leaves the manual endpoints as they are on an update.
	KeepEndpoints bool
}

func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, slug, default_limit, default_window, cost_expression, created_at, fleet_zones, fleet_error, fleet, spent_mode, best_effort_unlimited FROM models ORDER BY name`)
	if err != nil {
		return nil, err
	}
	models, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Model, error) {
		m := Model{Endpoints: []Endpoint{}, Warnings: []string{}}
		err := r.Scan(&m.ID, &m.Name, &m.Slug, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &m.CreatedAt, &m.SiteWeights, &m.SiteWeightsNote, &m.Fleet, &m.SpentMode, &m.BestEffortUnlimited)
		return m, err
	})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*Model, len(models))
	for i := range models {
		byID[models[i].ID] = &models[i]
	}
	rows, err = s.db.Query(ctx,
		`SELECT e.model_id, e.cluster_id, c.name, e.host, e.port, e.upstream_model, e.source, e.backends,
		        e.capacity_observed, e.capacity_observed_at, e.capacity_applied, e.capacity_changed_at, e.capacity_detail,
		        e.serving, e.drained, e.revision, e.max_model_len, e.pools
		 FROM model_endpoints e JOIN clusters c ON c.id = e.cluster_id ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var modelID string
		var e Endpoint
		if err := rows.Scan(&modelID, &e.ClusterID, &e.ClusterName, &e.Host, &e.Port, &e.UpstreamModel, &e.Source, &e.Backends,
			&e.Capacity.Observed, &e.Capacity.ObservedAt, &e.Capacity.Weight, &e.Capacity.ChangedAt, &e.Capacity.Detail,
			&e.Capacity.Serving, &e.Capacity.Drained, &e.Capacity.Revision, &e.Capacity.MaxModelLen, &e.Capacity.Pools); err != nil {
			return nil, err
		}
		if m := byID[modelID]; m != nil {
			m.Endpoints = append(m.Endpoints, e)
			m.QuotaCapable = m.QuotaCapable || e.Source == SourceManual || len(e.Backends) > 0
		}
	}
	return models, rows.Err()
}

func (s *Store) CreateModel(ctx context.Context, in ModelInput) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO models (name, slug, default_limit, default_window, cost_expression) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			in.Name, render.Slug(in.Name), in.DefaultLimit, in.DefaultWindow, in.CostExpression).Scan(&id)
		if err != nil {
			return err
		}
		return replaceEndpoints(ctx, tx, id, in)
	})
	return id, mapErr(err)
}

// UpdateModel does not change the slug: it names the objects on the clusters
// and is part of the rate limit counter key.
func (s *Store) UpdateModel(ctx context.Context, id string, in ModelInput) error {
	return mapErr(pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE models SET default_limit = $2, default_window = $3, cost_expression = $4 WHERE id = $1`,
			id, in.DefaultLimit, in.DefaultWindow, in.CostExpression)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if in.KeepEndpoints {
			return nil
		}
		return replaceEndpoints(ctx, tx, id, in)
	}))
}

// replaceEndpoints replaces the manual endpoints of a model. Discovered
// endpoints are left alone, except where a manual one now covers the cluster.
func replaceEndpoints(ctx context.Context, tx pgx.Tx, modelID string, in ModelInput) error {
	if _, err := tx.Exec(ctx, `DELETE FROM model_endpoints WHERE model_id = $1 AND source = 'manual'`, modelID); err != nil {
		return err
	}
	for _, e := range in.Endpoints {
		upstream := e.UpstreamModel
		if upstream == "" {
			upstream = in.Name
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO model_endpoints (model_id, cluster_id, host, port, upstream_model) VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (model_id, cluster_id) DO UPDATE SET host = EXCLUDED.host, port = EXCLUDED.port,
			     upstream_model = EXCLUDED.upstream_model, source = 'manual', backends = '[]'`,
			modelID, e.ClusterID, e.Host, e.Port, upstream); err != nil {
			return err
		}
	}
	return nil
}

// GetModel returns a model's own settings, without its endpoints.
func (s *Store) GetModel(ctx context.Context, id string) (Model, error) {
	var m Model
	err := s.db.QueryRow(ctx, `SELECT id, name, slug, default_limit, default_window, cost_expression, created_at FROM models WHERE id = $1`, id).
		Scan(&m.ID, &m.Name, &m.Slug, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &m.CreatedAt)
	return m, mapErr(err)
}

func (s *Store) DeleteModel(ctx context.Context, id string) error {
	return affected(s.db.Exec(ctx, `DELETE FROM models WHERE id = $1`, id))
}

var (
	// ErrFleetNoSite is returned when an entry route is asked for a model
	// that no fleet cluster serves.
	ErrFleetNoSite = errors.New("no fleet cluster serves the model")
	// ErrFleetManual is returned when an entry route is asked for a model
	// that also has endpoints entered by hand.
	ErrFleetManual = errors.New("the model has manual endpoints")
	// ErrBestEffortOn is returned when the entry route of a model in
	// best-effort mode is turned off: the mode is built on that route.
	ErrBestEffortOn = errors.New("the model is in best-effort mode")
	// ErrNoEntryRoute is returned when best-effort mode is asked for a model
	// without an entry route.
	ErrNoEntryRoute = errors.New("the model has no entry route")
)

// SetModelFleet turns the entry route of a model on or off and returns the
// model's name.
func (s *Store) SetModelFleet(ctx context.Context, id string, on bool) (string, error) {
	var name string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var sites, manual int
		var mode string
		err := tx.QueryRow(ctx,
			`SELECT m.name, m.spent_mode,
			        (SELECT count(*) FROM jsonb_array_elements(m.fleet_zones) z JOIN clusters c ON c.name = z->>'zone'
			         WHERE c.fleet_enabled AND c.peer_host <> ''),
			        (SELECT count(*) FROM model_endpoints e WHERE e.model_id = m.id AND e.source = 'manual')
			 FROM models m WHERE m.id = $1 FOR UPDATE`, id).Scan(&name, &mode, &sites, &manual)
		if err != nil {
			return err
		}
		switch {
		case !on && mode == SpentBestEffort:
			return ErrBestEffortOn
		case on && manual > 0:
			return ErrFleetManual
		case on && sites == 0:
			return ErrFleetNoSite
		}
		_, err = tx.Exec(ctx, `UPDATE models SET fleet = $2 WHERE id = $1`, id, on)
		return err
	})
	return name, mapErr(err)
}
