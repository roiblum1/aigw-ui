package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/capacity"
)

const profileColumns = `id, name, version, workload, slo, sweep, note, active, created_at`

func scanProfile(r scanner) (capacity.Profile, error) {
	var p capacity.Profile
	err := r.Scan(&p.ID, &p.Name, &p.Version, &p.Workload, &p.SLO, &p.Sweep, &p.Note, &p.Active, &p.CreatedAt)
	return p, err
}

// ActiveProfile returns the benchmark profile measurements use now.
func (s *Store) ActiveProfile(ctx context.Context) (capacity.Profile, error) {
	p, err := scanProfile(s.db.QueryRow(ctx, `SELECT `+profileColumns+` FROM benchmark_profiles WHERE active`))
	return p, mapErr(err)
}

// ListProfiles returns every version of the profile, the newest first.
func (s *Store) ListProfiles(ctx context.Context) ([]capacity.Profile, error) {
	return list(ctx, s, scanProfile, `SELECT `+profileColumns+` FROM benchmark_profiles ORDER BY version DESC`)
}

// AddProfileVersion stores an edited profile as the next version and makes
// it the active one. The earlier versions stay, with the results measured
// with them.
func (s *Store) AddProfileVersion(ctx context.Context, p capacity.Profile) (capacity.Profile, error) {
	var out capacity.Profile
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `LOCK TABLE benchmark_profiles IN EXCLUSIVE MODE`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE benchmark_profiles SET active = false WHERE active`); err != nil {
			return err
		}
		var err error
		out, err = scanProfile(tx.QueryRow(ctx,
			`INSERT INTO benchmark_profiles (name, version, workload, slo, sweep, note, active)
			 VALUES ($1, (SELECT COALESCE(max(version), 0) + 1 FROM benchmark_profiles), $2, $3, $4, $5, true)
			 RETURNING `+profileColumns, p.Name, p.Workload, p.SLO, p.Sweep, p.Note))
		return err
	})
	return out, mapErr(err)
}
