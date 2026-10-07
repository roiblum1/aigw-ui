package store

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"aigw-ui/internal/render"
	"aigw-ui/internal/secretbox"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

type Store struct {
	db  *pgxpool.Pool
	box *secretbox.Box
}

func Open(ctx context.Context, url string, box *secretbox.Box) (*Store, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	// On a fresh install the database is often still starting. Wait for it
	// rather than exit and be restarted by the platform.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		err = db.Ping(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			db.Close()
			return nil, fmt.Errorf("connect to postgres: %w", err)
		}
		slog.Info("waiting for postgres", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	s := &Store{db: db, box: box}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() { s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// mapErr translates driver errors into the package's sentinel errors.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return ErrConflict
		case "23503", "22P02": // missing reference, malformed uuid
			return ErrNotFound
		}
	}
	return err
}

func affected(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- clusters ----

const clusterCols = `id, name, site, namespace, gateway_name, auth_enabled, sync_status, sync_message, synced_at, created_at,
	discovery_message, discovered_at, gateway_url, discovery_token_enc IS NOT NULL`

func scanCluster(row pgx.Row) (Cluster, error) {
	var c Cluster
	err := row.Scan(&c.ID, &c.Name, &c.Site, &c.Namespace, &c.GatewayName, &c.AuthEnabled, &c.SyncStatus, &c.SyncMessage, &c.SyncedAt, &c.CreatedAt, &c.DiscoveryMessage, &c.DiscoveredAt, &c.GatewayURL, &c.HasDiscoveryToken)
	return c, mapErr(err)
}

func (s *Store) ListClusters(ctx context.Context) ([]Cluster, error) {
	rows, err := s.db.Query(ctx, `SELECT `+clusterCols+` FROM clusters ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cluster{}
	for rows.Next() {
		c, err := scanCluster(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetCluster(ctx context.Context, id string) (Cluster, error) {
	return scanCluster(s.db.QueryRow(ctx, `SELECT `+clusterCols+` FROM clusters WHERE id = $1`, id))
}

// sealOptional encrypts a value, or returns nil for an empty one.
func (s *Store) sealOptional(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, nil
	}
	return s.box.Seal(plain)
}

func (s *Store) CreateCluster(ctx context.Context, c Cluster, kubeconfig []byte, discoveryToken string) (Cluster, error) {
	enc, err := s.box.Seal(kubeconfig)
	if err != nil {
		return Cluster{}, err
	}
	token, err := s.sealOptional([]byte(discoveryToken))
	if err != nil {
		return Cluster{}, err
	}
	return scanCluster(s.db.QueryRow(ctx,
		`INSERT INTO clusters (name, site, namespace, gateway_name, auth_enabled, kubeconfig_enc, gateway_url, discovery_token_enc)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+clusterCols,
		c.Name, c.Site, c.Namespace, c.GatewayName, c.AuthEnabled, enc, c.GatewayURL, token))
}

// UpdateCluster keeps the stored kubeconfig and discovery token when the new
// value is empty. The token is dropped when the gateway URL is removed.
func (s *Store) UpdateCluster(ctx context.Context, c Cluster, kubeconfig []byte, discoveryToken string) (Cluster, error) {
	enc, err := s.sealOptional(kubeconfig)
	if err != nil {
		return Cluster{}, err
	}
	token, err := s.sealOptional([]byte(discoveryToken))
	if err != nil {
		return Cluster{}, err
	}
	return scanCluster(s.db.QueryRow(ctx,
		`UPDATE clusters SET name = $2, site = $3, namespace = $4, gateway_name = $5, auth_enabled = $6,
		        kubeconfig_enc = COALESCE($7, kubeconfig_enc), sync_status = 'pending', gateway_url = $8,
		        discovery_token_enc = CASE WHEN $8 = '' THEN NULL ELSE COALESCE($9, discovery_token_enc) END
		 WHERE id = $1 RETURNING `+clusterCols,
		c.ID, c.Name, c.Site, c.Namespace, c.GatewayName, c.AuthEnabled, enc, c.GatewayURL, token))
}

