package store

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/render"
	"aigw-ui/internal/token"
)

// Kinds of key.
const (
	KindKey   = "key"   // an API key, "sk-…", that every gateway lists
	KindToken = "token" // a token the hub signed, which the gateways check
)

// TokenSettings are what the hub writes into the tokens it signs.
type TokenSettings struct {
	Issuer, Audience string
	Lifetime         time.Duration
}

// SigningKey is one of the hub's signing keys, without its private half.
type SigningKey struct {
	ID        string     `json:"id"`
	CreatedAt time.Time  `json:"created_at"`
	RetiredAt *time.Time `json:"retired_at"`
	// Tokens is how many unrevoked tokens it signed that have not expired:
	// while there are any, the gateways keep its public key.
	Tokens int       `json:"tokens"`
	JWK    token.JWK `json:"-"`
}

// ListSigningKeys returns every signing key, the one that signs first.
func (s *Store) ListSigningKeys(ctx context.Context) ([]SigningKey, error) {
	return list(ctx, s, func(r scanner) (SigningKey, error) {
		var k SigningKey
		var jwk []byte
		err := r.Scan(&k.ID, &k.CreatedAt, &k.RetiredAt, &k.Tokens, &jwk)
		if err == nil {
			err = json.Unmarshal(jwk, &k.JWK)
		}
		return k, err
	}, `SELECT s.id, s.created_at, s.retired_at,
	           (SELECT count(*) FROM api_keys k WHERE k.signing_key_id = s.id AND k.revoked_at IS NULL AND k.expires_at > now()),
	           s.public_jwk
	    FROM signing_keys s ORDER BY s.retired_at IS NOT NULL, s.created_at DESC`)
}

// GatewayKeys returns the public keys the gateways check tokens against:
// the one that signs, and every retired one while a token it signed is
// still valid. A retired key with none left is no longer published.
func GatewayKeys(keys []SigningKey) []token.JWK {
	var out []token.JWK
	for _, k := range keys {
		if k.RetiredAt == nil || k.Tokens > 0 {
			out = append(out, k.JWK)
		}
	}
	return out
}

// PublicKeys returns the keys GatewayKeys publishes, by kid, for checking a
// token on the hub itself.
func PublicKeys(keys []SigningKey) map[string]*ecdsa.PublicKey {
	out := map[string]*ecdsa.PublicKey{}
	for _, j := range GatewayKeys(keys) {
		if pub, err := j.Public(); err == nil {
			out[j.Kid] = pub
		}
	}
	return out
}

// signer returns the key that signs new tokens, making the first one if
// there is none yet.
func (s *Store) signer(ctx context.Context, tx pgx.Tx) (token.Key, error) {
	var enc []byte
	err := tx.QueryRow(ctx, `SELECT private_enc FROM signing_keys WHERE retired_at IS NULL`).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.addSigningKey(ctx, tx)
	}
	if err != nil {
		return token.Key{}, err
	}
	der, err := s.box.Open(enc)
	if err != nil {
		return token.Key{}, err
	}
	return token.ParseKey(der)
}

func (s *Store) addSigningKey(ctx context.Context, tx pgx.Tx) (token.Key, error) {
	k, err := token.NewKey()
	if err != nil {
		return token.Key{}, err
	}
	der, err := k.Marshal()
	if err != nil {
		return token.Key{}, err
	}
	enc, err := s.box.Seal(der)
	if err != nil {
		return token.Key{}, err
	}
	jwk, err := json.Marshal(k.PublicJWK())
	if err != nil {
		return token.Key{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO signing_keys (id, private_enc, public_jwk) VALUES ($1, $2, $3)`, k.ID, enc, jwk)
	return k, err
}

// RotateSigningKey retires the key that signs and makes a new one, which
// signs from now on. Tokens the old key signed keep working until they
// expire or are revoked; until then the gateways keep its public key.
func (s *Store) RotateSigningKey(ctx context.Context) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE signing_keys SET retired_at = now() WHERE retired_at IS NULL`); err != nil {
			return err
		}
		k, err := s.addSigningKey(ctx, tx)
		id = k.ID
		return err
	})
	return id, mapErr(err)
}

