package selftest

import (
	"fmt"
	"net/http"
	"time"

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

// badTokens returns a token for the temporary tenant signed by a key the hub
// does not have, and one signed by the hub that expired a minute ago.
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
	claims.ExpiresAt = now.Add(-time.Minute).Unix()
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
