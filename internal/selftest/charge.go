package selftest

import (
	"fmt"

	"aigw-ui/internal/costcel"
	"aigw-ui/internal/gateway"
)

// expectedCharge returns what the gateway charges for an answered request:
// its total tokens, or the model's cost expression over its token counts. It
// reports false when the answer carries no usage or the expression fails.
func expectedCharge(res gateway.ChatResult, costExpression string) (int64, bool) {
	if res.Tokens <= 0 {
		return 0, false
	}
	if costExpression == "" {
		return res.Tokens, true
	}
	u := costcel.Usage{Input: uint32(res.PromptTokens), Output: uint32(res.CompletionTokens), Total: uint32(res.Tokens)}
	if res.CachedTokens != nil && *res.CachedTokens > 0 {
		u.CachedInput = uint32(*res.CachedTokens)
	}
	cost, err := costcel.Eval(costExpression, u)
	if err != nil {
		return 0, false
	}
	return int64(cost), true
}

// charged judges the tenant's counter after one answered request. where
// names the backends the counter was found on.
//
// The gateway counts one more than the cost: the check before the request is
// itself a hit of 1.
func charged(counted int64, res gateway.ChatResult, costExpression, where string) (string, string) {
	want, known := expectedCharge(res, costExpression)
	found := fmt.Sprintf("%d counted for the tenant on %s", counted, where)
	switch {
	case !known:
		return Passed, found + ". The answer carries no usage to compare it with. The counter names the Usage page computes are right."
	case counted == want || counted == want+1:
		return Passed, fmt.Sprintf("%s, for one request that costs %d. The counter names the Usage page computes are right.", found, want)
	case counted <= 1:
		return Failed, fmt.Sprintf("%s, for one request that costs %d. The request is counted and its cost is not: "+
			"the gateway did not hand the computed cost to the rate limit service, so a quota is never used up.", found, want)
	case counted >= 2*want:
		return Failed, fmt.Sprintf("%s, for one request that costs %d. The request is charged more than once: "+
			"the serving site's own route for the model is still attached to the client listener or to the whole Gateway, so its backend keeps a quota. "+
			"Attach that route to the peer listener alone.", found, want)
	default:
		return Warning, fmt.Sprintf("%s, for one request that costs %d by the hub's reckoning. The two should agree to within 1.", found, want)
	}
}
