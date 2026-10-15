package store

import (
	"context"
	"time"
)

// Spend is what was spent in credits on the priced models since a point in
// time, best-effort included: the planner's history.
type Spend struct {
	// BusiestHour is, per model, the most spent on it in one hour.
	BusiestHour map[string]int64
	// Days is how many days have spend recorded, BusiestDay the most spent
	// in one of them over every model.
	Days       int
	BusiestDay int64
}

// SpendSince reads the planner's history from the usage kept per hour.
func (s *Store) SpendSince(ctx context.Context, from time.Time) (Spend, error) {
	out := Spend{BusiestHour: map[string]int64{}}
	rows, err := s.db.Query(ctx,
		`SELECT model_id, max(spent)::bigint FROM (
		     SELECT model_id, hour, sum(used + best_effort) AS spent FROM usage_hours
		     WHERE unit = 'credits' AND hour >= $1 GROUP BY model_id, hour
		 ) h GROUP BY model_id`, from)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return out, err
		}
		out.BusiestHour[id] = n
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	err = s.db.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(spent), 0)::bigint FROM (
		     SELECT date_trunc('day', hour, 'UTC') AS day, sum(used + best_effort) AS spent FROM usage_hours
		     WHERE unit = 'credits' AND hour >= $1 GROUP BY day
		 ) d`, from).Scan(&out.Days, &out.BusiestDay)
	return out, err
}

// SpentBetween is what was spent in credits on the priced models between two
// points in time, best-effort included.
func (s *Store) SpentBetween(ctx context.Context, from, to time.Time) (int64, error) {
	var n int64
	err := s.db.QueryRow(ctx,
		`SELECT COALESCE(sum(used + best_effort), 0)::bigint FROM usage_hours WHERE unit = 'credits' AND hour >= $1 AND hour < $2`, from, to).Scan(&n)
	return n, err
}
