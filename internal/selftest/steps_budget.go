package selftest

import (
	"fmt"
	"net/http"
	"time"

	"aigw-ui/internal/gateway"
	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// budgetWait is how long the budget step waits for the gateway to hold the
// tenant to a budget it was just given: the policy travels through Envoy
// Gateway's controller to the proxy and the global rate limit service.
const budgetWait = 90 * time.Second

// budgetTokensOnly reports whether the gateway reads the tenant header a
// token sets, which a budget across models and a tenant's list of models
// are enforced through.
func (t *test) budgetTokensOnly() bool {
	return t.mode == render.IdentityTokens && t.cluster.AuthEnabled
}

// budget gives the temporary tenant a budget of 1 credit across models and
// a quota on the model that cannot run out, so that only the budget can
// refuse. A long request is answered and its cost in credits counted where
// the Usage page reads it; the next request is refused with HTTP 429.
func (t *test) budget() (string, string) {
	switch {
	case t.key == "":
		return Skipped, "Without API keys the gateway cannot tell tenants apart, so there is no tenant budget."
	case !t.budgetTokensOnly():
		return Skipped, "A budget across models needs the gateways on tokens only: its rule reads the tenant header a token sets."
	case t.model.Prices == nil:
		return Skipped, "The model has no prices: a budget across models counts priced models only."
	case t.r.usage == nil:
		return Skipped, "Usage monitoring is off: the server has no Redis configured."
	}
	if err := t.r.st.UpsertQuota(t.ctx, t.tenant.ID, t.model.ID, render.MaxLimit, testWindow, nil); err != nil {
		return Failed, err.Error()
	}
	if _, err := t.r.st.SetTenantBudget(t.ctx, t.tenant.ID, &store.TenantBudget{Credits: 1, Window: testWindow}); err != nil {
		return Failed, err.Error()
	}
	res, err := t.sync("Self-test of " + t.cluster.Name + ": gave " + t.tenant.Slug + " a budget of 1 credit across models")
	if err != nil {
		return Failed, "The cluster refused the objects: " + err.Error()
	}
	for _, r := range res.Rejected {
		switch {
		case r.Kind != "BackendTrafficPolicy":
		case r.Gateway == "Overridden":
			return Failed, "A policy on a route replaces the budget policy there, so the budget charges nothing on that route: " + r.GatewayMessage + ". Give that policy mergeType: StrategicMerge."
		default:
			return Failed, "The gateway's controller does not accept the budget policy: " + describe(res.Rejected) + ". Global rate limiting must be enabled in the Envoy Gateway configuration of this cluster (rateLimit.backend), with the Redis the gateways share."
		}
	}

	// The first answered request is charged once its answer is complete.
	// Until the policy is in place the quota of 1 still refuses, so a 429
	// here means waiting, not failure.
	start := time.Now()
	var first gateway.ChatResult
	var last string
	for first.Status != http.StatusOK && time.Since(start) < budgetWait {
		first, err = t.noted(gateway.ChatLong(t.ctx, t.cluster.GatewayURL, t.key, t.model.Name))
		if err != nil {
			last = err.Error()
		} else if first.Status != http.StatusOK {
			last = answer(first)
		}
		if first.Status != http.StatusOK && !t.wait() {
			break
		}
	}
	if first.Status != http.StatusOK {
		return Failed, fmt.Sprintf("No long request was answered within %d seconds after the quota was raised. Last: %s.", int(budgetWait.Seconds()), last)
	}
	want, known := expectedCharge(first, t.model.Cost())
	if known && want == 0 {
		return Skipped, fmt.Sprintf("A request of %d tokens costs less than a credit at the model's prices, so a budget cannot be seen to move. Run the self-test on a model with higher prices.", first.Tokens)
	}

	// Counted: the policy may have reached the proxy after the first answer,
	// in which case that answer was not charged; one more request tells.
	again := false
	var used int64
	for time.Since(start) < budgetWait {
		rep, err := t.r.usage.Report(t.ctx, t.tenant.ID)
		if err != nil {
			return Failed, "Redis: " + err.Error()
		}
		for _, b := range rep.Budgets {
			used = max(used, b.Used)
		}
		if used > 0 {
			break
		}
		if !again && time.Since(start) > counterWait {
			again = true
			if res, err := t.noted(gateway.ChatLong(t.ctx, t.cluster.GatewayURL, t.key, t.model.Name)); err == nil && res.Status == http.StatusOK {
				first = res
				want, known = expectedCharge(first, t.model.Cost())
			}
		}
		if !t.wait() {
			break
		}
	}
	if used == 0 {
		return Failed, "The request was answered but nothing was counted under the tenant's budget where the Usage page reads it: the route does not write the cost in credits, a policy on the route replaces the budget policy (Overridden in its status), or the rate limit service does not reach the shared Redis."
	}
	counted := fmt.Sprintf("%d credits counted for one request", used)
	if known {
		counted += fmt.Sprintf(" that costs %d by the hub's reckoning", want)
	}

	// Refused: the budget of 1 is spent.
	for time.Since(start) < budgetWait {
		res, err := t.chat(t.key)
		switch {
		case err != nil:
			last = err.Error()
		case res.Status == http.StatusTooManyRequests:
			return Passed, counted + "; the next request was refused with HTTP 429, with the model's own quota raised so that only the budget could refuse."
		default:
			last = answer(res)
		}
		if !t.wait() {
			break
		}
	}
	return Failed, counted + ", yet requests are still answered (last: " + last + "): the rate limit rule charges nothing, or the gateway does not hold the tenant to it."
}

// models holds the temporary tenant first to the model under test, which
// it must still be served, then to no model at all, so that its next
// request is refused with HTTP 403 before any model or quota is asked. The
// first proves the rule with a list matches the model header; the second
// that the rule refuses. The refusal comes from the authorization rule on
// the tenant header and the model header the gateway sets, so the budget
// of the step before is taken away first, and the quota raised, so that
// neither can refuse instead.
func (t *test) models() (string, string) {
	switch {
	case t.key == "":
		return Skipped, "Without API keys the gateway cannot tell tenants apart, so a tenant cannot be held to its models."
	case !t.budgetTokensOnly():
		return Skipped, "A tenant's list of models needs the gateways on tokens only: its rule reads the tenant header a token sets."
	}
	if _, err := t.r.st.SetTenantBudget(t.ctx, t.tenant.ID, nil); err != nil {
		return Failed, err.Error()
	}
	if err := t.r.st.UpsertQuota(t.ctx, t.tenant.ID, t.model.ID, render.MaxLimit, testWindow, nil); err != nil {
		return Failed, err.Error()
	}
	if _, err := t.r.st.SetTenantModels(t.ctx, t.tenant.ID, []string{t.model.ID}); err != nil {
		return Failed, err.Error()
	}
	if _, err := t.sync("Self-test of " + t.cluster.Name + ": held " + t.tenant.Slug + " to " + t.model.Name); err != nil {
		return Failed, "The cluster refused the objects: " + err.Error()
	}
	start := time.Now()
	var last string
	served := false
	for !served && time.Since(start) < keyWait {
		res, err := t.chat(t.key)
		switch {
		case err != nil:
			last = err.Error()
		case res.Status == http.StatusOK:
			served = true
			continue
		default:
			last = answer(res)
		}
		if !t.wait() {
			break
		}
	}
	if !served {
		return Failed, fmt.Sprintf("Held to %s alone, the tenant was not served that model within %d seconds (last: %s): the rule with a list refuses the model it lists.", t.model.Name, int(keyWait.Seconds()), last)
	}

	if _, err := t.r.st.SetTenantModels(t.ctx, t.tenant.ID, []string{}); err != nil {
		return Failed, err.Error()
	}
	if _, err := t.sync("Self-test of " + t.cluster.Name + ": held " + t.tenant.Slug + " to no model"); err != nil {
		return Failed, "The cluster refused the objects: " + err.Error()
	}
	start = time.Now()
	for time.Since(start) < keyWait {
		res, err := t.chat(t.key)
		switch {
		case err != nil:
			last = err.Error()
		case res.Status == http.StatusForbidden:
			return Passed, fmt.Sprintf("Held to %s alone, the tenant was served it; held to no model, it was refused with HTTP 403 %d seconds after the sync.", t.model.Name, int(time.Since(start).Seconds()))
		default:
			last = answer(res)
		}
		if !t.wait() {
			break
		}
	}
	return Failed, fmt.Sprintf("The tenant was still not refused for the model after %d seconds (last: %s): the rule does not match, or the gateway sets the model header after it checks authorization.", int(keyWait.Seconds()), last)
}
