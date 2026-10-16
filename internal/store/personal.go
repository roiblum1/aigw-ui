package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/token"
)

// A personal key is what a user's helper (aigw-token, Claude Code's
// apiKeyHelper) holds: it trades it at the hub for a token that lives
// TokenSettings.RenewedLifetime, signed with the tenant's budget and models
// as they are at that moment, and gets a new one before it runs out. The
// user never handles a token, and a change of budget reaches the gateway
// with the next one. The hub keeps only the key's hash; the gateways never
// see it, only the tokens, which are not stored.

// PersonalKeyPrefix starts every personal key, so that it is told apart
// from an API key ("sk-") and a token ("eyJ").
const PersonalKeyPrefix = "aigw-pk-"

func personalKeyHash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// CreatePersonalKey makes a personal key for a tenant. It returns the
// record and the key, which is shown once.
func (s *Store) CreatePersonalKey(ctx context.Context, tenantID, name string) (APIKey, string, error) {
	t, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return APIKey{}, "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return APIKey{}, "", err
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return APIKey{}, "", err
	}
	secret := PersonalKeyPrefix + base64.RawURLEncoding.EncodeToString(raw)
	jti := hex.EncodeToString(id)
	k, err := scanKey(s.db.QueryRow(ctx,
		`INSERT INTO api_keys (tenant_id, name, client_id, key_prefix, key_hash, kind, jti)
		 VALUES ($1, $2, $3, $4, $5, 'personal', $6) RETURNING `+keyColumns,
		tenantID, name, t.Slug+"."+jti, secret[len(secret)-keyPrefixLen:], personalKeyHash(secret), jti))
	return k, secret, mapErr(err)
}

// personalKey finds the live personal key of an enabled tenant.
func personalKey(ctx context.Context, q pgx.Tx, secret string) (keyID, clientID string, t Tenant, err error) {
	if !strings.HasPrefix(secret, PersonalKeyPrefix) {
		return "", "", Tenant{}, ErrNotFound
	}
	var tenantID string
	err = q.QueryRow(ctx,
		`SELECT k.id, k.client_id, k.tenant_id FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.key_hash = $1 AND k.kind = 'personal' AND k.revoked_at IS NULL AND t.enabled FOR UPDATE OF k`,
		personalKeyHash(secret)).Scan(&keyID, &clientID, &tenantID)
	if err != nil {
		return "", "", Tenant{}, mapErr(err)
	}
	t, err = scanTenant(q.QueryRow(ctx, tenantSelect+` WHERE t.id = $1`, tenantID))
	return keyID, clientID, t, err
}

// TenantOfPersonalKey returns the enabled tenant a live personal key
// belongs to: the tenant's own page takes one to sign in.
func (s *Store) TenantOfPersonalKey(ctx context.Context, secret string) (Tenant, error) {
	var t Tenant
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		_, _, t, err = personalKey(ctx, tx, secret)
		return err
	})
	return t, mapErr(err)
}

// RenewToken trades a live personal key of an enabled tenant for a new
// token, signed with the tenant's budget and models as they are now. The
// token is not stored: the key remembers when the last one it got expires,
// which is how long a revocation must deny it, and the signing key how long
// it must stay in the gateways' key set. ErrNotFound for any other key.
func (s *Store) RenewToken(ctx context.Context, secret string, set TokenSettings, now time.Time) (string, time.Time, Tenant, error) {
	var plain string
	var t Tenant
	expires := now.Add(set.RenewedLifetime).Truncate(time.Second)
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		keyID, clientID, tenant, err := personalKey(ctx, tx, secret)
		if err != nil {
			return err
		}
		t = tenant
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('aigw-ui signing key'))`); err != nil {
			return err
		}
		key, err := s.signer(ctx, tx)
		if err != nil {
			return err
		}
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		claims := token.Claims{Issuer: set.Issuer, Audience: set.Audience, Subject: t.Slug, ID: hex.EncodeToString(raw),
			ClientID: clientID, IssuedAt: now.Unix(), ExpiresAt: expires.Unix()}
		if err := tokenLimits(ctx, tx, t, &claims); err != nil {
			return err
		}
		if plain, err = token.Sign(key, claims); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET renewed_at = $2, expires_at = greatest(expires_at, $3) WHERE id = $1`,
			keyID, now, expires); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE signing_keys SET renewed_until = greatest(renewed_until, $2) WHERE id = $1`, key.ID, expires)
		return err
	})
	if err != nil {
		return "", time.Time{}, Tenant{}, mapErr(err)
	}
	return plain, expires, t, nil
}

// tenantOfRenewed returns the enabled tenant of a token a personal key got,
// once its claims were checked: the key must be live.
func (s *Store) tenantOfRenewed(ctx context.Context, c token.Claims) (Tenant, error) {
	var tenantID string
	err := s.db.QueryRow(ctx,
		`SELECT k.tenant_id FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.client_id = $1 AND k.kind = 'personal' AND k.revoked_at IS NULL AND t.slug = $2 AND t.enabled`,
		c.ClientID, c.Subject).Scan(&tenantID)
	if err != nil {
		return Tenant{}, mapErr(err)
	}
	return s.GetTenant(ctx, tenantID)
}

// DeniedClients returns the client ID of every personal key the gateways
// must refuse the tokens of: revoked, or of a deleted tenant, until
// DenialMargin after the last token it got expires.
func (s *Store) DeniedClients(ctx context.Context) ([]string, error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM client_denials WHERE expires_at <= $1`, time.Now().Add(-DenialMargin)); err != nil {
		return nil, err
	}
	return list(ctx, s, func(r scanner) (string, error) {
		var id string
		return id, r.Scan(&id)
	}, `SELECT client_id FROM client_denials ORDER BY client_id`)
}
