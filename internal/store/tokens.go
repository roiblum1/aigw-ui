package store

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"aigw-ui/internal/render"
	"aigw-ui/internal/token"
)

// Kinds of key.
const (
	KindKey   = "key"   // an API key, "sk-…", that every gateway lists
	KindToken = "token" // a token the hub signed, which the gateways check
	// KindPersonal is a secret a user's helper trades at the hub for a
	// token that lives a few days; the gateways never see it.
	KindPersonal = "personal"
)

// TokenSettings are what the hub writes into the tokens it signs.
type TokenSettings struct {
	Issuer, Audience string
	Lifetime         time.Duration
	// RenewedLifetime is how long a token for a personal key lives: how
	// long a user goes on while the hub cannot be reached, and how long a
	// token with an older budget can be kept from being renewed.
	RenewedLifetime time.Duration
}

// SigningKey is one of the hub's signing keys, without its private half.
type SigningKey struct {
	ID        string     `json:"id"`
	CreatedAt time.Time  `json:"created_at"`
	RetiredAt *time.Time `json:"retired_at"`
	// Tokens is how many unrevoked tokens it signed that have not expired,
	// and RenewedUntil when the last token it signed for a personal key
	// expires: while there are any, the gateways keep its public key.
	Tokens       int        `json:"tokens"`
	RenewedUntil *time.Time `json:"renewed_until"`
	JWK          token.JWK  `json:"-"`
}

// ListSigningKeys returns every signing key, the one that signs first.
func (s *Store) ListSigningKeys(ctx context.Context) ([]SigningKey, error) {
	return list(ctx, s, func(r scanner) (SigningKey, error) {
		var k SigningKey
		var jwk []byte
		err := r.Scan(&k.ID, &k.CreatedAt, &k.RetiredAt, &k.Tokens, &k.RenewedUntil, &jwk)
		if err == nil {
			err = json.Unmarshal(jwk, &k.JWK)
		}
		return k, err
	}, `SELECT s.id, s.created_at, s.retired_at,
	           (SELECT count(*) FROM api_keys k WHERE k.signing_key_id = s.id AND k.revoked_at IS NULL AND k.expires_at > now()),
	           s.renewed_until, s.public_jwk
	    FROM signing_keys s ORDER BY s.retired_at IS NOT NULL, s.created_at DESC`)
}

