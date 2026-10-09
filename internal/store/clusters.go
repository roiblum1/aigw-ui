package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const clusterCols = `id, name, site, namespace, gateway_name, auth_enabled, sync_status, sync_message, synced_at, created_at,
	discovery_message, discovered_at, gateway_url, discovery_token_enc IS NOT NULL, fleet_enabled, client_listener`

func scanCluster(row pgx.Row) (Cluster, error) {
	var c Cluster
	err := row.Scan(&c.ID, &c.Name, &c.Site, &c.Namespace, &c.GatewayName, &c.AuthEnabled, &c.SyncStatus, &c.SyncMessage, &c.SyncedAt, &c.CreatedAt, &c.DiscoveryMessage, &c.DiscoveredAt, &c.GatewayURL, &c.HasDiscoveryToken, &c.FleetEnabled, &c.ClientListener)
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
		`INSERT INTO clusters (name, site, namespace, gateway_name, auth_enabled, kubeconfig_enc, gateway_url, discovery_token_enc, fleet_enabled, client_listener)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+clusterCols,
		c.Name, c.Site, c.Namespace, c.GatewayName, c.AuthEnabled, enc, c.GatewayURL, token, c.FleetEnabled, c.ClientListener))
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
		        discovery_token_enc = CASE WHEN $8 = '' THEN NULL ELSE COALESCE($9, discovery_token_enc) END,
		        fleet_enabled = $10, client_listener = $11
		 WHERE id = $1 RETURNING `+clusterCols,
		c.ID, c.Name, c.Site, c.Namespace, c.GatewayName, c.AuthEnabled, enc, c.GatewayURL, token, c.FleetEnabled, c.ClientListener))
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

// DeleteCluster also closes the task results that were still waiting for the
// cluster: nothing will ever apply them now.
func (s *Store) DeleteCluster(ctx context.Context, id string) error {
	return mapErr(pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE task_results SET status = 'failed', message = 'The cluster was removed before this was applied.', finished_at = now()
			 WHERE cluster_id = $1 AND status <> 'succeeded'`, id); err != nil {
			return err
		}
		return affected(tx.Exec(ctx, `DELETE FROM clusters WHERE id = $1`, id))
	}))
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
