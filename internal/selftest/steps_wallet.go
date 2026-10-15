package selftest

import (
	"fmt"
	"net/http"
	"time"

	"aigw-ui/internal/store"
)

// walletWait is how long the wallet step waits for the hub to close the
// tenant: the usage history records the spend once a minute, and the wallet
// service looks every OVERAGE_INTERVAL.
const walletWait = 150 * time.Second

// wallet gives the temporary tenant a wallet of 1 credit an hour, which its
// first request spent, and waits until the hub closes it and the gateway
// refuses it with 403, the authorization rule's answer, not the quota's 429.
func (t *test) wallet() (string, string) {
	switch {
	case t.key == "":
		return Skipped, "\"Enforce API keys\" is off for this cluster, so the gateway does not tell tenants apart."
	case t.model.Unit != store.UnitCredits:
		return Skipped, "A wallet covers priced models only, and this model has no prices."
	case t.model.PriceDryRun:
		return Skipped, "The model's prices are in dry-run: nobody is refused."
	case t.bestEffort():
		return Skipped, "The model serves a tenant past its budget as best-effort, and so a tenant whose wallet is spent: the best-effort step covers that."
	case t.r.usage == nil:
		return Skipped, "Wallets are added up from the usage history, which needs Redis; none is configured."
	case !t.counted:
		return Skipped, "Needs a counted request first."
	}
	if _, err := t.r.st.SetWallet(t.ctx, t.tenant.ID, &store.Wallet{Credits: 1, Window: "1h"}); err != nil {
		return Failed, err.Error()
	}
	start := time.Now()
	for {
		tn, err := t.r.st.GetTenant(t.ctx, t.tenant.ID)
		if err != nil {
			return Failed, err.Error()
		}
		if tn.ClosedUntil != nil {
			break
		}
		if time.Since(start) > walletWait {
			return Failed, fmt.Sprintf("The hub did not close the tenant within %d seconds of a wallet of 1 credit it had spent. Is the usage history on (config.usageHistoryInterval)?", int(walletWait.Seconds()))
		}
		if !t.wait() {
			return Failed, "The run ended."
		}
	}
	closed := time.Since(start)
	if _, err := t.sync("Self-test of " + t.cluster.Name + ": the wallet of " + t.tenant.Slug + " is spent"); err != nil {
		return Failed, "Sync: " + err.Error()
	}
	refusedAt := time.Now()
	last := "no answer"
	for time.Since(refusedAt) < keyWait {
		res, err := t.chat(t.key)
		if err == nil {
			last = answer(res)
			if res.Status == http.StatusForbidden {
				return Passed, fmt.Sprintf("The hub closed the tenant %d seconds after its wallet was set, and the gateway refused it with HTTP 403 %d seconds after the sync.",
					int(closed.Seconds()), int(time.Since(refusedAt).Seconds()))
			}
		}
		if !t.wait() {
			break
		}
	}
	return Failed, fmt.Sprintf("The hub closed the tenant, but the gateway did not refuse it with HTTP 403 within %d seconds (last: %s): the wallet rule is not in effect.",
		int(keyWait.Seconds()), last)
}