// GatewayKeys returns the public keys the gateways check tokens against:
// the one that signs, and every retired one while a token it signed is
// still valid. A retired key with none left is no longer published.
func GatewayKeys(keys []SigningKey) []token.JWK {
	var out []token.JWK
	now := time.Now()
	for _, k := range keys {
		if k.RetiredAt == nil || k.Tokens > 0 || (k.RenewedUntil != nil && k.RenewedUntil.After(now)) {
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
		claims := token.Claims{Issuer: set.Issuer, Audience: set.Audience, Subject: t.Slug, ID: jti, ClientID: clientID,
			IssuedAt: now.Unix(), ExpiresAt: expires.Unix()}
		if err := tokenLimits(ctx, tx, t, &claims); err != nil {
			return err
		}
		plain, err = token.Sign(key, claims)
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

// tokenLimits writes what the gateway holds a tenant to into its token's
// claims: its budget across models, the amount of each own quota by model
// slug and, when it is held to some, its models by the names clients send.
func tokenLimits(ctx context.Context, tx pgx.Tx, t Tenant, c *token.Claims) error {
	if t.Budget != nil {
		if unit := render.BudgetUnits[t.Budget.Window]; unit != "" {
			c.RequestsPerUnit, c.Unit = t.Budget.Credits, unit
		}
	}
	// The amount of each own quota, by model slug.
	rows, err := tx.Query(ctx, `SELECT m.slug, q.token_limit, q.window_size, q.shadow FROM quotas q JOIN models m ON m.id = q.model_id WHERE q.tenant_id = $1`, t.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var slug, window string
		var limit int64
		var shadow bool
		if err := rows.Scan(&slug, &limit, &window, &shadow); err != nil {
			rows.Close()
			return err
		}
		if c.Quotas == nil {
			c.Quotas = map[string]string{}
		}
		c.Quotas[slug] = render.QuotaAmountName(limit, window, shadow)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if t.Models == nil {
		return nil
	}
	names := []string{}
	rows, err = tx.Query(ctx, `SELECT name FROM models WHERE id::text = ANY($1) ORDER BY name`, t.Models)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
	}
	c.Models = &names
	return rows.Err()
}

// TenantOfToken returns the enabled tenant a token belongs to, once its
// claims were checked: the key must be one of the tenant's, a token, and
// neither revoked nor expired; or the token one a live personal key got.
func (s *Store) TenantOfToken(ctx context.Context, c token.Claims) (Tenant, error) {
	var tenantID string
	err := s.db.QueryRow(ctx,
		`SELECT k.tenant_id FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.jti = $1 AND k.kind = 'token' AND k.revoked_at IS NULL AND k.expires_at > now() AND t.slug = $2 AND t.enabled`,
		c.ID, c.Subject).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.tenantOfRenewed(ctx, c)
	}
	if err != nil {
		return Tenant{}, mapErr(err)
	}
	return s.GetTenant(ctx, tenantID)
}

// DenialMargin is how long a denial outlives its token. A gateway takes a
// token up to 60 seconds past its expiry, Envoy's allowance for the clocks of
// the hub and the gateway to differ, so a denial dropped at the expiry would
// let a revoked token through again for that long.
const DenialMargin = 5 * time.Minute

// DeniedTokens returns every token the gateways must refuse although it is
// signed: revoked, or of a deleted tenant, until DenialMargin after it
// expires. Rows past that are removed on the way.
//
// Runs of them go into spans by expiry instead of a jti each (spansOf): a
// test that deletes its tenants leaves hundreds of denials, each of which
// stayed listed for the 180 days its token lived, and the policy grew by one
// line per token (the sandbox, 2026-10-17). The denials and the live tokens
// are read in one snapshot, so every stored token that has not expired is
// in one of the two.
func (s *Store) DeniedTokens(ctx context.Context, now time.Time) (singles []string, spans []render.ExpirySpan, err error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM token_denials WHERE expires_at <= $1`, now.Add(-DenialMargin)); err != nil {
		return nil, nil, err
	}
	var denied []denial
	var live []int64
	err = pgx.BeginTxFunc(ctx, s.db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		// floor, as a token's exp is: expires_at has had whole seconds
		// since tokens came (0.18.0), and a rounded bound one second off
		// would let the token at a span's edge through.
		rows, err := tx.Query(ctx, `SELECT jti, floor(extract(epoch FROM expires_at))::bigint FROM token_denials ORDER BY expires_at, jti`)
		if err != nil {
			return err
		}
		if denied, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (denial, error) {
			var d denial
			return d, r.Scan(&d.jti, &d.exp)
		}); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT DISTINCT floor(extract(epoch FROM expires_at))::bigint FROM api_keys
		                           WHERE kind = 'token' AND revoked_at IS NULL AND expires_at > $1 ORDER BY 1`, now.Add(-DenialMargin))
		if err != nil {
			return err
		}
		live, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	singles, spans = spansOf(denied, live, now.Add(s.tokens.Lifetime-spanRecent).Unix())
	return singles, spans, nil
}

// denial is a denied token: its jti and its expiry (exp, Unix seconds).
type denial struct {
	jti string
	exp int64
}

// minSpan is the fewest denied tokens a span stands for: a span's entry in
// the rule, two comparisons Envoy can build, is about as long as eight jtis.
const minSpan = 10

// spanRecent keeps the denials of the tokens signed in its last stretch out
// of the spans. A token's expiry is when it was signed plus the lifetime, so
// a token signed while the spans are worked out, and stored just after,
// expires later than every span ends: it is not refused by mistake.
const spanRecent = 10 * time.Minute

// spansOf puts the denied tokens, sorted by expiry, into spans of at least
// minSpan that no live token (expiries sorted) expires within, and returns
// the rest as jtis, sorted. A denial that expires at cutoff or later, or in
// the same second as a live token, stays a jti.
func spansOf(denied []denial, live []int64, cutoff int64) (singles []string, spans []render.ExpirySpan) {
	var run []denial
	flush := func() {
		if len(run) >= minSpan {
			spans = append(spans, render.ExpirySpan{From: run[0].exp, To: run[len(run)-1].exp, Tokens: len(run)})
		} else {
			for _, d := range run {
				singles = append(singles, d.jti)
			}
		}
		run = nil
	}
	next := 0 // the first live expiry not yet passed
	for _, d := range denied {
		for next < len(live) && live[next] < d.exp {
			// A live token expires between the run's last denial and this
			// one: the run ends there.
			flush()
			next++
		}
		if d.exp >= cutoff || (next < len(live) && live[next] == d.exp) {
			flush()
			singles = append(singles, d.jti)
			continue
		}
		run = append(run, d)
	}
	flush()
	sort.Strings(singles)
	return singles, spans
}

// Identity is how the gateways tell tenants apart, the same on every
// cluster: API keys until the hub signs its first token, then API keys and
// tokens while any enabled tenant has an API key left, then tokens only.
func (s *Store) Identity(ctx context.Context) (render.Identity, error) {
	keys, err := s.ListSigningKeys(ctx)
	if err != nil || len(keys) == 0 {
		return render.Identity{Mode: render.IdentityKeys}, err
	}
	id := render.Identity{Issuer: s.tokens.Issuer, Audience: s.tokens.Audience, JWKS: token.JWKS(GatewayKeys(keys))}
	if id.Mode, err = s.IdentityMode(ctx); err != nil {
		return id, err
	}
	if id.Denied, id.DeniedSpans, err = s.DeniedTokens(ctx, time.Now()); err != nil {
		return id, err
	}
	if id.DeniedClients, err = s.DeniedClients(ctx); err != nil {
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

// IdentityMode is Identity's mode alone, without reading anything else.
func (s *Store) IdentityMode(ctx context.Context) (string, error) {
	var signed, apiKeys bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM signing_keys), `+liveAPIKeys).Scan(&signed, &apiKeys)
	switch {
	case err != nil || !signed:
		return render.IdentityKeys, mapErr(err)
	case apiKeys:
		return render.IdentityBoth, nil
	}
	return render.IdentityTokens, nil
}
