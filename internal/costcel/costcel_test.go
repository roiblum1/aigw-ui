package costcel

import "testing"

// clamped charges cached prompt tokens a tenth and output four times, and
// bills a cached count above the prompt as not cached at all.
const clamped = "(cached_input_tokens <= input_tokens ? 10u * (input_tokens - cached_input_tokens) + cached_input_tokens : 10u * input_tokens) + 40u * output_tokens"

func TestCheck(t *testing.T) {
	for _, ok := range []string{
		"total_tokens", "input_tokens + output_tokens * 4u", clamped,
		"uint(double(cached_input_tokens) * 0.1) + output_tokens",
		`model == "x" ? total_tokens : 2u * total_tokens`,
	} {
		if err := Check(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for name, bad := range map[string]string{
		"unknown name":       "tokens",
		"uint times int":     "input_tokens * 10",
		"uint times double":  "input_tokens * 0.5",
		"double result":      "double(input_tokens) * 0.5",
		"negative result":    "-1",
		"division by count":  "100u / total_tokens",
		"not an expression":  "input_tokens; drop",
		"unmatched bracket":  "(input_tokens",
		"text result":        "model",
		"unknown function":   "math.ceil(1.5)",
		"capital in a name":  "Input_tokens",
		"nothing to compute": "",
	} {
		if err := Check(bad); err == nil {
			t.Errorf("%s: %q should be refused", name, bad)
		}
	}
}

func TestEval(t *testing.T) {
	for name, tc := range map[string]struct {
		expr string
		u    Usage
		want uint64
	}{
		"a cached prompt costs a tenth":       {clamped, Usage{Input: 1000, CachedInput: 800, Output: 200}, 10800},
		"nothing cached":                      {clamped, Usage{Input: 1000, Output: 200}, 18000},
		"more cached than sent is full price": {clamped, Usage{Input: 100, CachedInput: 9999, Output: 200}, 9000},
		"division rounds down":                {"total_tokens / 2u", Usage{Total: 7}, 3},
	} {
		got, err := Eval(tc.expr, tc.u)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %d, %v, want %d", name, got, err, tc.want)
		}
	}
	// Without the guard a forged count is an error at the gateway, and the
	// request is then not charged.
	if _, err := Eval("input_tokens - cached_input_tokens", Usage{Input: 1, CachedInput: 2}); err == nil {
		t.Error("an unguarded subtraction below zero should fail")
	}
}
