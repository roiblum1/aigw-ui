package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/capacity"
)

// States of a capacity run.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// Phases of a running measurement.
const (
	// PhaseDraining waits for the entry route to send nothing to the site.
	PhaseDraining = "draining"
	// PhaseMeasuring has the Job running.
	PhaseMeasuring = "measuring"
)

// Who started a measurement.
const (
	ByOperator = "operator"
	ByCampaign = "campaign"
)

// CapacityRun is one measurement of what an instance of a model serves.
type CapacityRun struct {
	ID          string `json:"id"`
	ModelID     string `json:"model_id"`
	ModelName   string `json:"model_name"`
	ClusterID   string `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	Namespace   string `json:"namespace"`
	// Deployment is the LLMInferenceService measured, and Shape its shape
	// when the measurement started.
	Deployment string         `json:"deployment"`
	Shape      capacity.Shape `json:"shape"`
	JobName    string         `json:"job_name"`
	// Instance is the pod that was measured.
	Instance string            `json:"instance"`
	Settings capacity.Settings `json:"settings"`
	Status   string            `json:"status"`
	Detail   string            `json:"detail"`
	Dataset  *capacity.Dataset `json:"dataset"`
	Steps    []capacity.Step   `json:"steps"`
	// Result is worked out from the steps when the run is read.
	Result     capacity.Result `json:"result"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	// SpecHash is the deployment's spec when it was measured, "" for a
	// measurement of a shape from before 0.16, which counts for nothing.
	SpecHash string `json:"spec_hash"`
	// ProfileID and ProfileVersion are the benchmark profile it ran.
	ProfileID      *string `json:"profile_id"`
	ProfileVersion *int    `json:"profile_version"`
	// Mode is capacity.ModeDrained or ModeLive, "" before 0.16.
	Mode string `json:"mode"`
	// InstancesDuring is what the result is divided by for one instance.
	InstancesDuring float64 `json:"instances_during"`
	// Phase is where a running measurement is; DrainedSite is set when it
	// drained the site itself and restores it at the end.
	Phase       string `json:"phase"`
	DrainedSite bool   `json:"drained_site"`
	StartedBy   string `json:"started_by"`
}

// PerInstance is what one instance of the deployment serves by this run:
// its best step, divided over the decode replicas of a prefill/decode
// deployment. Nil when no step met the targets.
func (r CapacityRun) PerInstance() *capacity.Throughput {
	if r.Result.Best == nil {
		return nil
	}
	t := r.Settings.Throughput(*r.Result.Best).PerInstance(r.InstancesDuring)
	return &t
}

const runSelect = `SELECT r.id, r.model_id, m.name, r.cluster_id, c.name, r.namespace, r.deployment, r.topology, r.gpu, r.gpus_per_instance,
	r.revision, r.job_name, r.instance, r.settings,
	r.status, r.detail, r.dataset, r.steps, r.created_at, r.finished_at,
	r.spec_hash, r.profile_id, r.profile_version, r.instances_during, r.phase, r.drained_site, r.started_by
	FROM capacity_runs r JOIN models m ON m.id = r.model_id JOIN clusters c ON c.id = r.cluster_id`

func scanRun(r scanner) (CapacityRun, error) {
	var run CapacityRun
	err := r.Scan(&run.ID, &run.ModelID, &run.ModelName, &run.ClusterID, &run.ClusterName, &run.Namespace, &run.Deployment,
		&run.Shape.Topology, &run.Shape.GPU, &run.Shape.GPUsPerInstance, &run.Shape.Revision, &run.JobName, &run.Instance, &run.Settings, &run.Status, &run.Detail, &run.Dataset, &run.Steps, &run.CreatedAt, &run.FinishedAt,
		&run.SpecHash, &run.ProfileID, &run.ProfileVersion, &run.InstancesDuring, &run.Phase, &run.DrainedSite, &run.StartedBy)
	if run.Steps == nil {
		run.Steps = []capacity.Step{}
	}
	run.Mode = run.Settings.Mode
	run.Result = capacity.Analyze(run.Settings, run.Steps)
	return run, err
}

