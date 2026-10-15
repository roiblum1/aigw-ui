package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const tenantSelect = `SELECT t.id, t.slug, t.display_name, t.enabled, t.created_at,
	(SELECT count(*) FROM api_keys k WHERE k.tenant_id = t.id AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > now())),
	(SELECT count(*) FROM api_keys k WHERE k.tenant_id = t.id AND k.revoked_at IS NULL AND k.kind = 'key'),
	(SELECT count(*) FROM quotas q WHERE q.tenant_id = t.id),
	t.budget_credits, t.budget_window, t.restricted,
	(SELECT coalesce(array_agg(tm.model_id::text ORDER BY tm.model_id), '{}') FROM tenant_models tm WHERE tm.tenant_id = t.id)
	FROM tenants t`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	var credits *int64
	var window *string
	var restricted bool
	var models []string
	err := row.Scan(&t.ID, &t.Slug, &t.DisplayName, &t.Enabled, &t.CreatedAt, &t.KeyCount, &t.APIKeyCount, &t.QuotaCount,
		&credits, &window, &restricted, &models)
	if credits != nil && window != nil {
		t.Budget = &TenantBudget{Credits: *credits, Window: *window}
	}
	if restricted {
		t.Models = models
	}
	return t, mapErr(err)
}

// SetTenantBudget sets the tenant's budget across models, nil to remove it,
// and returns the tenant.
func (s *Store) SetTenantBudget(ctx context.Context, id string, b *TenantBudget) (Tenant, error) {
	var credits *int64
	var window *string
	if b != nil {
		credits, window = &b.Credits, &b.Window
	}
	if err := affected(s.db.Exec(ctx, `UPDATE tenants SET budget_credits = $2, budget_window = $3 WHERE id = $1`, id, credits, window)); err != nil {
		return Tenant{}, err
	}
	return s.GetTenant(ctx, id)
}

// SetTenantModels sets the models the tenant may call: nil for every model,
// else the given ones and no other. It returns the tenant.
func (s *Store) SetTenantModels(ctx context.Context, id string, models []string) (Tenant, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Tenant{}, err
	}
	defer tx.Rollback(ctx)
	if err := affected(tx.Exec(ctx, `UPDATE tenants SET restricted = $2 WHERE id = $1`, id, models != nil)); err != nil {
		return Tenant{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tenant_models WHERE tenant_id = $1`, id); err != nil {
		return Tenant{}, mapErr(err)
	}
	for _, m := range models {
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_models (tenant_id, model_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, m); err != nil {
			return Tenant{}, mapErr(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Tenant{}, mapErr(err)
	}
	return s.GetTenant(ctx, id)
}

// AnyBudget reports whether any enabled tenant has a budget across models
// or may call only some models: both need every tenant on tokens.
func (s *Store) AnyBudget(ctx context.Context) (bool, error) {
	var yes bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE enabled AND (budget_credits IS NOT NULL OR restricted))`).Scan(&yes)
	return yes, mapErr(err)
}

func (s *Store) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.Query(ctx, tenantSelect+` ORDER BY t.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Tenant{}
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetTenant(ctx context.Context, id string) (Tenant, error) {
	return scanTenant(s.db.QueryRow(ctx, tenantSelect+` WHERE t.id = $1`, id))
}

func (s *Store) CreateTenant(ctx context.Context, slug, displayName string) (Tenant, error) {
	var id string
	err := s.db.QueryRow(ctx, `INSERT INTO tenants (slug, display_name) VALUES ($1, $2) RETURNING id`, slug, displayName).Scan(&id)
	if err != nil {
		return Tenant{}, mapErr(err)
	}
	return s.GetTenant(ctx, id)
}

// UpdateTenant does not change the slug: it is part of every key's client ID.
func (s *Store) UpdateTenant(ctx context.Context, id, displayName string, enabled bool) (Tenant, error) {
	if err := affected(s.db.Exec(ctx, `UPDATE tenants SET display_name = $2, enabled = $3 WHERE id = $1`, id, displayName, enabled)); err != nil {
		return Tenant{}, err
	}
	return s.GetTenant(ctx, id)
}

func (s *Store) DeleteTenant(ctx context.Context, id string) error {
	return affected(s.db.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id))
}

// DeleteTenantsWithPrefix deletes every tenant whose slug starts with prefix,
// with its keys and quotas, and returns how many there were.
func (s *Store) DeleteTenantsWithPrefix(ctx context.Context, prefix string) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM tenants WHERE starts_with(slug, $1)`, prefix)
	return tag.RowsAffected(), mapErr(err)
}

// ---- API keys ----

