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
// see it, only the tokens. Those are not stored, except while the gateways
// take API keys too (listedToken).

// PersonalKeyPrefix starts every personal key, so that it is told apart
// from an API key ("sk-") and a token ("eyJ").
const PersonalKeyPrefix = "aigw-pk-"

func personalKeyHash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// CreatePersonalKey makes a personal key for a tenant. It returns the
// record and the key, which is shown once. While the gateways take API keys
// too, the key gets its first token now, for the sync of this change to
// list, so that the gateways take it by the time its owner signs in.
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
	var k APIKey
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		k, err = scanKey(tx.QueryRow(ctx,
			`INSERT INTO api_keys (tenant_id, name, client_id, key_prefix, key_hash, kind, jti)
			 VALUES ($1, $2, $3, $4, $5, 'personal', $6) RETURNING `+keyColumns,
			tenantID, name, t.Slug+"."+jti, secret[len(secret)-keyPrefixLen:], personalKeyHash(secret), jti))
		if err != nil {
			return err
		}
		listed, err := takesAPIKeys(ctx, tx)
		if err != nil || !listed {
			return err
		}
		_, _, _, err = s.listedToken(ctx, tx, k.ID, k.ClientID, t, s.tokens, time.Now())
		return err
	})
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

// Renewal is what a personal key's helper is given for the key.
type Renewal struct {
	Token   string
	Expires time.Time
	Tenant  Tenant
	// Listed is set when a new token was stored for the gateways' API key
	// check to list (listedToken): a sync has to take it there.
	Listed bool
}

// RenewToken trades a live personal key of an enabled tenant for a token,
// signed with the tenant's budget and models as they are now. The token is
// not stored: the key remembers when the last one it got expires, which is
// how long a revocation must deny it, and the signing key how long it must
// stay in the gateways' key set. While the gateways take API keys too, the
// token is one of the key's stored ones instead (listedToken). ErrNotFound
// for any other key.
func (s *Store) RenewToken(ctx context.Context, secret string, set TokenSettings, now time.Time) (Renewal, error) {
	var out Renewal
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		keyID, clientID, t, err := personalKey(ctx, tx, secret)
		if err != nil {
			return err
		}
		out.Tenant = t
		listed, err := takesAPIKeys(ctx, tx)
		if err != nil {
			return err
		}
		if listed {
			out.Token, out.Expires, out.Listed, err = s.listedToken(ctx, tx, keyID, clientID, t, set, now)
		} else {
			out.Token, _, out.Expires, err = s.signRenewed(ctx, tx, keyID, clientID, t, set, now)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE api_keys SET renewed_at = $2 WHERE id = $1`, keyID, now)
		return err
	})
	if err != nil {
		return Renewal{}, mapErr(err)
	}
	return out, nil
}

// signRenewed signs a token for a personal key, with the tenant's limits as
// they are now, and moves the key's expiry (how long a revocation denies its
// tokens) and the signing key's (how long the gateways keep it) to the
// token's.
func (s *Store) signRenewed(ctx context.Context, tx pgx.Tx, keyID, clientID string, t Tenant, set TokenSettings, now time.Time) (plain, jti string, expires time.Time, err error) {
	expires = now.Add(set.RenewedLifetime).Truncate(time.Second)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('aigw-ui signing key'))`); err != nil {
		return "", "", time.Time{}, err
	}
	key, err := s.signer(ctx, tx)
	if err != nil {
		return "", "", time.Time{}, err
	}
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", "", time.Time{}, err
	}
	jti = hex.EncodeToString(raw)
	claims := token.Claims{Issuer: set.Issuer, Audience: set.Audience, Subject: t.Slug, ID: jti,
		ClientID: clientID, IssuedAt: now.Unix(), ExpiresAt: expires.Unix()}
	if err := tokenLimits(ctx, tx, t, &claims); err != nil {
		return "", "", time.Time{}, err
	}
	if plain, err = token.Sign(key, claims); err != nil {
		return "", "", time.Time{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE api_keys SET expires_at = greatest(expires_at, $2) WHERE id = $1`, keyID, expires); err != nil {
		return "", "", time.Time{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE signing_keys SET renewed_until = greatest(renewed_until, $2) WHERE id = $1`, key.ID, expires)
	return plain, jti, expires, err
}

