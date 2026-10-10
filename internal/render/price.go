package render

import (
	"fmt"
	"strings"
)

// CreditsPerDollar is the unit a priced model is counted in: one credit is
// a hundred-thousandth of a dollar. It is small enough that rounding a
// request down loses next to nothing, and large enough that a model's pool
// for a day fits the rate limit service's counter, which ends at
// serviceQuotaLimit: about 42,949 dollars.
const CreditsPerDollar = 100_000

// MaxLimit is the largest limit the gateway keeps: it is cast to 32 bits on
// the way to the rate limit service, and a larger one wraps around.
const MaxLimit = serviceQuotaLimit

// Prices are what a million tokens of a model cost, in credits.
type Prices struct {
	// Input is the price of the part of a prompt the model had to compute,
	// Cached of the part it took from its prefix cache.
	Input, Cached, Output int64
}

// Expression returns the cost expression for these prices: what one request
// is charged, in credits, rounded down.
//
// input_tokens includes the cached part, so that is taken out first. A
// cached count above the prompt cannot be true; the whole prompt is then
// charged as computed, and the subtraction is skipped, because below zero
// it is an error in CEL and the gateway charges nothing for a request whose
// expression fails. Reasoning tokens are part of output_tokens already.
func (p Prices) Expression() string {
	return fmt.Sprintf("((cached_input_tokens <= input_tokens ? input_tokens - cached_input_tokens : input_tokens) * %du"+
		" + (cached_input_tokens <= input_tokens ? cached_input_tokens : 0u) * %du"+
		" + output_tokens * %du) / 1000000u", p.Input, p.Cached, p.Output)
}

// Dollars writes an amount of credits as money, with as many decimals as it
// needs and at least two.
func Dollars(credits int64) string {
	s := fmt.Sprintf("%d.%05d", credits/CreditsPerDollar, credits%CreditsPerDollar)
	for strings.HasSuffix(s, "0") && len(s)-strings.IndexByte(s, '.') > 3 {
		s = s[:len(s)-1]
	}
	return "$" + s
}