// NewRun is a run that is about to start.
type NewRun struct {
	ID, ModelID, ClusterID, Namespace, Deployment, Instance, JobName string
	Shape                                                            capacity.Shape
	Settings                                                         capacity.Settings
	SpecHash                                                         string
	Profile                                                          capacity.Profile
	InstancesDuring                                                  float64
	// Phase is PhaseDraining for a drained run, PhaseMeasuring for a live
	// one; DrainedSite says the run drains the site itself.
	Phase       string
	DrainedSite bool
	StartedBy   string
}

// CreateCapacityRun records a run that is about to start. A model with a run
// that has not ended gives ErrConflict. A run that drains its site drains it
// in the same transaction: the site is drained if and only if the run is
// recorded, and a drain that would leave the model with no site gives
// ErrLastSite.
func (s *Store) CreateCapacityRun(ctx context.Context, r NewRun) error {
	if r.InstancesDuring <= 0 {
		r.InstancesDuring = 1
	}
	if r.StartedBy == "" {
		r.StartedBy = ByOperator
	}
	var profileID *string
	var version *int
	if r.Profile.ID != "" {
		profileID, version = &r.Profile.ID, &r.Profile.Version
	}
	return mapErr(pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO capacity_runs (id, model_id, cluster_id, namespace, deployment, instance, topology, gpu, gpus_per_instance, revision,
			     job_name, settings, status, spec_hash, profile_id, profile_version, instances_during, phase, drained_site, started_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'running', $13, $14, $15, $16, $17, $18, $19)`,
			r.ID, r.ModelID, r.ClusterID, r.Namespace, r.Deployment, r.Instance, r.Shape.Topology, r.Shape.GPU, r.Shape.GPUsPerInstance,
			r.Shape.Revision, r.JobName, r.Settings, r.SpecHash, profileID, version, r.InstancesDuring, r.Phase, r.DrainedSite, r.StartedBy)
		if err != nil || !r.DrainedSite {
			return err
		}
		_, _, err = setDrained(ctx, tx, r.ModelID, r.ClusterID, true)
		return err
	}))
}

// SetRunPhase moves a running measurement to its next phase.
func (s *Store) SetRunPhase(ctx context.Context, id, phase string) error {
	return affected(s.db.Exec(ctx, `UPDATE capacity_runs SET phase = $2 WHERE id = $1 AND status = 'running'`, id, phase))
}

// GetCapacityRun returns one run.
func (s *Store) GetCapacityRun(ctx context.Context, id string) (CapacityRun, error) {
	run, err := scanRun(s.db.QueryRow(ctx, runSelect+` WHERE r.id = $1`, id))
	return run, mapErr(err)
}

// ListCapacityRuns returns the runs of a model, the newest first.
func (s *Store) ListCapacityRuns(ctx context.Context, modelID string) ([]CapacityRun, error) {
	return list(ctx, s, scanRun, runSelect+` WHERE r.model_id = $1 ORDER BY r.created_at DESC LIMIT 50`, modelID)
}

// RunningCapacityRuns returns the runs that have not ended.
func (s *Store) RunningCapacityRuns(ctx context.Context) ([]CapacityRun, error) {
	return list(ctx, s, scanRun, runSelect+` WHERE r.status = 'running' ORDER BY r.created_at`)
}

// LatestCapacityRuns returns, for every deployment, spec and profile
// version, the newest run that found a load the instance serves at the
// promised latency. Runs of shapes from before 0.16 are left out: they
// count for nothing.
func (s *Store) LatestCapacityRuns(ctx context.Context) ([]CapacityRun, error) {
	runs, err := list(ctx, s, scanRun, runSelect+` WHERE r.status = 'succeeded' AND r.spec_hash <> '' AND r.profile_version IS NOT NULL
	    ORDER BY r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	out := []CapacityRun{}
	seen := map[string]bool{}
	for _, run := range runs {
		key := fmt.Sprintf("%s|%s/%s|%s|%d", run.ClusterID, run.Namespace, run.Deployment, run.SpecHash, *run.ProfileVersion)
		if run.Result.Best == nil || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, run)
	}
	return out, nil
}