// DiscoveryToken returns the cluster's token for /v1/models, or "" if none is set.
func (s *Store) DiscoveryToken(ctx context.Context, id string) (string, error) {
	var enc []byte
	if err := s.db.QueryRow(ctx, `SELECT discovery_token_enc FROM clusters WHERE id = $1`, id).Scan(&enc); err != nil {
		return "", mapErr(err)
	}
	if enc == nil {
		return "", nil
	}
	plain, err := s.box.Open(enc)
	return string(plain), err
}

func (s *Store) DeleteCluster(ctx context.Context, id string) error {
	return affected(s.db.Exec(ctx, `DELETE FROM clusters WHERE id = $1`, id))
}

func (s *Store) Kubeconfig(ctx context.Context, id string) ([]byte, error) {
	var enc []byte
	if err := s.db.QueryRow(ctx, `SELECT kubeconfig_enc FROM clusters WHERE id = $1`, id).Scan(&enc); err != nil {
		return nil, mapErr(err)
	}
	return s.box.Open(enc)
}

func (s *Store) SetSyncResult(ctx context.Context, id, status, message string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE clusters SET sync_status = $2, sync_message = $3, synced_at = now() WHERE id = $1`, id, status, message)
	return err
}

// MarkPending flags every cluster as out of date after a change to desired state.
func (s *Store) MarkPending(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `UPDATE clusters SET sync_status = 'pending' WHERE sync_status <> 'pending'`)
	return err
}

// ---- models ----

type ModelInput struct {
	Name           string
	DefaultLimit   int64
	DefaultWindow  string
	CostExpression string
	Endpoints      []Endpoint
	// KeepEndpoints leaves the manual endpoints as they are on an update.
	KeepEndpoints bool
}

func (s *Store) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, slug, default_limit, default_window, cost_expression, created_at FROM models ORDER BY name`)
	if err != nil {
		return nil, err
	}
	models, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Model, error) {
		m := Model{Endpoints: []Endpoint{}}
		err := r.Scan(&m.ID, &m.Name, &m.Slug, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &m.CreatedAt)
		return m, err
	})
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*Model, len(models))
	for i := range models {
		byID[models[i].ID] = &models[i]
	}
	rows, err = s.db.Query(ctx,
		`SELECT e.model_id, e.cluster_id, c.name, e.host, e.port, e.upstream_model, e.source, e.backends
		 FROM model_endpoints e JOIN clusters c ON c.id = e.cluster_id ORDER BY c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var modelID string
		var e Endpoint
		if err := rows.Scan(&modelID, &e.ClusterID, &e.ClusterName, &e.Host, &e.Port, &e.UpstreamModel, &e.Source, &e.Backends); err != nil {
			return nil, err
		}
		if m := byID[modelID]; m != nil {
			m.Endpoints = append(m.Endpoints, e)
			m.QuotaCapable = m.QuotaCapable || e.Source == SourceManual || len(e.Backends) > 0
		}
	}
	return models, rows.Err()
}

func (s *Store) CreateModel(ctx context.Context, in ModelInput) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO models (name, slug, default_limit, default_window, cost_expression) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			in.Name, render.Slug(in.Name), in.DefaultLimit, in.DefaultWindow, in.CostExpression).Scan(&id)
		if err != nil {
			return err
		}
		return replaceEndpoints(ctx, tx, id, in)
	})
	return id, mapErr(err)
}

// UpdateModel does not change the slug: it names the objects on the clusters
// and is part of the rate limit counter key.
func (s *Store) UpdateModel(ctx context.Context, id string, in ModelInput) error {
	return mapErr(pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE models SET default_limit = $2, default_window = $3, cost_expression = $4 WHERE id = $1`,
			id, in.DefaultLimit, in.DefaultWindow, in.CostExpression)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if in.KeepEndpoints {
			return nil
		}
		return replaceEndpoints(ctx, tx, id, in)
	}))
}

