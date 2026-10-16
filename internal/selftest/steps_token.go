package selftest

import (
	"fmt"
	"net/http"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
	"aigw-ui/internal/token"
)

// tokenLife is how long the self-test's tokens last: once its tenant is
// deleted they are denied, and the denial goes with the token's expiry.
const tokenLife = time.Hour

func (t *test) tokenSettings() store.TokenSettings {
	set := t.r.st.Tokens()
	set.Lifetime = tokenLife
	return set
}

// expiredAgo is how long ago the self-test's expired token expired. A gateway
// takes a token up to 60 seconds past its expiry, Envoy's allowance for the
// clocks of the hub and the gateway to differ, which Envoy Gateway does not
// let a policy change; a token that expired a minute ago is still taken when
// it is checked in the second it was signed.
const expiredAgo = 10 * time.Minute

// badTokens returns a token for the temporary tenant signed by a key the hub
// does not have, and one signed by the hub that expired expiredAgo ago.
func (t *test) badTokens() (forged, expired string, err error) {
	set, now := t.r.st.Tokens(), time.Now()
	claims := token.Claims{Issuer: set.Issuer, Audience: set.Audience, Subject: t.tenant.Slug, ID: "00000000selftest",
		ClientID: t.tenant.Slug + ".00000000", IssuedAt: now.Add(-time.Hour).Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	other, err := token.NewKey()
	if err != nil {
		return "", "", err
	}
	if forged, err = token.Sign(other, claims); err != nil {
		return "", "", err
	}
	claims.ExpiresAt = now.Add(-expiredAgo).Unix()
	expired, err = t.r.st.SignUnstored(t.ctx, claims)
	return forged, expired, err
}

// revokedToken gives the temporary tenant a second token, waits until the
// gateway takes it, revokes it, and waits until the gateway refuses it.
// Taken means not refused as unknown: the tenant's quota may be spent.
func (t *test) revokedToken() (string, string) {
	switch {
	case t.key == "":
		return Skipped, "\"Enforce API keys\" is off for this cluster, so the gateway takes requests without a key."
	case !t.tokens:
		return Skipped, "The hub has issued no token yet: the gateway takes API keys only."
	}
	k, second, err := t.r.st.CreateToken(t.ctx, t.tenant.ID, "self-test, revoked", t.tokenSettings(), time.Now())
	if err != nil {
		return Failed, err.Error()
	}
	if _, err := t.sync("Self-test of " + t.cluster.Name + ": a second token for " + t.tenant.Slug); err != nil {
		return Failed, "Sync: " + err.Error()
	}
	if status, ok := t.until(second, false); !ok {
		return Failed, fmt.Sprintf("The gateway never took the second token within %d seconds (last: %s).", int(keyWait.Seconds()), status)
	}
	if _, err := t.r.st.RevokeKey(t.ctx, k.ID); err != nil {
		return Failed, err.Error()
	}
	start := time.Now()
	if _, err := t.sync("Self-test of " + t.cluster.Name + ": revoked the second token of " + t.tenant.Slug); err != nil {
		return Failed, "Sync: " + err.Error()
	}
	status, ok := t.until(second, true)
	if !ok {
		return Failed, fmt.Sprintf("The gateway still takes a revoked token %d seconds after the sync (last: %s): the deny rule is not in effect.",
			int(keyWait.Seconds()), status)
	}
	return Passed, fmt.Sprintf("Refused with %s %d seconds after the sync that revoked it.", status, int(time.Since(start).Seconds()))
}

// personalKey gives the temporary tenant a personal key and trades it for
// a token, as a user's helper does at POST /api/v1/token, and expects the
// gateway to take that token. Then it revokes the key and expects the
// token to be refused: tokens a personal key got are not stored, so the
// gateway refuses them by the key's client ID.
func (t *test) personalKey() (string, string) {
	switch {
	case t.key == "":
		return Skipped, "\"Enforce API keys\" is off for this cluster, so the gateway takes requests without a key."
	case !t.tokens:
		return Skipped, "The hub has issued no token yet: the gateway takes API keys only."
	}
	k, secret, err := t.r.st.CreatePersonalKey(t.ctx, t.tenant.ID, "self-test, personal")
	if err != nil {
		return Failed, err.Error()
	}
	set := t.tokenSettings()
	set.RenewedLifetime = tokenLife
	renewed, _, _, err := t.r.st.RenewToken(t.ctx, secret, set, time.Now())
	if err != nil {
		return Failed, "The personal key got no token: " + err.Error()
	}
	if status, ok := t.until(renewed, false); !ok {
		return Failed, fmt.Sprintf("The gateway never took the token the personal key got within %d seconds (last: %s).", int(keyWait.Seconds()), status)
	}
	if _, err := t.r.st.RevokeKey(t.ctx, k.ID); err != nil {
		return Failed, err.Error()
	}
	start := time.Now()
	if _, err := t.sync("Self-test of " + t.cluster.Name + ": revoked the personal key of " + t.tenant.Slug); err != nil {
		return Failed, "Sync: " + err.Error()
	}
	status, ok := t.until(renewed, true)
	if !ok {
		return Failed, fmt.Sprintf("The gateway still takes the token of a revoked personal key %d seconds after the sync (last: %s): the rule on its client ID is not in effect.",
			int(keyWait.Seconds()), status)
	}
	return Passed, fmt.Sprintf("The token the personal key got was answered; once the key was revoked it was refused with %s %d seconds after the sync.",
		status, int(time.Since(start).Seconds()))
}

// until sends requests with key until the gateway refuses it (refused) or
// takes it (!refused), and reports the last status and whether it came.
func (t *test) until(key string, refused bool) (string, bool) {
	start := time.Now()
	last := "no answer"
	for time.Since(start) < keyWait {
		res, err := t.chat(key)
		if err == nil {
			last = fmt.Sprintf("HTTP %d", res.Status)
			denied := res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden
			if denied == refused {
				return last, true
			}
		}
		if !t.wait() {
			break
		}
	}
	return last, false
}

// standard reports whether the gateway counts the model by its standard: it
// has one, and the gateways take tokens only.
func (t *test) standard() bool {
	return t.model.Standard != nil && t.mode == render.IdentityTokens && t.cluster.AuthEnabled
}

// tenantHeader sends a request with the temporary tenant's token and another
// tenant's slug in the tenant header. It has to be counted for the token's
// tenant, and nothing for the slug the client sent: under a standard every
// tenant has a bucket of its own on one rule, so a header the client could
// set would let it spend under any name, made up or another tenant's.
func (t *test) tenantHeader() (string, string) {
	switch {
	case !t.tokens:
		return Skipped, "The hub has issued no token yet: the gateway does not set the tenant header."
	case !t.standard():
		return Skipped, "Nothing counts by the tenant header on this model: it has no standard budget, or the gateways still take API keys."
	case t.r.usage == nil:
		return Skipped, "Usage monitoring is off: the server has no Redis configured."
	}
	other := TenantPrefix + "nobody"
	before, err := t.quota()
	if err != nil {
		return Failed, "Redis: " + err.Error()
	}
	res, err := t.chatWith(map[string]string{render.TenantHeader: other})
	switch {
	case err != nil:
		return Failed, err.Error()
	case res.Status != http.StatusOK:
		return Failed, "The request was not answered (" + answer(res) + "), so nothing can be said."
	}
	start := time.Now()
	for time.Since(start) < counterWait {
		now, err := t.quota()
		if err != nil {
			return Failed, "Redis: " + err.Error()
		}
		// On a model in best-effort mode the step before moved the tenant
		// to the best-effort route, which counts apart.
		if now.Used+now.OverageUsed > before.Used+before.OverageUsed {
			theirs, _, err := t.r.usage.StandardUsed(t.ctx, t.model.Slug, other, time.Now())
			if err != nil {
				return Failed, "Redis: " + err.Error()
			}
			if theirs > 0 {
				return Failed, fmt.Sprintf("The request was counted for the tenant of its token, and %d for %q, the tenant the client named: the gateway passes on a tenant header the client sends.", theirs, other)
			}
			return Passed, "A request that named " + other + " in " + render.TenantHeader + " was counted for the tenant of its token, and nothing for " + other + ". The gateway replaces the header."
		}
		if !t.wait() {
			break
		}
	}
	return Failed, fmt.Sprintf("The request was answered but not counted for the tenant of its token within %d seconds.", int(counterWait.Seconds()))
}