func (s *Store) ListKeys(ctx context.Context, tenantID string) ([]APIKey, error) {
	return list(ctx, s, scanKey, `SELECT `+keyColumns+` FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
}

// keyPrefixLen is how much of a key is stored in the clear, to show which
// key a row is.
const keyPrefixLen = 9

// keyColumns is what scanKey reads. The key itself is never among them.
const keyColumns = `id, tenant_id, name, client_id, key_prefix, revoked_at, created_at, kind, expires_at`

func scanKey(r scanner) (APIKey, error) {
	var k APIKey
	err := r.Scan(&k.ID, &k.TenantID, &k.Name, &k.ClientID, &k.KeyPrefix, &k.RevokedAt, &k.CreatedAt, &k.Kind, &k.ExpiresAt)
	return k, err
}

// CreateKey returns the stored record and the plaintext key. The plaintext is
// kept encrypted because every sync has to write it to the clusters.
func (s *Store) CreateKey(ctx context.Context, tenantID, name string) (APIKey, string, error) {
	t, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return APIKey{}, "", err
	}
	raw := make([]byte, 28)
	if _, err := rand.Read(raw); err != nil {
		return APIKey{}, "", err
	}
	plain := "sk-" + hex.EncodeToString(raw[4:])
	clientID := t.Slug + "." + hex.EncodeToString(raw[:4])
	enc, err := s.box.Seal([]byte(plain))
	if err != nil {
		return APIKey{}, "", err
	}
	k, err := scanKey(s.db.QueryRow(ctx,
		`INSERT INTO api_keys (tenant_id, name, client_id, key_prefix, key_enc) VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+keyColumns, tenantID, name, clientID, plain[:keyPrefixLen], enc))
	return k, plain, mapErr(err)
}

// RevokeKey returns the key it revoked. A token is also denied by the
// gateways until DenialMargin after it expires (token_denials), whatever
// becomes of its tenant.
func (s *Store) RevokeKey(ctx context.Context, id string) (APIKey, error) {
	k, err := scanKey(s.db.QueryRow(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL RETURNING `+keyColumns, id))
	return k, mapErr(err)
}

// ---- quotas ----

func (s *Store) ListQuotas(ctx context.Context, tenantID string) ([]Quota, error) {
	return list(ctx, s, func(r scanner) (Quota, error) {
		var q Quota
		err := r.Scan(&q.ID, &q.TenantID, &q.ModelID, &q.ModelName, &q.TokenLimit, &q.Window, &q.Shadow, &q.Unit)
		return q, err
	}, `SELECT q.id, q.tenant_id, q.model_id, m.name, q.token_limit, q.window_size, q.shadow,
	           CASE WHEN EXISTS (SELECT 1 FROM model_prices p WHERE p.model_id = m.id AND p.applied_at IS NOT NULL)
	                THEN '`+UnitCredits+`' ELSE '`+UnitTokens+`' END
	    FROM quotas q JOIN models m ON m.id = q.model_id WHERE q.tenant_id = $1 ORDER BY m.name`, tenantID)
}

// UpsertQuota keeps the stored shadow setting when shadow is nil. A new quota
// is enforced unless shadow says otherwise.
//
// A new quota takes the lowest position on the model that no other quota
// holds, and keeps it for as long as it exists. See render.TenantQuota.Slot.
func (s *Store) UpsertQuota(ctx context.Context, tenantID, modelID string, limit int64, window string, shadow *bool) error {
	var err error
	// Two new quotas on one model can pick the same position at the same
	// time. The unique constraint refuses the second, which then picks again.
	for range 5 {
		_, err = s.db.Exec(ctx,
			`WITH before AS (
			     SELECT token_limit, window_size, shadow FROM quotas WHERE tenant_id = $1 AND model_id = $2
			 ), saved AS (
			     INSERT INTO quotas (tenant_id, model_id, token_limit, window_size, shadow, slot)
			     VALUES ($1, $2, $3, $4, COALESCE($5::boolean, false), (
			         SELECT min(free) FROM generate_series(0, (SELECT count(*) FROM quotas WHERE model_id = $2)::int) free
			         WHERE NOT EXISTS (SELECT 1 FROM quotas q WHERE q.model_id = $2 AND q.slot = free)))
			     ON CONFLICT (tenant_id, model_id) DO UPDATE SET token_limit = EXCLUDED.token_limit, window_size = EXCLUDED.window_size,
			         shadow = COALESCE($5::boolean, quotas.shadow)
			     RETURNING token_limit, window_size, shadow
			 )
			 -- A quota that changed is a new budget: the tenant is judged
			 -- against it from the start. Saving it unchanged moves nobody.
			 UPDATE overage SET until = now()
			 WHERE tenant_id = $1 AND model_id = $2 AND until > now()
			   AND EXISTS (SELECT 1 FROM saved s, before b
			               WHERE (s.token_limit, s.window_size, s.shadow) IS DISTINCT FROM (b.token_limit, b.window_size, b.shadow))`,
			tenantID, modelID, limit, window, shadow)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.ConstraintName != "quotas_model_slot" {
			break
		}
	}
	return mapErr(err)
}

// DeleteQuota returns "<tenant> on <model>" for the quota it removed.
func (s *Store) DeleteQuota(ctx context.Context, id string) (string, error) {
	var label string
	err := s.db.QueryRow(ctx,
		`WITH gone AS (
		     DELETE FROM quotas q USING tenants t, models m
		     WHERE q.id = $1 AND t.id = q.tenant_id AND m.id = q.model_id
		     RETURNING q.tenant_id, q.model_id, t.slug || ' on ' || m.name AS label
		 ), ended AS (
		     UPDATE overage o SET until = now() FROM gone
		     WHERE o.tenant_id = gone.tenant_id AND o.model_id = gone.model_id AND o.until > now()
		 )
		 SELECT label FROM gone`, id).Scan(&label)
	return label, mapErr(err)
}
