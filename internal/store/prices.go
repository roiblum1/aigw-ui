package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/render"
)

// What a model's quotas and usage are counted in.
const (
	UnitTokens  = "tokens"
	UnitCredits = "credits"
)

// Amount writes a limit or a usage in a unit the way a person reads it:
// "$12.50" or "40000 tokens".
func Amount(n int64, unit string) string {
	if unit == UnitCredits {
		return render.Dollars(n)
	}
	return fmt.Sprintf("%d tokens", n)
}

// Price is one set of prices of a model, in credits per million tokens.
type Price struct {
	Version int   `json:"version"`
	Input   int64 `json:"price_input"`
	Cached  int64 `json:"price_cached"`
	Output  int64 `json:"price_output"`
	// EffectiveAt is the start of the day the prices count from. AppliedAt
	// is nil until the server has started rendering them.
	EffectiveAt time.Time  `json:"effective_at"`
	AppliedAt   *time.Time `json:"applied_at"`
	Note        string     `json:"note"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Render returns the prices as the renderer takes them.
func (p Price) Render() render.Prices {
	return render.Prices{Input: p.Input, Cached: p.Cached, Output: p.Output}
}

// AppliedPrice is a set of prices the server has just started to use.
type AppliedPrice struct {
	ModelName string
	Price     Price
	// First is set for a model that was counted in tokens until now.
	First bool
}

var (
	// ErrNotPriced is returned when dry-run is changed on a model that is
	// counted in tokens.
	ErrNotPriced = errors.New("the model has no prices")
	// ErrHasQuotas is returned when prices are to start at once on a model
	// whose tenants already have counters in the running windows.
	ErrHasQuotas = errors.New("the model has tenant quotas")
)

const priceColumns = `version, price_input, price_cached, price_output, effective_at, applied_at, note, created_at`

func scanPrice(r scanner) (Price, error) {
	var p Price
	err := r.Scan(&p.Version, &p.Input, &p.Cached, &p.Output, &p.EffectiveAt, &p.AppliedAt, &p.Note, &p.CreatedAt)
	return p, err
}

// ListPrices returns every set of prices of a model, the newest first.
func (s *Store) ListPrices(ctx context.Context, modelID string) ([]Price, error) {
	return list(ctx, s, scanPrice, `SELECT `+priceColumns+` FROM model_prices WHERE model_id = $1 ORDER BY version DESC`, modelID)
}

// SetPendingPrice stores the prices a model gets at p.EffectiveAt, in place
// of the ones that were waiting. It returns the model's name.
//
// now starts them at once. That is refused for a model with tenant quotas:
// their counters of the running windows would hold amounts at two prices.
func (s *Store) SetPendingPrice(ctx context.Context, modelID string, p Price, now bool) (string, error) {
	var name string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var quotas int
		if err := tx.QueryRow(ctx, `SELECT m.name, (SELECT count(*) FROM quotas q WHERE q.model_id = m.id) FROM models m WHERE m.id = $1 FOR UPDATE`, modelID).
			Scan(&name, &quotas); err != nil {
			return err
		}
		if now && quotas > 0 {
			return ErrHasQuotas
		}
		if _, err := tx.Exec(ctx, `DELETE FROM model_prices WHERE model_id = $1 AND applied_at IS NULL`, modelID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO model_prices (model_id, version, price_input, price_cached, price_output, effective_at, note)
			 VALUES ($1, (SELECT COALESCE(max(version), 0) + 1 FROM model_prices WHERE model_id = $1), $2, $3, $4,
			         CASE WHEN $7 THEN now() ELSE $5 END, $6)`,
			modelID, p.Input, p.Cached, p.Output, p.EffectiveAt, p.Note, now)
		return err
	})
	return name, mapErr(err)
}

// DeletePendingPrice drops the prices that wait for their day and returns
// the model's name.
func (s *Store) DeletePendingPrice(ctx context.Context, modelID string) (string, error) {
	var name string
	err := s.db.QueryRow(ctx,
		`WITH gone AS (DELETE FROM model_prices WHERE model_id = $1 AND applied_at IS NULL RETURNING model_id)
		 SELECT m.name FROM models m JOIN gone g ON g.model_id = m.id`, modelID).Scan(&name)
	return name, mapErr(err)
}

