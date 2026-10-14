package store

import (
	"context"
	"time"

	"aigw-ui/internal/capacity"
)

// States of a capacity run.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// CapacityRun is one measurement of what an instance of a model serves.
type CapacityRun struct {
	ID          string `json:"id"`
	ModelID     string `json:"model_id"`
	ModelName   string `json:"model_name"`
	ClusterID   string `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	Namespace   string `json:"namespace"`
	JobName     string `json:"job_name"`
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
}

const runSelect = `SELECT r.id, r.model_id, m.name, r.cluster_id, c.name, r.namespace, r.job_name, r.instance, r.settings,
	r.status, r.detail, r.dataset, r.steps, r.created_at, r.finished_at
	FROM capacity_runs r JOIN models m ON m.id = r.model_id JOIN clusters c ON c.id = r.cluster_id`

func scanRun(r scanner) (CapacityRun, error) {
	var run CapacityRun
	err := r.Scan(&run.ID, &run.ModelID, &run.ModelName, &run.ClusterID, &run.ClusterName, &run.Namespace, &run.JobName,
		&run.Instance, &run.Settings, &run.Status, &run.Detail, &run.Dataset, &run.Steps, &run.CreatedAt, &run.FinishedAt)
	if run.Steps == nil {
		run.Steps = []capacity.Step{}
	}
	run.Result = capacity.Analyze(run.Settings, run.Steps)
	return run, err
}

// CreateCapacityRun records a run that is about to start. A model with a run
// that has not ended gives ErrConflict.
func (s *Store) CreateCapacityRun(ctx context.Context, id, modelID, clusterID, namespace, jobName, instance string, set capacity.Settings) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO capacity_runs (id, model_id, cluster_id, namespace, job_name, instance, settings, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 'running')`, id, modelID, clusterID, namespace, jobName, instance, set)
	return mapErr(err)
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

// LatestCapacityRuns returns the newest run of every model that found a load
// the instance serves at the promised latency.
func (s *Store) LatestCapacityRuns(ctx context.Context) ([]CapacityRun, error) {
	runs, err := list(ctx, s, scanRun, runSelect+` WHERE r.status = 'succeeded' ORDER BY r.model_id, r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	out := []CapacityRun{}
	for _, run := range runs {
		if run.Result.Best == nil || (len(out) > 0 && out[len(out)-1].ModelID == run.ModelID) {
			continue
		}
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

// FinishCapacityRun ends a run. A run that has already ended stays as it
// is, and gives ErrNotFound.
func (s *Store) FinishCapacityRun(ctx context.Context, id, status, detail string) error {
	return affected(s.db.Exec(ctx,
		`UPDATE capacity_runs SET status = $2, detail = $3, finished_at = now() WHERE id = $1 AND status = 'running'`, id, status, detail))
}

// DeleteCapacityRun removes a run that has ended.
func (s *Store) DeleteCapacityRun(ctx context.Context, id string) error {
	return affected(s.db.Exec(ctx, `DELETE FROM capacity_runs WHERE id = $1 AND status <> 'running'`, id))
}

// DayUsage is what one model was used for in its busiest recent day.
type DayUsage struct {
	ModelID string
	Unit    string
	Day     time.Time
	Used    int64
}

// BusiestDays returns, for every model, the day since from on which it was
// used the most, best-effort included, in the unit it is counted in now.
func (s *Store) BusiestDays(ctx context.Context, from time.Time) ([]DayUsage, error) {
	return list(ctx, s, func(r scanner) (DayUsage, error) {
		var d DayUsage
		err := r.Scan(&d.ModelID, &d.Unit, &d.Day, &d.Used)
		return d, err
	}, `SELECT DISTINCT ON (model_id, unit) model_id, unit, day, used FROM (
	        SELECT model_id, unit, date_trunc('day', hour, 'UTC') AS day, sum(used + best_effort)::bigint AS used
	        FROM usage_hours WHERE hour >= $1 GROUP BY model_id, unit, day
	    ) d ORDER BY model_id, unit, used DESC`, from)
}

// AllQuotas returns every tenant's quota on every model.
func (s *Store) AllQuotas(ctx context.Context) ([]Quota, error) {
	return list(ctx, s, func(r scanner) (Quota, error) {
		var q Quota
		err := r.Scan(&q.ID, &q.TenantID, &q.ModelID, &q.ModelName, &q.TokenLimit, &q.Window, &q.Shadow)
		return q, err
	}, `SELECT q.id, q.tenant_id, q.model_id, m.name, q.token_limit, q.window_size, q.shadow
	    FROM quotas q JOIN models m ON m.id = q.model_id JOIN tenants t ON t.id = q.tenant_id WHERE t.enabled`)
}
