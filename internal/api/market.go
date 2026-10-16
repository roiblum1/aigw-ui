package api

import (
	"math"
	"net/http"
	"strings"

	"aigw-ui/internal/market"
	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// How a model's market entry was found.
const (
	marketBySet        = "set"        // chosen in the Prices dialog
	marketByAnnotation = "annotation" // aigw-ui.io/market-id on its deployments
	marketByName       = "name"       // a served name is the entry's Hugging Face id
)

// What a model's prices are next to the market's.
const (
	marketNoPrices = "no-prices" // the model is counted in tokens
	marketInUse    = "in-use"    // its prices are the market's
	marketMoved    = "moved"     // they were taken from the market, which has changed since
	marketCustom   = "custom"    // they were set by hand
)

// marketInfo is the market price of a model, as the Prices dialog shows it.
type marketInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	HFID     string `json:"hf_id"`
	Source   string `json:"source"`
	Snapshot string `json:"snapshot_date"`
	// Dollars for a million tokens, as the market has them.
	Prompt     float64  `json:"prompt"`
	Completion float64  `json:"completion"`
	CacheRead  *float64 `json:"cache_read"`
	// Prices are what Use market prices sets, in credits for a million
	// tokens; nil when the market's price cannot be used, such as a free
	// prompt.
	Prices *marketPrices `json:"prices"`
	State  string        `json:"state"`
}

type marketPrices struct {
	Input  int64 `json:"price_input"`
	Cached int64 `json:"price_cached"`
	Output int64 `json:"price_output"`
}

// modelOut is a model as GET /models returns it: the stored model with its
// market price.
type modelOut struct {
	store.Model
	// Rules is how many bucket rules the model's QuotaPolicy has, and
	// RulesPerTenant how many the tenants' own quotas would take with a rule
	// each, which they do while the gateways take API keys too.
	Rules          int         `json:"rules"`
	RulesPerTenant int         `json:"rules_per_tenant"`
	Market         *marketInfo `json:"market"`
	// MarketNote says why a model has no market price when it is not for
	// want of a match, such as an annotation the snapshot does not have.
	MarketNote string `json:"market_note"`
}

// marketEntry finds a model's entry in the snapshot: the one chosen by hand,
// else the one its deployments' annotation names, else the one whose Hugging
// Face id is a name the model is served under.
func marketEntry(m store.Model, snap *market.Snapshot) (e market.Entry, source, note string, ok bool) {
	missing := func(id string) string {
		return "the market snapshot of " + snap.Date + " has no " + id
	}
	switch {
	case m.MarketIDSet != nil:
		if *m.MarketIDSet == "" {
			return e, marketBySet, "", false
		}
		if e, ok = snap.Find(*m.MarketIDSet); !ok {
			return e, marketBySet, missing(*m.MarketIDSet), false
		}
		return e, marketBySet, "", true
	case len(m.MarketAnnotations) > 1:
		return e, marketByAnnotation, "its deployments name different market entries in " + kubeMarketAnnotation + ": " +
			strings.Join(m.MarketAnnotations, ", "), false
	case len(m.MarketAnnotations) == 1:
		if e, ok = snap.Find(m.MarketAnnotations[0]); !ok {
			return e, marketByAnnotation, missing(m.MarketAnnotations[0]), false
		}
		return e, marketByAnnotation, "", true
	}
	e, ok = snap.Match(servedNames(m)...)
	return e, marketByName, "", ok
}

// kubeMarketAnnotation is kube.MarketIDAnnotation, named here so that the
// API does not depend on the Kubernetes client.
const kubeMarketAnnotation = "aigw-ui.io/market-id"

// servedNames are the names requests for a model carry on some cluster.
func servedNames(m store.Model) []string {
	names := []string{m.Name}
	for _, e := range m.Endpoints {
		names = append(names, e.UpstreamModel)
		for _, b := range e.Backends {
			names = append(names, b.Model)
		}
	}
	return names
}

// creditsOf turns market prices into the prices a model is given.
func creditsOf(p market.Prices) marketPrices {
	c := func(dollars float64) int64 { return int64(math.Round(dollars * render.CreditsPerDollar)) }
	out := marketPrices{Input: max(c(p.Input), 1), Cached: c(p.Cached), Output: c(p.Output)}
	out.Cached = min(out.Cached, out.Input)
	return out
}

// withMarket adds its market price to a model.
func withMarket(m store.Model, snap *market.Snapshot) modelOut {
	out := modelOut{Model: m}
	e, source, note, ok := marketEntry(m, snap)
	out.MarketNote = note
	if !ok {
		return out
	}
	info := &marketInfo{ID: e.ID, Name: e.Name, HFID: e.HFID, Source: source, Snapshot: snap.Date,
		Prompt: e.Prompt, Completion: e.Completion, CacheRead: e.CacheRead, State: marketNoPrices}
	if p, usable := e.Prices(); usable {
		c := creditsOf(p)
		info.Prices = &c
	}
	latest := m.PendingPrices
	if latest == nil {
		latest = m.Prices
	}
	switch {
	case latest == nil:
	case info.Prices != nil && sameAsMarket(*latest, *info.Prices):
		info.State = marketInUse
	case latest.Market != "":
		info.State = marketMoved
	default:
		info.State = marketCustom
	}
	out.Market = info
	return out
}

func sameAsMarket(p store.Price, m marketPrices) bool {
	return p.Input == m.Input && p.Cached == m.Cached && p.Output == m.Output
}

// marketRef is what a set of prices records when it is the market's
// unchanged: "<entry>@<snapshot date>". "" otherwise.
func marketRef(m store.Model, p store.Price, snap *market.Snapshot) string {
	out := withMarket(m, snap)
	if out.Market == nil || out.Market.Prices == nil || !sameAsMarket(p, *out.Market.Prices) {
		return ""
	}
	return out.Market.ID + "@" + snap.Date
}

// searchMarket lists the snapshot's entries that match q, for choosing a
// model's entry by hand.
func (s *Server) searchMarket(w http.ResponseWriter, r *http.Request) {
	snap := market.Current()
	writeJSON(w, http.StatusOK, map[string]any{
		"source":        snap.Source,
		"snapshot_date": snap.Date,
		"models":        snap.Search(r.URL.Query().Get("q"), 25),
	})
}

// setMarketID chooses a model's market entry. A null id goes back to the
// annotation and the automatic match, "" means no market price. It changes
// no price.
func (s *Server) setMarketID(w http.ResponseWriter, r *http.Request) {
	var b struct {
		MarketID *string `json:"market_id"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	snap := market.Current()
	if b.MarketID != nil {
		id := strings.TrimSpace(*b.MarketID)
		if _, ok := snap.Find(id); id != "" && !ok {
			fail(w, invalid("the market snapshot of %s has no %q", snap.Date, id))
			return
		}
		b.MarketID = &id
	}
	name, err := s.st.SetMarketID(r.Context(), r.PathValue("id"), b.MarketID)
	if err != nil {
		fail(w, err)
		return
	}
	switch {
	case b.MarketID == nil:
		note(r, "model.market", "The market entry of "+name+" follows its annotation and served names again")
	case *b.MarketID == "":
		note(r, "model.market", name+" has no market price")
	default:
		note(r, "model.market", "Set the market entry of "+name+" to "+*b.MarketID)
	}
	w.WriteHeader(http.StatusNoContent)
}
