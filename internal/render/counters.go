package render

import (
	"strconv"
	"time"
)

// quotaDomain is the rate limit domain the gateway uses for every QuotaPolicy.
const quotaDomain = "ai-gateway-quota"

// Counter is where the gateway's rate limit service keeps the usage of one
// tenant quota on one backend. The names below follow what the gateway
// generates from a QuotaPolicy (internal/ratelimit/translator in its source)
// and how the rate limit service turns a descriptor into a Redis key.
type Counter struct {
	ModelSlug  string
	TenantSlug string
	// Backend is "<namespace>/<AIServiceBackend name>".
	Backend string
	Limit   int64
	Window  string
	Shadow  bool
	// stem is the Redis key without the key prefix and the window start.
	stem string
}

// Counters lists the counter of every tenant quota the state renders.
func Counters(s State) []Counter {
	var out []Counter
	for _, m := range s.Models {
		rules := tenantRules(m)
		seen := map[string]bool{}
		for _, t := range m.targets() {
			ns := t.Namespace
			if ns == "" {
				ns = s.Namespace
			}
			backend := ns + "/" + t.Backend
			if seen[backend+"\x00"+t.Model] {
				continue
			}
			seen[backend+"\x00"+t.Model] = true
			for i, q := range rules {
				// The gateway names the descriptor of a rule after its position,
				// header and pattern, and sends the same string as its value.
				rule := "rule-" + strconv.Itoa(i) + "-" + ClientIDHeader + "|" + TenantClientIDPattern(q.TenantSlug) + "-match-0"
				out = append(out, Counter{
					ModelSlug: m.Slug, TenantSlug: q.TenantSlug, Backend: backend,
					Limit: q.Limit, Window: q.Window, Shadow: q.Shadow,
					stem: quotaDomain + "_backend_name_" + backend + "_model_name_override_" + t.Model + "_" + rule + "_" + rule + "_",
				})
			}
		}
	}
	return out
}

func windowSeconds(window string) int64 {
	switch window {
	case "1s":
		return 1
	case "1m":
		return 60
	case "1h":
		return 3600
	}
	return 86400
}

// WindowStart returns when the window that contains now began. Windows are
// aligned to the Unix epoch, so a day starts at 00:00 UTC.
func WindowStart(window string, now time.Time) time.Time {
	d := windowSeconds(window)
	return time.Unix(now.Unix()/d*d, 0).UTC()
}

// WindowEnd returns when the counter of the window that contains now resets.
func WindowEnd(window string, now time.Time) time.Time {
	return WindowStart(window, now).Add(time.Duration(windowSeconds(window)) * time.Second)
}

// RedisKey returns the key that holds the tokens used in the window that
// contains now. prefix is the rate limit service's CACHE_KEY_PREFIX.
func (c Counter) RedisKey(prefix string, now time.Time) string {
	return prefix + c.stem + strconv.FormatInt(WindowStart(c.Window, now).Unix(), 10)
}
