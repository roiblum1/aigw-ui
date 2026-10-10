package selftest

import (
	"fmt"
	"net/http"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
	"aigw-ui/internal/usage"
)

// hasBestEffortRoute reports whether the cluster under test gets the model's
// best-effort route. A cluster outside the fleet has only its own route for
// the model, and refuses a tenant past its budget whatever the model says.
func (t *test) hasBestEffortRoute() bool {
	return t.cluster.FleetEnabled && t.model.Fleet && t.model.SpentMode == store.SpentBestEffort
}

// bestEffort reports whether a tenant past its budget is moved to
// best-effort on this cluster and the server can do the moving.
func (t *test) bestEffort() bool { return t.hasBestEffortRoute() && t.r.overage != nil }

// quota returns the temporary tenant's usage on the model.
func (t *test) quota() (usage.Quota, error) {
	rep, err := t.r.usage.Report(t.ctx, t.tenant.ID)
	if err != nil {
		return usage.Quota{}, err
	}
	for _, q := range rep.Quotas {
		if q.ModelID == t.model.ID {
			return q, nil
		}
	}
	return usage.Quota{}, fmt.Errorf("the tenant's quota is not in the usage report")
}

// overage expects the tenant to be moved to the best-effort route: its first
// request used up the 1-token quota. An answer alone proves nothing, because
// a tenant over its own quota can also be answered from the model's pool.
// The proof is where the tokens are counted: on the best-effort route's
// counter, with the counter of the model's own route standing still.
func (t *test) overage() (string, string) {
	switch {
	case t.model.SpentMode != store.SpentBestEffort:
		return Skipped, "The model refuses a tenant past its budget. That is the step before this one."
	case t.model.PriceDryRun:
		return Skipped, "The model's prices are in dry-run, and nobody is moved to best-effort during it."
	case !t.hasBestEffortRoute():
		return Skipped, "This cluster has no entry route for the model, so it has no best-effort route either. It refuses a tenant past its budget, which the step before this one checks."
	case t.r.overage == nil || t.r.usage == nil:
		return Skipped, "The server has no Redis configured, so it cannot see that a budget is spent."
	case t.key == "":
		return Skipped, "Without API keys the gateway cannot tell tenants apart."
	case !t.counted:
		return Failed, "The tenant's first request was not counted, so the server cannot see that its budget is spent."
	}
	// The loop would get here by itself within one interval.
	if err := t.r.overage.Tick(t.ctx); err != nil {
		return Failed, "Reading the counters: " + err.Error()
	}
	moved, err := t.quota()
	if err != nil {
		return Failed, err.Error()
	}
	if moved.BestEffortUntil == nil {
		return Failed, fmt.Sprintf("The tenant has used %d of %d tokens and was not moved to best-effort.", moved.Used, moved.Limit)
	}
	if _, err := t.sync("Self-test of " + t.cluster.Name + ": moved the temporary tenant to best-effort"); err != nil {
		return Failed, "The tenant is moved here, but the cluster could not be synced: " + err.Error()
	}

	// Requests that were on their way during the move are still counted on
	// the model's own route, so that counter is compared between two looks.
	own := moved.Used
	wait := max(2*t.r.overage.Every(), limitWait)
	start := time.Now()
	last := "no request was answered"
	for time.Since(start) < wait {
		res, err := t.chat(t.key)
		switch {
		case err != nil:
			last = err.Error()
		case res.Status != http.StatusOK:
			last = answer(res)
		default:
			last = "requests were answered"
			now, err := t.quota()
			if err != nil {
				return Failed, "Redis: " + err.Error()
			}
			switch {
			case now.Used > own:
				last = fmt.Sprintf("the answered requests were still counted on the model's own route (%d tokens, %d before)", now.Used, own)
				own = now.Used
			case now.OverageUsed > 0:
				return Passed, fmt.Sprintf("Answered after the 1-token budget was spent. %d tokens were counted on the best-effort route and the counter of the model's own route stayed at %d.", now.OverageUsed, now.Used)
			}
		}
		if !t.wait() {
			break
		}
	}
	return Failed, fmt.Sprintf("After %d seconds the tenant's requests did not show up on the best-effort route: %s. Check that the route %s is accepted on the cluster and that it wins over the model's entry route.", int(wait.Seconds()), last, render.BestEffortName(t.model.Slug))
}
