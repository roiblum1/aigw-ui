// Package budget is the budget planner: how many dollars a day the fleet can
// still give, and which model runs out first. It turns what each
// LLMInferenceService was measured to serve and each model's prices into
// dollars a second, and adds them up over models and tenants. It is pure,
// like weights and capacity: it computes, and nothing in it changes a price
// or a budget.
//
// Amounts are credits (render.CreditsPerDollar to the dollar), the unit of a
// priced model. A model without prices has no dollars and is left out of
// every figure here.
package budget

import (
	"math"
	"sort"

	"aigw-ui/internal/capacity"
)

// SecondsPerDay and SecondsPerHour turn a rate into a day or an hour.
const (
	SecondsPerDay  = 86400
	SecondsPerHour = 3600
)

// Saturated is the share of what a model serves at the targets beyond which
// its busiest hour is marked saturated: the campaign's own threshold.
const Saturated = capacity.Saturated

// LLMI is one LLMInferenceService of a model.
type LLMI struct {
	// Ready is its ready instances now, on a site that is not drained.
	Ready float64
	// PerInstance is what one instance serves a second by the result that
	// counts for it, nil when none does: it then counts 0.
	PerInstance *capacity.Throughput
}

// Model is what the planner knows of one model.
type Model struct {
	ID string
	// Prices are credits for a million tokens; nil for a model in tokens.
	Prices *capacity.Prices
	LLMIs  []LLMI
	// BusyShare (b) and OvercommitLimit (k), as set on the Capacity page.
	BusyShare       float64
	OvercommitLimit float64
	// BusiestHour is the most spent on the model in one hour in the last
	// 30 days, best-effort and wallet spend included, in credits.
	BusiestHour int64
}

// ValuePerSecond is r_i: what one instance of an LLMI serves a second at
// the model's prices, in credits. Uncached prompt, cached prompt and output
// tokens, each at its price per million.
func ValuePerSecond(t capacity.Throughput, p capacity.Prices) float64 {
	return t.Value(&p)
}

// Plan is what one priced model can be given a day.
type Plan struct {
	ModelID string `json:"model_id"`
	// ValuePerSecond is R_m: every ready instance busy at the targets, in
	// credits a second.
	ValuePerSecond float64 `json:"value_per_second"`
	// CanBeGivenDay = 86,400 × busy share × R_m, and CeilingDay that times
	// the overcommit limit.
	CanBeGivenDay int64 `json:"can_be_given_day"`
	CeilingDay    int64 `json:"ceiling_day"`
	// PeakHourShare is ρ_m: the busiest hour's spend over what the model
	// serves in an hour; nil before it is measured.
	PeakHourShare *float64 `json:"peak_hour_share"`
	Saturated     bool     `json:"saturated"`
	// Values are r_i of the LLMIs, in their order, nil for one that is not
	// measured.
	Values []*float64 `json:"values"`
}

// PlanModel works out a priced model's plan. ok is false for a model in
// tokens, which has no dollars.
func PlanModel(m Model) (Plan, bool) {
	if m.Prices == nil {
		return Plan{}, false
	}
	p := Plan{ModelID: m.ID, Values: make([]*float64, len(m.LLMIs))}
	for i, l := range m.LLMIs {
		if l.PerInstance == nil {
			continue
		}
		r := ValuePerSecond(*l.PerInstance, *m.Prices)
		p.Values[i] = &r
		p.ValuePerSecond += l.Ready * r
	}
	p.CanBeGivenDay = round(SecondsPerDay * m.BusyShare * p.ValuePerSecond)
	p.CeilingDay = round(float64(p.CanBeGivenDay) * m.OvercommitLimit)
	if p.ValuePerSecond > 0 {
		rho := float64(m.BusiestHour) / (SecondsPerHour * p.ValuePerSecond)
		p.PeakHourShare, p.Saturated = &rho, rho > Saturated
	}
	return p, true
}

// Budget is a tenant's enforced budget on one model.
type Budget struct {
	ModelID string
	Limit   int64
	Window  string
}

// DayMax is the most a budget lets be spent in a day: one dollar an hour is
// 24.
func (b Budget) DayMax() int64 { return b.Limit * capacity.PerDay(b.Window) }

// Tenant is an enabled tenant: its wallet, nil without one, and its budgets
// on the priced models.
type Tenant struct {
	ID      string
	Wallet  *Budget
	Budgets []Budget
}