// UpdateCapacityRun stores what a running run has printed so far.
func (s *Store) UpdateCapacityRun(ctx context.Context, id string, dataset *capacity.Dataset, steps []capacity.Step) error {
	if steps == nil {
		steps = []capacity.Step{}
	}
	return affected(s.db.Exec(ctx, `UPDATE capacity_runs SET dataset = $2, steps = $3 WHERE id = $1 AND status = 'running'`, id, dataset, steps))
}

// FinishCapacityRun ends a run, and restores the site of a run that drained
// it, in one transaction: a run never ends with its drain left behind. A run
// that has already ended stays as it is, and gives ErrNotFound.
func (s *Store) FinishCapacityRun(ctx context.Context, id, status, detail string) error {
	return mapErr(pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var modelID, clusterID string
		var drainedSite bool
		err := tx.QueryRow(ctx,
			`UPDATE capacity_runs SET status = $2, detail = $3, finished_at = now() WHERE id = $1 AND status = 'running'
			 RETURNING model_id, cluster_id, drained_site`, id, status, detail).Scan(&modelID, &clusterID, &drainedSite)
		if err != nil || !drainedSite {
			return err
		}
		// The endpoint may be gone with the model's last deployment there:
		// nothing is left to restore then.
		_, err = tx.Exec(ctx, `UPDATE model_endpoints SET drained = false, unlisted = false WHERE model_id = $1 AND cluster_id = $2`, modelID, clusterID)
		return err
	}))
}

// DeleteCapacityRun removes a run that has ended.
func (s *Store) DeleteCapacityRun(ctx context.Context, id string) error {
	return affected(s.db.Exec(ctx, `DELETE FROM capacity_runs WHERE id = $1 AND status <> 'running'`, id))
}

// RecentUsage is what one model was used for since a point in time, in one
// unit, best-effort included.
type RecentUsage struct {
	ModelID string
	Unit    string
	// Busiest is the most in one day (UTC), Total all of it.
	Busiest int64
	Total   int64
}

// UsageSince returns, for every model and unit, its use since from.
func (s *Store) UsageSince(ctx context.Context, from time.Time) ([]RecentUsage, error) {
	return list(ctx, s, func(r scanner) (RecentUsage, error) {
		var u RecentUsage
		err := r.Scan(&u.ModelID, &u.Unit, &u.Busiest, &u.Total)
		return u, err
	}, `SELECT model_id, unit, max(used)::bigint, sum(used)::bigint FROM (
	        SELECT model_id, unit, date_trunc('day', hour, 'UTC') AS day, sum(used + best_effort) AS used
	        FROM usage_hours WHERE hour >= $1 GROUP BY model_id, unit, day
	    ) d GROUP BY model_id, unit`, from)
}

// HourUse is what one model was used for in one hour, in one unit, over
// every tenant, best-effort included.
type HourUse struct {
	ModelID string
	Unit    string
	Hour    time.Time
	Used    int64
}

// HourlyUse returns every model's use, hour by hour, since from.
func (s *Store) HourlyUse(ctx context.Context, from time.Time) ([]HourUse, error) {
	return list(ctx, s, func(r scanner) (HourUse, error) {
		var u HourUse
		err := r.Scan(&u.ModelID, &u.Unit, &u.Hour, &u.Used)
		return u, err
	}, `SELECT model_id, unit, hour, sum(used + best_effort)::bigint FROM usage_hours WHERE hour >= $1
	    GROUP BY model_id, unit, hour ORDER BY model_id, hour`, from)
}

// AllQuotas returns every enabled tenant's quota on every model, each
// marked Budgeted when its tenant has a budget across models too.
func (s *Store) AllQuotas(ctx context.Context) ([]Quota, error) {
	return list(ctx, s, func(r scanner) (Quota, error) {
		var q Quota
		err := r.Scan(&q.ID, &q.TenantID, &q.ModelID, &q.ModelName, &q.TokenLimit, &q.Window, &q.Shadow, &q.Budgeted)
		return q, err
	}, `SELECT q.id, q.tenant_id, q.model_id, m.name, q.token_limit, q.window_size, q.shadow,
	           t.budget_credits IS NOT NULL AND t.budget_window IS NOT NULL
	    FROM quotas q JOIN models m ON m.id = q.model_id JOIN tenants t ON t.id = q.tenant_id WHERE t.enabled`)
}
