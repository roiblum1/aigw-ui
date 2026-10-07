package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// keepTasks is how many tasks the log holds. Older ones are removed.
const keepTasks = 500

// TaskResult is what happened on one cluster when a task was applied there.
type TaskResult struct {
	ClusterName string `json:"cluster_name"`
	// Status is "pending", "succeeded" or "failed". A failed result is tried
	// again by the next sync.
	Status  string `json:"status"`
	Message string `json:"message"`
	// Changes are the objects the sync created, updated or deleted, and
	// Rejected the ones the gateway reports as not accepted.
	Changes    json.RawMessage `json:"changes"`
	Rejected   json.RawMessage `json:"rejected"`
	FinishedAt *time.Time      `json:"finished_at"`
}

type Task struct {
	ID      int64  `json:"id"`
	Action  string `json:"action"`
	Summary string `json:"summary"`
	// Status is "pending", "succeeded" or "failed".
	Status    string       `json:"status"`
	Message   string       `json:"message"`
	CreatedAt time.Time    `json:"created_at"`
	Results   []TaskResult `json:"results"`
}

// CreateTask records a change that has to reach the clusters, with a pending
// result for each of them. The next sync of a cluster fills its result in.
// clusterID limits the task to one cluster when it is not empty.
func (s *Store) CreateTask(ctx context.Context, action, summary, clusterID string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO tasks (action, summary) VALUES ($1, $2) RETURNING id`, action, summary).Scan(&id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO task_results (task_id, cluster_id, cluster_name)
			 SELECT $1, id, name FROM clusters WHERE $2 = '' OR id::text = $2`, id, clusterID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET status = 'succeeded', message = 'Saved. There is no cluster to apply it to yet.' WHERE id = $1`, id); err != nil {
				return err
			}
		}
		return trimTasks(ctx, tx)
	})
}

// CreateFinishedTask records something that is already done and involves no
// sync, such as a usage reset.
func (s *Store) CreateFinishedTask(ctx context.Context, action, summary string, ok bool, message string) error {
	status := "succeeded"
	if !ok {
		status = "failed"
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tasks (action, summary, status, message) VALUES ($1, $2, $3, $4)`, action, summary, status, message); err != nil {
			return err
		}
		return trimTasks(ctx, tx)
	})
}

func trimTasks(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `DELETE FROM tasks WHERE id <= (SELECT max(id) FROM tasks) - $1`, keepTasks)
	return err
}

// OpenTasks returns the tasks that still wait for this cluster. A sync reads
// them before it reads the desired state, so that state holds their changes.
func (s *Store) OpenTasks(ctx context.Context, clusterID string) ([]int64, error) {
	rows, err := s.db.Query(ctx, `SELECT task_id FROM task_results WHERE cluster_id = $1 AND status <> 'succeeded'`, clusterID)
	if err != nil {
		return nil, mapErr(err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// FinishTasks stores the outcome of one sync on the tasks it covered.
func (s *Store) FinishTasks(ctx context.Context, clusterID string, taskIDs []int64, ok bool, message string, changes, rejected any) error {
	if len(taskIDs) == 0 {
		return nil
	}
	status := "succeeded"
	if !ok {
		status = "failed"
	}
	c, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	r, err := json.Marshal(rejected)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx,
		`UPDATE task_results SET status = $3, message = $4, changes = COALESCE(NULLIF($5::jsonb, 'null'), '[]'),
		        rejected = COALESCE(NULLIF($6::jsonb, 'null'), '[]'), finished_at = now()
		 WHERE cluster_id = $1 AND task_id = ANY($2)`,
		clusterID, taskIDs, status, message, string(c), string(r))
	return err
}

// ListTasks returns the newest tasks first, each with its per-cluster results.
func (s *Store) ListTasks(ctx context.Context, limit int) ([]Task, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, action, summary, COALESCE(status, ''), message, created_at FROM tasks ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	tasks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Task, error) {
		t := Task{Results: []TaskResult{}}
		err := r.Scan(&t.ID, &t.Action, &t.Summary, &t.Status, &t.Message, &t.CreatedAt)
		return t, err
	})
	if err != nil || len(tasks) == 0 {
		return []Task{}, err
	}
	byID := make(map[int64]*Task, len(tasks))
	ids := make([]int64, 0, len(tasks))
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
		ids = append(ids, tasks[i].ID)
	}
	rows, err = s.db.Query(ctx,
		`SELECT task_id, cluster_name, status, message, changes, rejected, finished_at
		 FROM task_results WHERE task_id = ANY($1) ORDER BY cluster_name`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r TaskResult
		if err := rows.Scan(&id, &r.ClusterName, &r.Status, &r.Message, &r.Changes, &r.Rejected, &r.FinishedAt); err != nil {
			return nil, err
		}
		byID[id].Results = append(byID[id].Results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A task without its own status takes the worst of its clusters.
	for i := range tasks {
		t := &tasks[i]
		if t.Status != "" {
			continue
		}
		t.Status = "succeeded"
		for _, r := range t.Results {
			if r.Status == "failed" {
				t.Status = "failed"
				break
			}
			if r.Status == "pending" {
				t.Status = "pending"
			}
		}
	}
	return tasks, nil
}