// Given is a_t, what a tenant can spend in a day: its wallet at its daily
// maximum, or the sum of its budgets when it has none. A wallet tenant's
// budgets are caps inside its wallet, not added.
func (t Tenant) Given() int64 {
	if t.Wallet != nil {
		return t.Wallet.DayMax()
	}
	var n int64
	for _, b := range t.Budgets {
		n += b.DayMax()
	}
	return n
}

// GivenOn is what the tenants without a wallet are given on one model a day.
func GivenOn(tenants []Tenant, modelID string) int64 {
	var n int64
	for _, t := range tenants {
		if t.Wallet != nil {
			continue
		}
		for _, b := range t.Budgets {
			if b.ModelID == modelID {
				n += b.DayMax()
			}
		}
	}
	return n
}

// History is the fleet's spend over the last days, for the overcommit
// suggestion.
type History struct {
	// Days is how many days have usage recorded, BusiestDay the most spent
	// in one of them over every priced model, wallets included.
	Days       int
	BusiestDay int64
}

// MinHistoryDays is how much history a suggestion needs.
const MinHistoryDays = 14

// MaxSuggestedOvercommit is the most the planner suggests.
const MaxSuggestedOvercommit = 5

// Fleet is the line the planner shows first.
type Fleet struct {
	// ValueDay is V: every priced model busy all day at the targets.
	ValueDay      int64 `json:"value_day"`
	CanBeGivenDay int64 `json:"can_be_given_day"`
	CeilingDay    int64 `json:"ceiling_day"`
	// GivenDay is what every tenant can spend in a day; WalletsDay the part
	// of it that is wallets.
	GivenDay   int64 `json:"given_day"`
	WalletsDay int64 `json:"wallets_day"`
	LeftDay    int64 `json:"left_day"`
	// SuggestedOvercommit is k_suggested, nil with less than two weeks of
	// history or nothing given.
	SuggestedOvercommit *float64 `json:"suggested_overcommit"`
}

// PlanFleet adds the priced models' plans and the tenants up.
func PlanFleet(plans []Plan, tenants []Tenant, h History) Fleet {
	var f Fleet
	var value float64
	for _, p := range plans {
		value += p.ValuePerSecond
		f.CanBeGivenDay += p.CanBeGivenDay
		f.CeilingDay += p.CeilingDay
	}
	f.ValueDay = round(SecondsPerDay * value)
	for _, t := range tenants {
		a := t.Given()
		f.GivenDay += a
		if t.Wallet != nil {
			f.WalletsDay += a
		}
	}
	f.LeftDay = f.CeilingDay - f.GivenDay
	// s is the share of what is given that the busiest day used. Budgets
	// are not kept by day, so what is given now stands for what was given
	// then.
	if h.Days >= MinHistoryDays && f.GivenDay > 0 && h.BusiestDay > 0 {
		s := float64(h.BusiestDay) / float64(f.GivenDay)
		k := math.Min(MaxSuggestedOvercommit, math.Max(1, 1/s))
		f.SuggestedOvercommit = &k
	}
	return f
}

// ByPeak sorts plans by their busiest hour's share, the highest first: where
// the next GPU should go. Models not measured come last.
func ByPeak(plans []Plan) []Plan {
	out := append([]Plan(nil), plans...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].PeakHourShare, out[j].PeakHourShare
		if a == nil || b == nil {
			return a != nil
		}
		return *a > *b
	})
	return out
}

// Proposal is one tenant's share of a split.
type Proposal struct {
	TenantID string `json:"tenant_id"`
	// Amount is what the tenant gets a day, in credits: a wallet, or a
	// budget on ModelID.
	Amount  int64  `json:"amount"`
	ModelID string `json:"model_id,omitempty"`
}

// Split proposes equal shares of amount among tenants: wallets when modelID
// is "", budgets on that model otherwise. Each share is rounded down to a
// whole credit. It changes nothing: an operator reviews it and applies it.
func Split(amount int64, tenants []string, modelID string) []Proposal {
	if len(tenants) == 0 || amount <= 0 {
		return []Proposal{}
	}
	share := amount / int64(len(tenants))
	out := make([]Proposal, len(tenants))
	for i, t := range tenants {
		out[i] = Proposal{TenantID: t, Amount: share, ModelID: modelID}
	}
	return out
}

func round(v float64) int64 { return int64(math.Round(v)) }
