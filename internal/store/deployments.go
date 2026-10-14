package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Deployment is one LLMInferenceService of a model on a cluster, with its
// shape: what one instance is and the GPUs it holds.
type Deployment struct {
	ClusterID   string `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	ModelID     string `json:"model_id"`
	// Topology is "single", "multi-node" or "prefill-decode".
	Topology string `json:"topology"`
	// GPU is the node label nvidia.com/gpu.product of its pods, or "".
	GPU             string  `json:"gpu"`
	GPUsPerInstance float64 `json:"gpus_per_instance"`
	Revision        string  `json:"revision"`
	// Instances is nil until the deployment reports its ready count.
	Instances *float64 `json:"instances"`
	// Drained is set when an operator drained the model's site.
	Drained    bool      `json:"drained"`
	ObservedAt time.Time `json:"observed_at"`
}

func upsertDeployment(ctx context.Context, tx pgx.Tx, clusterID, modelID string, d Deployment) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO model_deployments (cluster_id, namespace, name, model_id, topology, gpu, gpus_per_instance, revision, instances, observed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
		 ON CONFLICT (cluster_id, namespace, name) DO UPDATE SET model_id = $4, topology = $5, gpu = $6, gpus_per_instance = $7,
		     revision = $8, instances = $9, observed_at = now()`,
		clusterID, d.Namespace, d.Name, modelID, d.Topology, d.GPU, d.GPUsPerInstance, d.Revision, d.Instances)
	return err
}

// ListDeployments returns every deployment of every model, by model, cluster
// and name. Drained is the drain of the model on that cluster.
func (s *Store) ListDeployments(ctx context.Context) ([]Deployment, error) {
	return list(ctx, s, func(r scanner) (Deployment, error) {
		var d Deployment
		err := r.Scan(&d.ClusterID, &d.ClusterName, &d.Namespace, &d.Name, &d.ModelID, &d.Topology, &d.GPU, &d.GPUsPerInstance,
			&d.Revision, &d.Instances, &d.Drained, &d.ObservedAt)
		return d, err
	}, `SELECT d.cluster_id, c.name, d.namespace, d.name, d.model_id, d.topology, d.gpu, d.gpus_per_instance, d.revision, d.instances,
	           COALESCE(e.drained, false), d.observed_at
	    FROM model_deployments d JOIN clusters c ON c.id = d.cluster_id
	    LEFT JOIN model_endpoints e ON e.model_id = d.model_id AND e.cluster_id = d.cluster_id
	    ORDER BY d.model_id, c.name, d.namespace, d.name`)
}

// GetDeployment returns one deployment of a model.
func (s *Store) GetDeployment(ctx context.Context, modelID, clusterID, namespace, name string) (Deployment, error) {
	all, err := s.ListDeployments(ctx)
	if err != nil {
		return Deployment{}, err
	}
	for _, d := range all {
		if d.ModelID == modelID && d.ClusterID == clusterID && d.Namespace == namespace && d.Name == name {
			return d, nil
		}
	}
	return Deployment{}, ErrNotFound
}

// GPUPrice is what one GPU costs an hour, all in.
type GPUPrice struct {
	GPU        string    `json:"gpu"`
	USDPerHour float64   `json:"usd_per_hour"`
	Note       string    `json:"note"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ListGPUPrices returns every GPU price, by GPU.
func (s *Store) ListGPUPrices(ctx context.Context) ([]GPUPrice, error) {
	return list(ctx, s, func(r scanner) (GPUPrice, error) {
		var p GPUPrice
		err := r.Scan(&p.GPU, &p.USDPerHour, &p.Note, &p.UpdatedAt)
		return p, err
	}, `SELECT gpu, usd_per_hour, note, updated_at FROM gpu_prices ORDER BY gpu`)
}

// SetGPUPrice stores the price of a GPU, in place of the one it had.
func (s *Store) SetGPUPrice(ctx context.Context, p GPUPrice) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO gpu_prices (gpu, usd_per_hour, note, updated_at) VALUES ($1, $2, $3, now())
		 ON CONFLICT (gpu) DO UPDATE SET usd_per_hour = $2, note = $3, updated_at = now()`, p.GPU, p.USDPerHour, p.Note)
	return mapErr(err)
}

// DeleteGPUPrice removes the price of a GPU.
func (s *Store) DeleteGPUPrice(ctx context.Context, gpu string) error {
	return affected(s.db.Exec(ctx, `DELETE FROM gpu_prices WHERE gpu = $1`, gpu))
}

// CapacityTarget is what a model's plan is worked out at.
type CapacityTarget struct {
	// Utilization is the share of the day its instances are expected to be
	// busy; the prices that pay for them are worked out at it.
	Utilization float64
	// OvercommitLimit is how far the tenants' budgets may add up beyond what
	// the instances serve a day at that share.
	OvercommitLimit float64
}

// SetTargetUtilization sets the share of the day a model's instances are
// expected to be busy, and returns the model's name.
func (s *Store) SetTargetUtilization(ctx context.Context, modelID string, share float64) (string, error) {
	return s.setModelTarget(ctx, modelID, "target_utilization", share)
}

// SetOvercommitLimit sets how far the budgets on a model may add up beyond
// what it can be given a day, and returns the model's name.
func (s *Store) SetOvercommitLimit(ctx context.Context, modelID string, limit float64) (string, error) {
	return s.setModelTarget(ctx, modelID, "overcommit_limit", limit)
}

// setModelTarget sets one of the columns of CapacityTarget. column is one of
// the two names above, never a value from a request.
func (s *Store) setModelTarget(ctx context.Context, modelID, column string, value float64) (string, error) {
	var name string
	err := s.db.QueryRow(ctx, `UPDATE models SET `+column+` = $2 WHERE id = $1 RETURNING name`, modelID, value).Scan(&name)
	return name, mapErr(err)
}

// CapacityTargets returns every model's targets, by model.
func (s *Store) CapacityTargets(ctx context.Context) (map[string]CapacityTarget, error) {
	type row struct {
		id string
		t  CapacityTarget
	}
	rows, err := list(ctx, s, func(r scanner) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.t.Utilization, &x.t.OvercommitLimit)
		return x, err
	}, `SELECT id, target_utilization, overcommit_limit FROM models`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]CapacityTarget, len(rows))
	for _, x := range rows {
		out[x.id] = x.t
	}
	return out, nil
}
