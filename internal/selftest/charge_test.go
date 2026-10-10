package selftest

import (
	"testing"

	"aigw-ui/internal/gateway"
)

func TestCharged(t *testing.T) {
	cached := int64(200)
	plain := gateway.ChatResult{Tokens: 301, PromptTokens: 300, CompletionTokens: 1}
	hit := gateway.ChatResult{Tokens: 301, PromptTokens: 300, CompletionTokens: 1, CachedTokens: &cached}
	const weighted = "(cached_input_tokens <= input_tokens ? 10u * (input_tokens - cached_input_tokens) + cached_input_tokens : 10u * input_tokens) + 40u * output_tokens"
	for name, tc := range map[string]struct {
		counted int64
		res     gateway.ChatResult
		cost    string
		want    string
	}{
		"exactly the tokens":               {301, plain, "", Passed},
		"one more, the check itself":       {302, plain, "", Passed},
		"only the check":                   {1, plain, "", Failed},
		"twice":                            {602, plain, "", Failed},
		"three times, both sites":          {903, plain, "", Failed},
		"a little more":                    {310, plain, "", Warning},
		"no usage in the answer":           {24, gateway.ChatResult{}, "", Passed},
		"an expression, nothing cached":    {3041, plain, weighted, Passed},
		"an expression, cached part":       {1240, hit, weighted, Passed},
		"an expression the hub cannot run": {5, plain, "tokens", Passed},
	} {
		if got, detail := charged(tc.counted, tc.res, tc.cost, "fleet-m"); got != tc.want {
			t.Errorf("%s: %s (%s), want %s", name, got, detail, tc.want)
		}
	}
}

func TestCachedReport(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	for name, tc := range map[string]struct {
		cached *int64
		want   string
	}{
		"not reported":         {nil, Warning},
		"reported, no hit":     {n(0), Warning},
		"a hit":                {n(288), Passed},
		"more than the prompt": {n(9999), Failed},
		"negative":             {n(-5), Failed},
	} {
		if got, detail := cachedReport(gateway.ChatResult{PromptTokens: 300, CachedTokens: tc.cached}); got != tc.want {
			t.Errorf("%s: %s (%s), want %s", name, got, detail, tc.want)
		}
	}
}