// replaceEndpoints replaces the manual endpoints of a model. Discovered
// endpoints are left alone, except where a manual one now covers the cluster.
func replaceEndpoints(ctx context.Context, tx pgx.Tx, modelID string, in ModelInput) error {
	if _, err := tx.Exec(ctx, `DELETE FROM model_endpoints WHERE model_id = $1 AND source = 'manual'`, modelID); err != nil {
		return err
	}
	for _, e := range in.Endpoints {
		upstream := e.UpstreamModel
		if upstream == "" {
			upstream = in.Name
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO model_endpoints (model_id, cluster_id, host, port, upstream_model) VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (model_id, cluster_id) DO UPDATE SET host = EXCLUDED.host, port = EXCLUDED.port,
			     upstream_model = EXCLUDED.upstream_model, source = 'manual', backends = '[]'`,
			modelID, e.ClusterID, e.Host, e.Port, upstream); err != nil {
			return err
		}
	}
	return nil
}

// GetModel returns a model's own settings, without its endpoints.
func (s *Store) GetModel(ctx context.Context, id string) (Model, error) {
	var m Model
	err := s.db.QueryRow(ctx, `SELECT id, name, slug, default_limit, default_window, cost_expression, created_at FROM models WHERE id = $1`, id).
		Scan(&m.ID, &m.Name, &m.Slug, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &m.CreatedAt)
	return m, mapErr(err)
}

func (s *Store) DeleteModel(ctx context.Context, id string) error {
	return affected(s.db.Exec(ctx, `DELETE FROM models WHERE id = $1`, id))
}

// ---- tenants ----

const tenantSelect = `SELECT t.id, t.slug, t.display_name, t.enabled, t.created_at,
	(SELECT count(*) FROM api_keys k WHERE k.tenant_id = t.id AND k.revoked_at IS NULL),
	(SELECT count(*) FROM quotas q WHERE q.tenant_id = t.id)
	FROM tenants t`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	err := row.Scan(&t.ID, &t.Slug, &t.DisplayName, &t.Enabled, &t.CreatedAt, &t.KeyCount, &t.QuotaCount)
	return t, mapErr(err)
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

// ---- API keys ----

func (s *Store) ListKeys(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, tenant_id, name, client_id, key_prefix, revoked_at, created_at
		 FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (APIKey, error) {
		var k APIKey
		err := r.Scan(&k.ID, &k.TenantID, &k.Name, &k.ClientID, &k.KeyPrefix, &k.RevokedAt, &k.CreatedAt)
		return k, err
	})
	if keys == nil {
		keys = []APIKey{}
	}
	return keys, mapErr(err)
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
	var k APIKey
	err = s.db.QueryRow(ctx,
		`INSERT INTO api_keys (tenant_id, name, client_id, key_prefix, key_enc) VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, tenant_id, name, client_id, key_prefix, revoked_at, created_at`,
		tenantID, name, clientID, plain[:9], enc).
		Scan(&k.ID, &k.TenantID, &k.Name, &k.ClientID, &k.KeyPrefix, &k.RevokedAt, &k.CreatedAt)
	return k, plain, mapErr(err)
}

func (s *Store) RevokeKey(ctx context.Context, id string) error {
	return affected(s.db.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id))
}

// ---- quotas ----

func (s *Store) ListQuotas(ctx context.Context, tenantID string) ([]Quota, error) {
	rows, err := s.db.Query(ctx,
		`SELECT q.id, q.tenant_id, q.model_id, m.name, q.token_limit, q.window_size, q.shadow
		 FROM quotas q JOIN models m ON m.id = q.model_id WHERE q.tenant_id = $1 ORDER BY m.name`, tenantID)
	if err != nil {
		return nil, mapErr(err)
	}
	quotas, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Quota, error) {
		var q Quota
		err := r.Scan(&q.ID, &q.TenantID, &q.ModelID, &q.ModelName, &q.TokenLimit, &q.Window, &q.Shadow)
		return q, err
	})
	if quotas == nil {
		quotas = []Quota{}
	}
	return quotas, mapErr(err)
}

// UpsertQuota keeps the stored shadow setting when shadow is nil. A new quota
// is enforced unless shadow says otherwise.
func (s *Store) UpsertQuota(ctx context.Context, tenantID, modelID string, limit int64, window string, shadow *bool) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO quotas (tenant_id, model_id, token_limit, window_size, shadow) VALUES ($1, $2, $3, $4, COALESCE($5::boolean, false))
		 ON CONFLICT (tenant_id, model_id) DO UPDATE SET token_limit = EXCLUDED.token_limit, window_size = EXCLUDED.window_size,
		     shadow = COALESCE($5::boolean, quotas.shadow)`,
		tenantID, modelID, limit, window, shadow)
	return mapErr(err)
}

func (s *Store) DeleteQuota(ctx context.Context, id string) error {
	return affected(s.db.Exec(ctx, `DELETE FROM quotas WHERE id = $1`, id))
}

// ---- derived ----

func (s *Store) Overview(ctx context.Context) (Overview, error) {
	var o Overview
	err := s.db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM clusters),
		(SELECT count(*) FROM clusters WHERE sync_status = 'error'),
		(SELECT count(*) FROM models),
		(SELECT count(*) FROM tenants),
		(SELECT count(*) FROM api_keys WHERE revoked_at IS NULL),
		(SELECT count(*) FROM quotas)`).
		Scan(&o.Clusters, &o.ClustersError, &o.Models, &o.Tenants, &o.ActiveKeys, &o.Quotas)
	return o, err
}

// RenderState collects everything one cluster should be running.
func (s *Store) RenderState(ctx context.Context, clusterID string) (render.State, error) {
	c, err := s.GetCluster(ctx, clusterID)
	if err != nil {
		return render.State{}, err
	}
	st := render.State{Namespace: c.Namespace, GatewayName: c.GatewayName, AuthEnabled: c.AuthEnabled}

	rows, err := s.db.Query(ctx,
		`SELECT m.id, m.name, m.slug, e.host, e.port, e.upstream_model, m.default_limit, m.default_window, m.cost_expression, e.source, e.backends
		 FROM model_endpoints e JOIN models m ON m.id = e.model_id WHERE e.cluster_id = $1`, clusterID)
	if err != nil {
		return st, err
	}
	index := map[string]int{}
	for rows.Next() {
		var id string
		var m render.Model
		var source string
		var backends []BackendRef
		if err := rows.Scan(&id, &m.Name, &m.Slug, &m.Host, &m.Port, &m.UpstreamModel, &m.DefaultLimit, &m.DefaultWindow, &m.CostExpression, &source, &backends); err != nil {
			rows.Close()
			return st, err
		}
		if source == SourceDiscovered {
			m.Existing = make([]render.Target, 0, len(backends))
			for _, b := range backends {
				m.Existing = append(m.Existing, render.Target{Namespace: b.Namespace, Backend: b.Name, Model: b.Model})
			}
		}
		index[id] = len(st.Models)
		st.Models = append(st.Models, m)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	rows, err = s.db.Query(ctx,
		`SELECT q.model_id, t.slug, q.token_limit, q.window_size, q.shadow
		 FROM quotas q JOIN tenants t ON t.id = q.tenant_id WHERE t.enabled`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var modelID string
		var q render.TenantQuota
		if err := rows.Scan(&modelID, &q.TenantSlug, &q.Limit, &q.Window, &q.Shadow); err != nil {
			rows.Close()
			return st, err
		}
		if i, ok := index[modelID]; ok {
			st.Models[i].Quotas = append(st.Models[i].Quotas, q)
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	rows, err = s.db.Query(ctx,
		`SELECT k.client_id, k.key_enc FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.revoked_at IS NULL AND t.enabled ORDER BY k.client_id`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var clientID string
		var enc []byte
		if err := rows.Scan(&clientID, &enc); err != nil {
			return st, err
		}
		plain, err := s.box.Open(enc)
		if err != nil {
			return st, fmt.Errorf("decrypt key %s: %w", clientID, err)
		}
		st.Keys = append(st.Keys, render.Key{ClientID: clientID, Value: string(plain)})
	}
	return st, rows.Err()
}

// ---- discovery ----

// ApplyDiscovery records the models found on one cluster. It creates models
// that are new, updates this cluster's discovered endpoints and drops the ones
// that are gone. It never deletes a model, because that would also delete the
// tenants' quotas on it, and it never touches manual endpoints. A model may
// have no backends: it is then listed, but a quota cannot be attached to it.
// It reports whether anything that affects rendering changed.
func (s *Store) ApplyDiscovery(ctx context.Context, clusterID string, found []DiscoveredModel) (bool, error) {
	changed := false
	var skipped []string
	names := make([]string, 0, len(found))

	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		for _, d := range found {
			slug := render.Slug(d.Name)
			if slug == "" {
				skipped = append(skipped, d.Name)
				continue
			}

			var modelID string
			err := tx.QueryRow(ctx, `SELECT id FROM models WHERE name = $1`, d.Name).Scan(&modelID)
			if errors.Is(err, pgx.ErrNoRows) {
				// ON CONFLICT covers a different model whose name gives the same slug.
				err = tx.QueryRow(ctx,
					`INSERT INTO models (name, slug) VALUES ($1, $2) ON CONFLICT DO NOTHING RETURNING id`,
					d.Name, slug).Scan(&modelID)
				if errors.Is(err, pgx.ErrNoRows) {
					skipped = append(skipped, d.Name)
					continue
				}
				changed = true
			}
			if err != nil {
				return err
			}
			names = append(names, d.Name)

			if d.Backends == nil {
				d.Backends = []BackendRef{}
			}
			backends, err := json.Marshal(d.Backends)
			if err != nil {
				return err
			}
			upstream := d.Name
			if len(d.Backends) > 0 {
				upstream = d.Backends[0].Model
			}
			tag, err := tx.Exec(ctx,
				`INSERT INTO model_endpoints (model_id, cluster_id, upstream_model, source, backends)
				 VALUES ($1, $2, $3, 'discovered', $4::jsonb)
				 ON CONFLICT (model_id, cluster_id) DO UPDATE
				     SET upstream_model = EXCLUDED.upstream_model, backends = EXCLUDED.backends
				     WHERE model_endpoints.source = 'discovered'
				       AND (model_endpoints.backends, model_endpoints.upstream_model) IS DISTINCT FROM (EXCLUDED.backends, EXCLUDED.upstream_model)`,
				modelID, clusterID, upstream, string(backends))
			if err != nil {
				return err
			}
			changed = changed || tag.RowsAffected() > 0
		}

		tag, err := tx.Exec(ctx,
			`DELETE FROM model_endpoints e USING models m
			 WHERE m.id = e.model_id AND e.cluster_id = $1 AND e.source = 'discovered' AND NOT (m.name = ANY($2))`,
			clusterID, names)
		if err != nil {
			return err
		}
		changed = changed || tag.RowsAffected() > 0

		msg := fmt.Sprintf("%d models found", len(names))
		if len(skipped) > 0 {
			msg += fmt.Sprintf("; skipped %q: the name cannot be used or clashes with another model", skipped)
		}
		_, err = tx.Exec(ctx, `UPDATE clusters SET discovery_message = $2, discovered_at = now() WHERE id = $1`, clusterID, msg)
		return err
	})
	return changed, mapErr(err)
}

func (s *Store) SetDiscoveryError(ctx context.Context, clusterID, message string) error {
	_, err := s.db.Exec(ctx, `UPDATE clusters SET discovery_message = $2, discovered_at = now() WHERE id = $1`,
		clusterID, "failed: "+message)
	return err
}

// QuotaCapable reports whether any cluster has something a quota on this model
// can attach to: a manual endpoint, or a discovered AIServiceBackend.
func (s *Store) QuotaCapable(ctx context.Context, modelID string) (bool, error) {
	var exists, capable bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM models WHERE id = $1),
		        EXISTS (SELECT 1 FROM model_endpoints WHERE model_id = $1 AND (source = 'manual' OR jsonb_array_length(backends) > 0))`,
		modelID).Scan(&exists, &capable)
	if err != nil {
		return false, mapErr(err)
	}
	if !exists {
		return false, ErrNotFound
	}
	return capable, nil
}
