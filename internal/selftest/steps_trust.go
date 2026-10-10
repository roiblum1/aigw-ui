package selftest

import (
	"fmt"
	"net/http"
	"time"

	"aigw-ui/internal/gateway"
	"aigw-ui/internal/render"
)

// foreignClientID is a client ID no tenant has: a tenant's slug never
// contains an underscore.
const foreignClientID = "not_a_tenant.00000000"

// clientID sends a request with the temporary tenant's key and another
// client ID in the header the gateway writes after it has checked the key.
// The request has to be counted for the key's tenant all the same. If the
// gateway kept the client's header, any tenant could spend another's budget.
func (t *test) clientID() (string, string) {
	switch {
	case t.key == "":
		return Skipped, "\"Enforce API keys\" is off for this cluster, so the gateway does not tell tenants apart."
	case t.r.usage == nil:
		return Skipped, "Usage monitoring is off: the server has no Redis configured."
	case !t.counted:
		return Skipped, "Needs a counted request first."
	}
	if !t.bestEffort() {
		// The tenant's quota is spent, so the request would be refused.
		if !t.r.usage.CanReset() {
			return Skipped, "The tenant's quota is spent and resetting usage is turned off (redis.allowReset)."
		}
		if _, err := t.r.usage.Reset(t.ctx, t.tenant.ID, t.model.ID); err != nil {
			return Failed, "Reset: " + err.Error()
		}
	}
	before, err := t.quota()
	if err != nil {
		return Failed, "Redis: " + err.Error()
	}
	start := time.Now()
	last := "no answer"
	sent := false
	for time.Since(start) < resetWait+counterWait {
		if !sent {
			res, err := t.chatWith(map[string]string{render.ClientIDHeader: foreignClientID})
			switch {
			case err != nil:
				last = err.Error()
			case res.Status == http.StatusOK:
				sent = true
			default:
				last = answer(res)
			}
		}
		if sent {
			now, err := t.quota()
			if err != nil {
				return Failed, "Redis: " + err.Error()
			}
			if now.Used+now.OverageUsed > before.Used+before.OverageUsed {
				return Passed, "A request that named another client ID in " + render.ClientIDHeader + " was counted for the tenant of its key. The gateway replaces the header."
			}
		}
		if !t.wait() {
			break
		}
	}
	if !sent {
		return Failed, "The request was not answered (" + last + "), so nothing can be said."
	}
	return Failed, "A request with the tenant's key and another client ID in " + render.ClientIDHeader + " was answered and not counted for the tenant. " +
		"The gateway trusts the header a client sends, so a client can use up another tenant's budget."
}

// cachedTokens looks at the latest answer of the run. Every request has the
// same prompt, so by now the model has seen it before.
func (t *test) cachedTokens() (string, string) {
	if t.answered < 2 {
		return Skipped, "Needs two answered requests with the same prompt, and this run had fewer."
	}
	return cachedReport(t.last)
}

// cachedReport judges how an answer to a repeated prompt reports the part
// taken from the prefix cache.
func cachedReport(res gateway.ChatResult) (string, string) {
	switch {
	case res.CachedTokens == nil:
		return Warning, "The answer does not say how much of the prompt came from the prefix cache (usage.prompt_tokens_details.cached_tokens). " +
			"A lower price for cached prompts then never applies. vLLM reports it when started with --enable-prompt-tokens-details."
	case *res.CachedTokens > res.PromptTokens || *res.CachedTokens < 0:
		return Failed, fmt.Sprintf("The answer reports %d cached tokens for a prompt of %d. The count cannot be trusted for a price.", *res.CachedTokens, res.PromptTokens)
	case *res.CachedTokens == 0:
		return Warning, fmt.Sprintf("The model reports cached tokens, and none of the %d prompt tokens of a repeated prompt came from the cache. "+
			"Another replica may have served it, or prefix caching is off.", res.PromptTokens)
	default:
		return Passed, fmt.Sprintf("%d of the %d prompt tokens of a repeated prompt came from the prefix cache.", *res.CachedTokens, res.PromptTokens)
	}
}
