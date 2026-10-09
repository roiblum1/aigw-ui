package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/weights"
)

// Capacity is what one cluster reported for one model.
type Capacity struct {
	Model       string
	Capacity    float64
	Step        float64
	Detail      string
	Revision    string
	MaxModelLen string
	// Known is false when a deployment of the model has not reported its
	// ready count. The model is served, but its weight stays as it is.
	Known bool
}

// EndpointCapacity is the capacity of one model on one cluster as the API
// shows it. Observed and Weight are nil while unknown.
type EndpointCapacity struct {
	// Serving is true when the cluster has a deployment of the model.
	Serving bool `json:"serving"`
	// Drained is set by an operator; the weight goes to 0 and stays there.
	Drained bool `json:"drained"`
	// Observed is ready instances times the capacity of one, as last read.
	Observed   *float64   `json:"observed"`
	ObservedAt *time.Time `json:"observed_at"`
	// Weight is the value applied as the site's share of the model's
	// traffic. It follows Observed slowly.
	Weight      *float64   `json:"weight"`
	ChangedAt   *time.Time `json:"changed_at"`
	Detail      string     `json:"detail"`
	Revision    string     `json:"revision"`
	MaxModelLen string     `json:"max_model_len"`
}

// ErrLastSite is returned when a drain would leave a model with no site.
var ErrLastSite = errors.New("this is the last site with capacity for the model")

// ApplyCapacity records what one cluster serves of each model and moves the
// applied weights one step towards it. reported is everything the cluster
// has a deployment for: a model that is not in it is not served there, and
// its weight goes to 0 by the same rules as any other loss of capacity. It
// returns the names of the models whose applied weight changed.
//
// A reported model is matched to an endpoint by the model's name here, or by
// the name the cluster's route sends to the backend.
func (s *Store) ApplyCapacity(ctx context.Context, clusterID string, reported []Capacity) ([]string, error) {
	byName := make(map[string]Capacity, len(reported))
	for _, c := range reported {
		byName[c.Model] = c
	}
	var changed []string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT e.id, m.name, e.upstream_model, e.backends, e.capacity_applied, e.capacity_low_streak, e.capacity_step, e.drained
			 FROM model_endpoints e JOIN models m ON m.id = e.model_id WHERE e.cluster_id = $1 FOR UPDATE OF e`, clusterID)
		if err != nil {
			return err
		}
		type endpoint struct {
			id, model, upstream string
			backends            []BackendRef
			applied             *float64
			lowStreak           int
			step                float64
			drained             bool
		}
		endpoints, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (endpoint, error) {
			var e endpoint
			err := r.Scan(&e.id, &e.model, &e.upstream, &e.backends, &e.applied, &e.lowStreak, &e.step, &e.drained)
			return e, err
		})
		if err != nil {
			return err
		}
		for _, e := range endpoints {
			names := []string{e.model, e.upstream}
			for _, b := range e.backends {
				names = append(names, b.Model)
			}
			var c Capacity
			found := false
			for _, name := range names {
				if c, found = byName[name]; found {
					break
				}
			}
			switch {
			case found && !c.Known:
				// Served, ready count unknown: keep the last weight.
				_, err := tx.Exec(ctx,
					`UPDATE model_endpoints SET serving = true, capacity_detail = $2, revision = $3, max_model_len = $4 WHERE id = $1`,
					e.id, c.Detail, c.Revision, c.MaxModelLen)
				if err != nil {
					return err
				}
				continue
			case !found && e.applied == nil:
				// Never served here: nothing to step down.
				if _, err := tx.Exec(ctx, `UPDATE model_endpoints SET serving = false WHERE id = $1`, e.id); err != nil {
					return err
				}
				continue
			case !found:
				c = Capacity{Step: e.step, Detail: "no deployment of the model on this cluster"}
			}
			target := c.Capacity
			if e.drained {
				target = 0
			}
			next, streak := weights.Next(e.applied, target, c.Step, e.lowStreak, e.drained)
			moved := e.applied == nil || next != *e.applied
			_, err := tx.Exec(ctx,
				`UPDATE model_endpoints SET serving = $8, capacity_observed = $2, capacity_step = $3, capacity_detail = $4, capacity_observed_at = now(),
				        capacity_applied = $5, capacity_low_streak = $6, revision = $9, max_model_len = $10,
				        capacity_changed_at = CASE WHEN $7 THEN now() ELSE capacity_changed_at END
				 WHERE id = $1`,
				e.id, c.Capacity, c.Step, c.Detail, next, streak, moved, found, c.Revision, c.MaxModelLen)
			if err != nil {
				return err
			}
			if moved {
				changed = append(changed, e.model)
			}
		}
		return nil
	})
	return changed, mapErr(err)
}

// SetDrained starts or ends an operator drain of one model on one cluster and
// returns the model's and the cluster's name. It refuses to drain the last
// site that has capacity for the model.
func (s *Store) SetDrained(ctx context.Context, modelID, clusterID string, drained bool) (model, cluster string, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT m.name, c.name FROM model_endpoints e JOIN models m ON m.id = e.model_id JOIN clusters c ON c.id = e.cluster_id
			 WHERE e.model_id = $1 AND e.cluster_id = $2 FOR UPDATE OF e`, modelID, clusterID).Scan(&model, &cluster)
		if err != nil {
			return err
		}
		if drained {
			var others int
			err := tx.QueryRow(ctx,
				`SELECT count(*) FROM model_endpoints
				 WHERE model_id = $1 AND cluster_id <> $2 AND NOT drained AND capacity_applied > 0`, modelID, clusterID).Scan(&others)
			if err != nil {
				return err
			}
			if others == 0 {
				return ErrLastSite
			}
		}
		_, err = tx.Exec(ctx, `UPDATE model_endpoints SET drained = $3 WHERE model_id = $1 AND cluster_id = $2`, modelID, clusterID, drained)
		return err
	})
	return model, cluster, mapErr(err)
}

// ZoneWeights returns, for every model a fleet cluster has, the capacity
// applied on each fleet cluster. The cluster's name is the zone, and a
// cluster without the model is listed as not serving. The result is the same
// whichever cluster asks, which is what keeps every gateway choosing the same
// site for a conversation.
func (s *Store) ZoneWeights(ctx context.Context) (map[string][]weights.Site, error) {
	rows, err := s.db.Query(ctx,
		`SELECT m.name, c.name, COALESCE(e.serving, false), e.capacity_applied
		 FROM models m
		 CROSS JOIN clusters c
		 LEFT JOIN model_endpoints e ON e.model_id = m.id AND e.cluster_id = c.id
		 WHERE c.fleet_enabled
		   AND EXISTS (SELECT 1 FROM model_endpoints x JOIN clusters xc ON xc.id = x.cluster_id
		               WHERE x.model_id = m.id AND xc.fleet_enabled AND (x.serving OR x.capacity_applied IS NOT NULL))
		 ORDER BY m.name, c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]weights.Site{}
	for rows.Next() {
		var model string
		var site weights.Site
		if err := rows.Scan(&model, &site.Zone, &site.Serving, &site.Capacity); err != nil {
			return nil, err
		}
		out[model] = append(out[model], site)
	}
	return out, rows.Err()
}
