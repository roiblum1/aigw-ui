package selftest

import (
	"fmt"
	"time"

	"aigw-ui/internal/render"
)

// tokenCounts checks that the gateway counts the temporary tenant's
// requests and the tokens of their answers on the model, under the names
// the hub reads for the tenant's own page.
func (t *test) tokenCounts() (string, string) {
	if t.r.usage == nil {
		return Skipped, "Usage monitoring is off: the server has no Redis configured."
	}
	state, err := t.r.st.RenderStateWithoutKeys(t.ctx, t.cluster.ID)
	if err != nil {
		return Failed, err.Error()
	}
	if !state.CountsTokens() {
		return Skipped, "The gateway does not count tokens by tenant here: that needs tokens only and redis.tokenCounts on."
	}
	start := time.Now()
	var key string
	var use map[render.TokenKind]int64
	for time.Since(start) < counterWait {
		use, key, err = t.r.usage.TokenUse(t.ctx, t.cluster.Namespace, t.tenant.Slug, t.model.Name, time.Now())
		if err != nil {
			return Failed, "Redis: " + err.Error()
		}
		if use[render.TokenRequests] > 0 && use[render.TokenInput] > 0 {
			return Passed, fmt.Sprintf("%d requests, %d input tokens (%d of them cached) and %d output tokens counted for the tenant on %s.",
				use[render.TokenRequests], use[render.TokenInput], use[render.TokenCached], use[render.TokenOutput], t.model.Name)
		}
		if !t.wait() {
			break
		}
	}
	if use[render.TokenRequests] > 0 {
		return Failed, fmt.Sprintf("The requests were counted (%d), their tokens not: the model's route writes no token counts. "+
			"A route the hub neither renders nor marks needs them from the site's GatewayConfig (see the operations guide).", use[render.TokenRequests])
	}
	return Failed, "No token counter appeared under the expected name, for example " + key + "."
}