// listedSettle is how long a token stored for the gateways to list is kept
// from the key's helper: the sync its renewal queued has to take it to
// every gateway first, which takes seconds (a full sync of 1,000 tenants
// took 10.8 s, 0.30.0).
const listedSettle = 2 * time.Minute

// liveAPIKeys holds while some enabled tenant has a live API key: the
// gateways then take API keys besides tokens (IdentityMode).
const liveAPIKeys = `EXISTS (SELECT 1 FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
                             WHERE k.kind = 'key' AND k.revoked_at IS NULL AND t.enabled)`

// takesAPIKeys reports whether the gateways take API keys besides tokens,
// or will once the hub signs its first token.
func takesAPIKeys(ctx context.Context, tx pgx.Tx) (bool, error) {
	var keys bool
	err := tx.QueryRow(ctx, `SELECT `+liveAPIKeys).Scan(&keys)
	return keys, err
}

// listedToken gives a personal key's helper a token the gateways list. While
// they take API keys too, their API key check runs before the JWT check and
// lets through only the credentials in its Secret, so a token signed on the
// spot would be refused until a sync took it there. The key's tokens are
// stored, for RenderState to list, and the helper is given the newest that
// has been stored for listedSettle, so renewing every few minutes changes
// nothing at the gateways. A new one is signed once the newest is half its
// life old; the helper moves to it listedSettle later, and a key has at
// most two at a time. A key with none, such as one made before the hub
// listed them, gets one at once, which the gateways take a sync later.
// Listed is set when a token was signed.
func (s *Store) listedToken(ctx context.Context, tx pgx.Tx, keyID, clientID string, t Tenant, set TokenSettings, now time.Time) (plain string, expires time.Time, listed bool, err error) {
	if _, err := tx.Exec(ctx, `DELETE FROM personal_tokens WHERE key_id = $1 AND expires_at <= $2`, keyID, now); err != nil {
		return "", time.Time{}, false, err
	}
	type stored struct {
		enc             []byte
		issued, expires time.Time
	}
	rows, err := tx.Query(ctx, `SELECT token_enc, issued_at, expires_at FROM personal_tokens WHERE key_id = $1 ORDER BY issued_at DESC`, keyID)
	if err != nil {
		return "", time.Time{}, false, err
	}
	tokens, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (stored, error) {
		var st stored
		return st, r.Scan(&st.enc, &st.issued, &st.expires)
	})
	if err != nil {
		return "", time.Time{}, false, err
	}
	if len(tokens) == 0 || now.Sub(tokens[0].issued) >= set.RenewedLifetime/2 {
		plain, jti, expires, err := s.signRenewed(ctx, tx, keyID, clientID, t, set, now)
		if err != nil {
			return "", time.Time{}, false, err
		}
		enc, err := s.box.Seal([]byte(plain))
		if err != nil {
			return "", time.Time{}, false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO personal_tokens (jti, key_id, token_enc, issued_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
			jti, keyID, enc, now, expires); err != nil {
			return "", time.Time{}, false, err
		}
		tokens = append([]stored{{enc: enc, issued: now, expires: expires}}, tokens...)
		listed = true
	}
	pick := tokens[0]
	for _, tk := range tokens {
		if now.Sub(tk.issued) >= listedSettle && tk.expires.Sub(now) > listedSettle {
			pick = tk
			break
		}
	}
	raw, err := s.box.Open(pick.enc)
	if err != nil {
		return "", time.Time{}, false, err
	}
	return string(raw), pick.expires, listed, nil
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
// DenialMargin after the last token it got expires. Expired listed tokens
// go too.
func (s *Store) DeniedClients(ctx context.Context) ([]string, error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM client_denials WHERE expires_at <= $1`, time.Now().Add(-DenialMargin)); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM personal_tokens WHERE expires_at <= now()`); err != nil {
		return nil, err
	}
	return list(ctx, s, func(r scanner) (string, error) {
		var id string
		return id, r.Scan(&id)
	}, `SELECT client_id FROM client_denials ORDER BY client_id`)
}
