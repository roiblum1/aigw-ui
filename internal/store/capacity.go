package store

import (
	"context"
	"errors"
	"slices"
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
	// Pools are the InferencePools the model is served through.
	Pools []Pool
	// Deployments are its LLMInferenceServices on the cluster, with their
	// shapes.
	Deployments []Deployment
	// Basis is BasisMeasured when Capacity comes from the measured shapes of
	// the deployments, and BasisDeclared ("" too) when it comes from the
	// annotation aigw-ui.io/capacity-per-instance.
	Basis string
}

// What a site's capacity is counted in.
const (
	BasisDeclared = "declared"
	BasisMeasured = "measured"
)

// Pool names an InferencePool on a cluster.
type Pool struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Group     string `json:"group"`
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
	// Basis is "measured" when Observed comes from the measured shapes of
	// the deployments, "declared" when it comes from the annotation.
	Basis string `json:"basis"`
	// Pools are the InferencePools the model is served through here. The
	// class "best-effort" can only be created where there is one.
	Pools []Pool `json:"pools"`
}

// ErrLastSite is returned when a drain would leave a model with no site.
var ErrLastSite = errors.New("this is the last site with capacity for the model")

// ApplyCapacity records what one cluster serves of each model and moves the
// applied capacities one step towards it. reported is everything the cluster
// has a deployment for: a model that is not in it is not served there, and
// its capacity goes to 0 by the same rules as any other loss of capacity.
// RefreshFleetZones turns the result into zone weights.
//
// A reported model is matched to an endpoint by the model's name here, or by
// the name the cluster's route sends to the backend.
func (s *Store) ApplyCapacity(ctx context.Context, clusterID string, reported []Capacity) error {
	byName := make(map[string]Capacity, len(reported))
	for _, c := range reported {
		byName[c.Model] = c
	}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT e.id, e.model_id, m.name, e.upstream_model, e.backends, e.capacity_applied, e.capacity_low_streak, e.capacity_step, e.drained,
			        e.capacity_basis
			 FROM model_endpoints e JOIN models m ON m.id = e.model_id WHERE e.cluster_id = $1 FOR UPDATE OF e`, clusterID)
		if err != nil {
			return err
		}
		type endpoint struct {
			id, modelID, model, upstream string
			backends                     []BackendRef
			applied                      *float64
			lowStreak                    int
			step                         float64
			drained                      bool
			basis                        string
		}
		endpoints, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (endpoint, error) {
			var e endpoint
			err := r.Scan(&e.id, &e.modelID, &e.model, &e.upstream, &e.backends, &e.applied, &e.lowStreak, &e.step, &e.drained, &e.basis)
			return e, err
		})
		if err != nil {
			return err
		}
		kept := []string{}
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
			if found {
				for _, d := range c.Deployments {
					if err := upsertDeployment(ctx, tx, clusterID, e.modelID, d); err != nil {
						return err
					}
					kept = append(kept, d.Namespace+"/"+d.Name)
				}
			}
			// A drained site that already sat at the lowest weight for a poll
			// leaves the model's zones. Ending the drain lists it again.
			unlisted := e.drained && (e.applied == nil || *e.applied == 0)
			switch {
			case found && !c.Known:
				// Served, ready count unknown: keep the last weight.
				_, err := tx.Exec(ctx,
					`UPDATE model_endpoints SET serving = true, capacity_detail = $2, revision = $3, max_model_len = $4, unlisted = $5, pools = $6 WHERE id = $1`,
					e.id, c.Detail, c.Revision, c.MaxModelLen, unlisted, pools(c))
				if err != nil {
					return err
				}
				continue
			case !found && e.applied == nil:
				// Never served here: nothing to step down.
				if _, err := tx.Exec(ctx, `UPDATE model_endpoints SET serving = false, unlisted = $2, pools = '[]' WHERE id = $1`, e.id, unlisted); err != nil {
					return err
				}
				continue
			case !found:
				c = Capacity{Step: e.step, Detail: "no deployment of the model on this cluster", Basis: e.basis}
			}
			if c.Basis == "" {
				c.Basis = BasisDeclared
			}
			target := c.Capacity
			if e.drained {
				target = 0
			}
			// A change of basis changes the unit: the new value is taken at
			// once, as a first reading. Stepped one instance per poll, the
			// sites of the model would compare a new unit with an old one
			// for many polls.
			applied, lowStreak := e.applied, e.lowStreak
			if c.Basis != e.basis {
				applied, lowStreak = nil, 0
			}
			next, streak := weights.Next(applied, target, c.Step, lowStreak, e.drained)
			moved := e.applied == nil || next != *e.applied
			_, err := tx.Exec(ctx,
				`UPDATE model_endpoints SET serving = $8, capacity_observed = $2, capacity_step = $3, capacity_detail = $4, capacity_observed_at = now(),
				        capacity_applied = $5, capacity_low_streak = $6, revision = $9, max_model_len = $10, unlisted = $11, pools = $12,
				        capacity_basis = $13, capacity_changed_at = CASE WHEN $7 THEN now() ELSE capacity_changed_at END
				 WHERE id = $1`,
				e.id, c.Capacity, c.Step, c.Detail, next, streak, moved, found, c.Revision, c.MaxModelLen, unlisted, pools(c), c.Basis)
			if err != nil {
				return err
			}
		}
		// A deployment that is gone, or no longer serves a model in the
		// catalog, is no longer counted.
		_, err = tx.Exec(ctx, `DELETE FROM model_deployments WHERE cluster_id = $1 AND NOT (namespace || '/' || name = ANY($2))`, clusterID, kept)
		return err
	})
	return mapErr(err)
}

// pools returns the reported pools, never nil: the column holds a list.
func pools(c Capacity) []Pool {
	if c.Pools == nil {
		return []Pool{}
	}
	return c.Pools
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
				`SELECT count(*) FROM model_endpoints e JOIN clusters c ON c.id = e.cluster_id
				 WHERE e.model_id = $1 AND e.cluster_id <> $2 AND c.fleet_enabled AND NOT e.drained AND e.capacity_applied > 0`, modelID, clusterID).Scan(&others)
			if err != nil {
				return err
			}
			if others == 0 {
				return ErrLastSite
			}
		}
		// unlisted is cleared either way: a new drain steps down first, and
		// the end of one lists the site again at the lowest weight.
		_, err = tx.Exec(ctx, `UPDATE model_endpoints SET drained = $3, unlisted = false WHERE model_id = $1 AND cluster_id = $2`, modelID, clusterID, drained)
		return err
	})
	return model, cluster, mapErr(err)
}

// RefreshFleetZones works out every model's zone weights from the applied
// capacities, once for the whole fleet, and stores them. Rendering reads the
// stored weights, so every cluster gets the same ones. It returns the names
// of the models whose zones changed.
//
// A site is listed for a model when its cluster is in the fleet, it serves
// the model or still has capacity applied, and it is not drained out. A model
// that had zones and would be left with none keeps the ones it has, with the
// reason in fleet_error: rendering it without a site would make it
// unreachable everywhere.
func (s *Store) RefreshFleetZones(ctx context.Context) ([]string, error) {
	var changed []string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT m.id, c.name, e.capacity_applied
			 FROM model_endpoints e JOIN models m ON m.id = e.model_id JOIN clusters c ON c.id = e.cluster_id
			 WHERE c.fleet_enabled AND NOT e.unlisted AND (e.serving OR COALESCE(e.capacity_applied, 0) > 0)`)
		if err != nil {
			return err
		}
		listed := map[string][]weights.Site{}
		for rows.Next() {
			var id string
			var site weights.Site
			if err := rows.Scan(&id, &site.Zone, &site.Capacity); err != nil {
				rows.Close()
				return err
			}
			listed[id] = append(listed[id], site)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `SELECT id, name, fleet_zones, fleet_error FROM models ORDER BY name FOR UPDATE`)
		if err != nil {
			return err
		}
		type model struct {
			id, name, fleetError string
			zones                []weights.Zone
		}
		models, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (model, error) {
			var m model
			err := r.Scan(&m.id, &m.name, &m.zones, &m.fleetError)
			return m, err
		})
		if err != nil {
			return err
		}
		for _, m := range models {
			next, problem := weights.Zones(listed[m.id]), ""
			if len(next) == 0 && len(m.zones) > 0 {
				next, problem = m.zones, "no site is left that serves the model; the last sites and weights are kept"
			}
			if slices.Equal(next, m.zones) && problem == m.fleetError {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE models SET fleet_zones = $2, fleet_error = $3 WHERE id = $1`, m.id, next, problem); err != nil {
				return err
			}
			if !slices.Equal(next, m.zones) {
				changed = append(changed, m.name)
			}
		}
		return nil
	})
	return changed, mapErr(err)
}

// SiteWeight returns the weight the entry route gives a model's site now,
// nil before it has one.
func (s *Store) SiteWeight(ctx context.Context, modelID, clusterID string) (*float64, error) {
	var w *float64
	err := s.db.QueryRow(ctx, `SELECT capacity_applied FROM model_endpoints WHERE model_id = $1 AND cluster_id = $2`, modelID, clusterID).Scan(&w)
	return w, mapErr(err)
}
