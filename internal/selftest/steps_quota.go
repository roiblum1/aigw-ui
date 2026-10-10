package selftest

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// counter looks for the tokens of the first request in Redis, under the name
// the Usage page computes.
func (t *test) counter() (string, string) {
	switch {
	case t.key == "":
		return Skipped, "Without API keys the gateway cannot tell tenants apart, so there is no tenant counter."
	case t.r.usage == nil:
		return Skipped, "Usage monitoring is off: the server has no Redis configured."
	}
	start := time.Now()
	hint := ""
	again := false
	for time.Since(start) < counterWait {
		rep, err := t.r.usage.Report(t.ctx, t.tenant.ID)
		if err != nil {
			return Failed, "Redis: " + err.Error()
		}
		hint = rep.Hint
		for _, q := range rep.Quotas {
			if q.ModelID != t.model.ID || q.Used == 0 {
				continue
			}
			t.counted = true
			var where []string
			for _, c := range q.Counters {
				if c.Used > 0 {
					where = append(where, c.Backend)
				}
			}
			return charged(q.Used, t.first, t.model.Cost(), strings.Join(where, ", "))
		}
		// The key can reach the gateway before the tenant's rule reaches
		// the rate limit service. The first request is then answered and
		// counted nowhere under the tenant's name. Counting is done when
		// the answer is, so a counter that is not there by now will not
		// come: one more request tells a late rule from a wrong name.
		if !again && time.Since(start) > counterWait/2 {
			again = true
			if res, err := t.chat(t.key); err == nil && res.Status == http.StatusOK {
				t.first = res
			}
		}
		if !t.wait() {
			break
		}
	}
	if hint == "" {
		hint = "No counter appeared under the expected name."
	}
	return Failed, hint
}

// limit expects a refusal: the first request used up the tenant's quota.
func (t *test) limit() (string, string) {
	if t.key == "" {
		return Skipped, "Without API keys the gateway cannot tell tenants apart, so a tenant quota cannot be tested."
	}
	if t.model.PriceDryRun {
		return Skipped, "The model's prices are in dry-run: every tenant is counted and nobody is refused."
	}
	if t.bestEffort() {
		return Skipped, "The model serves a tenant past its budget as best-effort and does not refuse it. The next step tests that."
	}
	start := time.Now()
	var last string
	for time.Since(start) < limitWait {
		res, err := t.chat(t.key)
		switch {
		case err != nil:
			last = err.Error()
		case res.Status == http.StatusTooManyRequests:
			t.refused = true
			return Passed, "Refused with HTTP 429 once the quota of 1 was used."
		case res.Status == http.StatusOK:
			last = answer(res)
			// In Shared mode a tenant over its own quota goes on while the
			// model's pool has tokens, so an answer is only wrong when the
			// pool is empty too. There is no point in spending more tokens.
			if left, known := t.poolLeft(); known && left > 0 {
				return Warning, fmt.Sprintf("Not refused, as expected here: the model's pool for tenants over their quota still has %d tokens, and the tenant draws from it. To test a refusal, run the self-test on a model with a small pool.", left)
			}
		default:
			last = answer(res)
		}
		if !t.wait() {
			break
		}
	}
	if !t.counted && t.r.usage != nil {
		return Failed, "Requests over the quota are still answered, and no counter was found either: the tenant's rule most likely does not match its requests. Last: " + last + "."
	}
	return Failed, fmt.Sprintf("Requests over the quota were still answered after %d seconds. Last: %s.", int(limitWait.Seconds()), last)
}

// poolLeft returns the tokens left in the model's pool. It is only known
// when the counters can be read and were found where they are expected.
func (t *test) poolLeft() (int64, bool) {
	if t.r.usage == nil || !t.counted {
		return 0, false
	}
	rep, err := t.r.usage.Report(t.ctx, "")
	if err != nil {
		return 0, false
	}
	for _, p := range rep.Pools {
		if p.ModelID == t.model.ID {
			return p.Limit - p.Used, true
		}
	}
	return 0, false
}

func (t *test) reset() (string, string) {
	switch {
	case t.bestEffort():
		return Skipped, "The model does not refuse a tenant past its budget, so there is no refusal to lift."
	case !t.refused:
		return Skipped, "Needs a refused request first."
	case t.r.usage == nil:
		return Skipped, "Usage monitoring is off: the server has no Redis configured."
	case !t.r.usage.CanReset():
		return Skipped, "Resetting usage is turned off (redis.allowReset)."
	}
	res, err := t.r.usage.Reset(t.ctx, t.tenant.ID, t.model.ID)
	if err != nil {
		return Failed, "Reset: " + err.Error()
	}
	if res.Deleted == 0 {
		return Failed, "The reset found no counter to delete, although the tenant was refused."
	}
	start := time.Now()
	var last string
	for time.Since(start) < resetWait {
		chat, err := t.chat(t.key)
		switch {
		case err != nil:
			last = err.Error()
		case chat.Status == http.StatusOK:
			return Passed, "After the counter was deleted the next request was answered."
		default:
			last = answer(chat)
		}
		if !t.wait() {
			break
		}
	}
	return Failed, "The counter was deleted but the tenant is still refused (" + last + "). The rate limit service remembers over-limit tenants in its own cache when LOCAL_CACHE_SIZE_IN_BYTES is set; a reset then only works once the window ends."
}