// CreateToken signs a token for a tenant and stores it, encrypted, as one of
// its keys: while some tenant still has an API key, every gateway lists the
// tokens too. It returns the record and the token, which is shown once.
func (s *Store) CreateToken(ctx context.Context, tenantID, name string, set TokenSettings, now time.Time) (APIKey, string, error) {
	t, err := s.GetTenant(ctx, tenantID)
	if err != nil {
		return APIKey{}, "", err
	}
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return APIKey{}, "", err
	}
	jti := hex.EncodeToString(raw)
	clientID := t.Slug + "." + jti
	var k APIKey
	var plain string
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// One signer at a time, so two first tokens make one key.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('aigw-ui signing key'))`); err != nil {
			return err
		}
		key, err := s.signer(ctx, tx)
		if err != nil {
			return err
		}
		expires := now.Add(set.Lifetime).Truncate(time.Second)
		plain, err = token.Sign(key, token.Claims{Issuer: set.Issuer, Audience: set.Audience, Subject: t.Slug, ID: jti, ClientID: clientID,
			IssuedAt: now.Unix(), ExpiresAt: expires.Unix()})
		if err != nil {
			return err
		}
		enc, err := s.box.Seal([]byte(plain))
		if err != nil {
			return err
		}
		k, err = scanKey(tx.QueryRow(ctx,
			`INSERT INTO api_keys (tenant_id, name, client_id, key_prefix, key_enc, kind, jti, signing_key_id, expires_at)
			 VALUES ($1, $2, $3, $4, $5, 'token', $6, $7, $8) RETURNING `+keyColumns,
			tenantID, name, clientID, plain[len(plain)-keyPrefixLen:], enc, jti, key.ID, expires))
		return err
	})
	return k, plain, mapErr(err)
}

// TenantOfToken returns the enabled tenant a token belongs to, once its
// claims were checked: the key must be one of the tenant's, a token, and
// neither revoked nor expired.
func (s *Store) TenantOfToken(ctx context.Context, c token.Claims) (Tenant, error) {
	var tenantID string
	err := s.db.QueryRow(ctx,
		`SELECT k.tenant_id FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.jti = $1 AND k.kind = 'token' AND k.revoked_at IS NULL AND k.expires_at > now() AND t.slug = $2 AND t.enabled`,
		c.ID, c.Subject).Scan(&tenantID)
	if err != nil {
		return Tenant{}, mapErr(err)
	}
	return s.GetTenant(ctx, tenantID)
}

// DeniedTokens returns the jti of every token the gateways must refuse
// although it is signed: revoked, or of a deleted tenant, until it expires.
// Rows of tokens that have expired are removed on the way.
func (s *Store) DeniedTokens(ctx context.Context) ([]string, error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM token_denials WHERE expires_at <= now()`); err != nil {
		return nil, err
	}
	return list(ctx, s, func(r scanner) (string, error) {
		var jti string
		return jti, r.Scan(&jti)
	}, `SELECT jti FROM token_denials ORDER BY jti`)
}

// Identity is how the gateways tell tenants apart, the same on every
// cluster: API keys until the hub signs its first token, then API keys and
// tokens while any enabled tenant has an API key left, then tokens only.
func (s *Store) Identity(ctx context.Context) (render.Identity, error) {
	keys, err := s.ListSigningKeys(ctx)
	if err != nil || len(keys) == 0 {
		return render.Identity{Mode: render.IdentityKeys}, err
	}
	id := render.Identity{Mode: render.IdentityTokens, Issuer: s.tokens.Issuer, Audience: s.tokens.Audience, JWKS: token.JWKS(GatewayKeys(keys))}
	var apiKeys bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
	                              WHERE k.kind = 'key' AND k.revoked_at IS NULL AND t.enabled)`).Scan(&apiKeys); err != nil {
		return id, err
	}
	if apiKeys {
		id.Mode = render.IdentityBoth
	}
	if id.Denied, err = s.DeniedTokens(ctx); err != nil {
		return id, err
	}
	id.Disabled, err = list(ctx, s, func(r scanner) (string, error) {
		var slug string
		return slug, r.Scan(&slug)
	}, `SELECT slug FROM tenants WHERE NOT enabled ORDER BY slug`)
	return id, err
}

// SignUnstored signs claims with the key that signs now, and stores nothing:
// the self-test's expired token. Nil error and "" when the hub has no
// signing key yet.
func (s *Store) SignUnstored(ctx context.Context, c token.Claims) (string, error) {
	var enc []byte
	err := s.db.QueryRow(ctx, `SELECT private_enc FROM signing_keys WHERE retired_at IS NULL`).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	der, err := s.box.Open(enc)
	if err != nil {
		return "", err
	}
	k, err := token.ParseKey(der)
	if err != nil {
		return "", err
	}
	return token.Sign(k, c)
}
