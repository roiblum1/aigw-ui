package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

func standardOf(limit *int64, window *string) *Standard {
	if limit == nil || window == nil {
		return nil
	}
	return &Standard{Limit: *limit, Window: *window}
}

// SetStandard sets the standard budget a model will have from at, nil to
// remove it, in place of any change still waiting, and returns the model's
// name.
func (s *Store) SetStandard(ctx context.Context, modelID string, st *Standard, at time.Time) (string, error) {
	var limit *int64
	var window *string
	if st != nil {
		limit, window = &st.Limit, &st.Window
	}
	var name string
	err := s.db.QueryRow(ctx, `UPDATE models SET standard_next_limit = $2, standard_next_window = $3, standard_next_at = $4 WHERE id = $1 RETURNING name`,
		modelID, limit, window, at).Scan(&name)
	return name, mapErr(err)
}

// CancelStandard drops a model's change of standard that is still waiting,
// and returns the model's name.
func (s *Store) CancelStandard(ctx context.Context, modelID string) (string, error) {
	var name string
	err := s.db.QueryRow(ctx, `UPDATE models SET standard_next_limit = NULL, standard_next_window = NULL, standard_next_at = NULL
	                           WHERE id = $1 AND standard_next_at IS NOT NULL RETURNING name`, modelID).Scan(&name)
	return name, mapErr(err)
}

// AppliedStandard is a standard that has just started, or been removed.
type AppliedStandard struct {
	ModelName string
	Standard  *Standard
}

// ApplyDueStandards starts every change of standard whose day has come.
func (s *Store) ApplyDueStandards(ctx context.Context) ([]AppliedStandard, error) {
	var out []AppliedStandard
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		out = nil
		rows, err := tx.Query(ctx,
			`UPDATE models SET standard_limit = standard_next_limit, standard_window = standard_next_window,
			        standard_next_limit = NULL, standard_next_window = NULL, standard_next_at = NULL
			 WHERE standard_next_at <= now() RETURNING name, standard_limit, standard_window`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a AppliedStandard
			var limit *int64
			var window *string
			if err := rows.Scan(&a.ModelName, &limit, &window); err != nil {
				return err
			}
			a.Standard = standardOf(limit, window)
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, mapErr(err)
}

// AnyStandard reports whether a model has a standard, or one is waiting: the
// gateways must then take tokens only, since the standard's rule reads the
// tenant header only a token sets.
func (s *Store) AnyStandard(ctx context.Context) (bool, error) {
	var yes bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM models WHERE standard_limit IS NOT NULL OR standard_next_limit IS NOT NULL)`).Scan(&yes)
	return yes, mapErr(err)
}