// ApplyDuePrices starts using every set of prices whose day has come and
// returns them.
//
// A model that was counted in tokens is switched to credits here: its pool
// and its tenants' limits are converted once, at the input price, and it is
// put in dry-run so that nobody is refused by a converted figure before
// somebody has looked at it.
func (s *Store) ApplyDuePrices(ctx context.Context) ([]AppliedPrice, error) {
	var out []AppliedPrice
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		out = nil
		rows, err := tx.Query(ctx,
			`SELECT p.model_id, m.name,
			        NOT EXISTS (SELECT 1 FROM model_prices a WHERE a.model_id = p.model_id AND a.applied_at IS NOT NULL),
			        p.version, p.price_input, p.price_cached, p.price_output, p.effective_at, p.applied_at, p.note, p.created_at
			 FROM model_prices p JOIN models m ON m.id = p.model_id
			 WHERE p.applied_at IS NULL AND p.effective_at <= now() ORDER BY m.name FOR UPDATE OF p, m`)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			var a AppliedPrice
			p := &a.Price
			if err := rows.Scan(&id, &a.ModelName, &a.First, &p.Version, &p.Input, &p.Cached, &p.Output, &p.EffectiveAt, &p.AppliedAt, &p.Note, &p.CreatedAt); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
			out = append(out, a)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i, id := range ids {
			if out[i].First {
				if err := toCredits(ctx, tx, id, out[i].Price.Input); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE model_prices SET applied_at = now() WHERE model_id = $1 AND version = $2`, id, out[i].Price.Version); err != nil {
				return err
			}
		}
		return nil
	})
	return out, mapErr(err)
}

// toCredits converts a model's limits from tokens to credits, valuing every
// token as an input token, and puts the model in dry-run.
func toCredits(ctx context.Context, tx pgx.Tx, modelID string, priceInput int64) error {
	const converted = `LEAST($3::numeric, GREATEST(1, floor(%s::numeric * $2 / 1000000)))::bigint`
	for _, q := range []string{
		`UPDATE quotas SET token_limit = ` + fmt.Sprintf(converted, "token_limit") + ` WHERE model_id = $1`,
		`UPDATE models SET price_dry_run = true, default_limit = ` + fmt.Sprintf(converted, "default_limit") +
			`, best_effort_limit = ` + fmt.Sprintf(converted, "best_effort_limit") + ` WHERE id = $1`,
		// Nobody is moved to best-effort during dry-run, and a period that
		// began against a limit in tokens says nothing about one in credits.
		`UPDATE overage SET until = now() WHERE model_id = $1 AND until > now() AND $2::bigint > 0 AND $3::bigint > 0`,
	} {
		if _, err := tx.Exec(ctx, q, modelID, priceInput, render.MaxLimit); err != nil {
			return err
		}
	}
	return nil
}

// SetPriceDryRun turns dry-run of a priced model on or off and returns the
// model's name.
func (s *Store) SetPriceDryRun(ctx context.Context, modelID string, on bool) (string, error) {
	var name string
	var priced bool
	err := s.db.QueryRow(ctx,
		`WITH priced AS (SELECT EXISTS (SELECT 1 FROM model_prices WHERE model_id = $1 AND applied_at IS NOT NULL) AS yes),
		      changed AS (UPDATE models SET price_dry_run = $2 WHERE id = $1 AND (SELECT yes FROM priced) RETURNING 1)
		 SELECT m.name, (SELECT yes FROM priced) FROM models m LEFT JOIN changed ON true WHERE m.id = $1`, modelID, on).Scan(&name, &priced)
	if err == nil && !priced {
		err = ErrNotPriced
	}
	return name, mapErr(err)
}

// modelPrices are the prices in use and the ones waiting, per model.
type modelPrices struct{ current, pending map[string]Price }

func (s *Store) modelPrices(ctx context.Context) (modelPrices, error) {
	out := modelPrices{current: map[string]Price{}, pending: map[string]Price{}}
	rows, err := s.db.Query(ctx,
		`SELECT DISTINCT ON (model_id, applied_at IS NULL) model_id, `+priceColumns+`
		 FROM model_prices ORDER BY model_id, applied_at IS NULL, version DESC`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var p Price
		if err := rows.Scan(&id, &p.Version, &p.Input, &p.Cached, &p.Output, &p.EffectiveAt, &p.AppliedAt, &p.Note, &p.CreatedAt); err != nil {
			return out, err
		}
		if p.AppliedAt == nil {
			out.pending[id] = p
		} else {
			out.current[id] = p
		}
	}
	return out, rows.Err()
}

// priced fills in a model's prices, its unit and the cost expression that
// is rendered for it.
func (mp modelPrices) priced(m *Model) {
	m.Unit = UnitTokens
	if p, ok := mp.current[m.ID]; ok {
		m.Prices, m.Unit = &p, UnitCredits
	}
	if p, ok := mp.pending[m.ID]; ok {
		m.PendingPrices = &p
	}
}
