package api

import (
	"strings"
	"testing"

	"aigw-ui/internal/market"
	"aigw-ui/internal/store"
)

func snapshot(t *testing.T, date, glmPrompt string) *market.Snapshot {
	t.Helper()
	s, err := market.Parse([]byte(`{"source": "OpenRouter", "snapshot_date": "` + date + `", "models": [
{"id": "z-ai/glm-5.3", "hf_id": "zai-org/GLM-5.3", "name": "Z.ai: GLM 5.3", "context": 1048576, "prompt": ` + glmPrompt + `, "completion": 8.8, "cache_read": 0.56},
{"id": "z-ai/glm-5.3:batch", "hf_id": "zai-org/GLM-5.3", "name": "Z.ai: GLM 5.3 (batch)", "context": 1048576, "prompt": 1.4, "completion": 4.4, "cache_read": null},
{"id": "moonshotai/kimi-k2.7-code", "hf_id": "moonshotai/Kimi-K2.7-Code", "name": "Kimi K2.7 Code", "context": 262144, "prompt": 0.6712, "completion": 3.35, "cache_read": null}
]}`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func str(s string) *string { return &s }

// A model finds its entry by hand, by annotation, or by a served name, in
// that order.
func TestMarketEntry(t *testing.T) {
	snap := snapshot(t, "2026-10-12", "2.8")
	served := store.Model{Name: "glm", Endpoints: []store.Endpoint{{UpstreamModel: "zai-org/GLM-5.3"}}}
	if e, source, _, ok := marketEntry(served, snap); !ok || e.ID != "z-ai/glm-5.3" || source != marketByName {
		t.Fatalf("by name: %s %s %v", e.ID, source, ok)
	}
	byBackend := store.Model{Name: "glm", Endpoints: []store.Endpoint{{Backends: []store.BackendRef{{Model: "zai-org/glm-5.3"}}}}}
	if e, _, _, ok := marketEntry(byBackend, snap); !ok || e.ID != "z-ai/glm-5.3" {
		t.Fatalf("by backend: %s %v", e.ID, ok)
	}
	annotated := store.Model{Name: "kimi", MarketAnnotations: []string{"moonshotai/kimi-k2.7-code"}}
	if e, source, _, ok := marketEntry(annotated, snap); !ok || e.ID != "moonshotai/kimi-k2.7-code" || source != marketByAnnotation {
		t.Fatalf("by annotation: %s %s %v", e.ID, source, ok)
	}
	// A choice made by hand comes first, even over a matching name; "" is
	// no market price.
	served.MarketIDSet = str("z-ai/glm-5.3:batch")
	if e, source, _, ok := marketEntry(served, snap); !ok || e.ID != "z-ai/glm-5.3:batch" || source != marketBySet {
		t.Fatalf("set: %s %s %v", e.ID, source, ok)
	}
	served.MarketIDSet = str("")
	if _, _, note, ok := marketEntry(served, snap); ok || note != "" {
		t.Fatalf("none: %v %q", ok, note)
	}
	// What cannot be found says why.
	if _, _, note, ok := marketEntry(store.Model{MarketAnnotations: []string{"gone/model"}}, snap); ok || !strings.Contains(note, "has no gone/model") {
		t.Fatalf("missing: %q", note)
	}
	if _, _, note, ok := marketEntry(store.Model{MarketAnnotations: []string{"a/b", "c/d"}}, snap); ok || !strings.Contains(note, "a/b, c/d") {
		t.Fatalf("disagree: %q", note)
	}
	if _, _, note, ok := marketEntry(store.Model{Name: "qwen-tiny"}, snap); ok || note != "" {
		t.Fatalf("no match: %q", note)
	}
}

// The market's prices in credits: $2.80 is 280,000 credits for a million
// tokens; without a cached price on the market, a tenth of the input price.
func TestMarketPrices(t *testing.T) {
	snap := snapshot(t, "2026-10-12", "2.8")
	glm := withMarket(store.Model{Name: "zai-org/GLM-5.3"}, snap)
	if glm.Market == nil || *glm.Market.Prices != (marketPrices{Input: 280000, Cached: 56000, Output: 880000}) || glm.Market.State != marketNoPrices {
		t.Fatalf("glm: %+v", glm.Market)
	}
	kimi := withMarket(store.Model{MarketAnnotations: []string{"moonshotai/kimi-k2.7-code"}}, snap)
	if *kimi.Market.Prices != (marketPrices{Input: 67120, Cached: 6712, Output: 335000}) {
		t.Fatalf("kimi: %+v", kimi.Market.Prices)
	}
}

// Prices taken from the market record it; a later snapshot with another
// price changes no price, and the page says the market moved.
func TestMarketMoved(t *testing.T) {
	before := snapshot(t, "2026-10-12", "2.8")
	m := store.Model{Name: "zai-org/GLM-5.3"}
	p := store.Price{Input: 280000, Cached: 56000, Output: 880000}
	if p.Market = marketRef(m, p, before); p.Market != "z-ai/glm-5.3@2026-10-12" {
		t.Fatalf("ref = %q", p.Market)
	}
	m.Prices = &p
	if got := withMarket(m, before).Market.State; got != marketInUse {
		t.Fatalf("before: %s", got)
	}

	after := snapshot(t, "2026-10-19", "3.1")
	out := withMarket(m, after)
	if out.Market.State != marketMoved || out.Prices.Input != 280000 || out.Market.Prices.Input != 310000 {
		t.Fatalf("after: %s, prices %+v, market %+v", out.Market.State, out.Prices, out.Market.Prices)
	}

	// Prices set by hand are custom, and record no market.
	hand := store.Price{Input: 300000, Cached: 30000, Output: 900000}
	if marketRef(m, hand, after) != "" {
		t.Fatal("hand prices recorded a market")
	}
	m.Prices = &hand
	if got := withMarket(m, after).Market.State; got != marketCustom {
		t.Fatalf("hand: %s", got)
	}
	// Waiting prices count over the ones in use.
	m.PendingPrices = &store.Price{Input: 310000, Cached: 56000, Output: 880000}
	if got := withMarket(m, after).Market.State; got != marketInUse {
		t.Fatalf("pending: %s", got)
	}
}
