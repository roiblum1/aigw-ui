package api

import (
	"testing"
	"time"
)

func TestPriceBody(t *testing.T) {
	now := time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)
	p, err := priceBody{Input: 0.11574, Cached: 0.011574, Output: 0.46296}.price(now)
	if err != nil {
		t.Fatal(err)
	}
	// Dollars for a million tokens become whole credits, rounded.
	if p.Input != 11574 || p.Cached != 1157 || p.Output != 46296 {
		t.Errorf("credits = %d, %d, %d", p.Input, p.Cached, p.Output)
	}
	if got := p.EffectiveAt.Format(time.RFC3339); got != "2026-10-11T00:00:00Z" {
		t.Errorf("starts at %s, want the next 00:00 UTC", got)
	}
	// Cached prompts free of charge, and a model with no output price.
	if _, err := (priceBody{Input: 1}).price(now); err != nil {
		t.Errorf("only an input price: %v", err)
	}
	for name, b := range map[string]priceBody{
		"no input price":           {Output: 1},
		"an input price below one": {Input: 0.000001},
		"negative":                 {Input: 1, Output: -1},
		"cached above input":       {Input: 1, Cached: 2},
		"typed in the wrong unit":  {Input: 115740},
	} {
		if _, err := b.price(now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
