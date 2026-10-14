package capacity

import (
	"math"

	"aigw-ui/internal/render"
)

// Prices are a model's prices in credits per million tokens.
type Prices struct {
	Input  int64 `json:"price_input"`
	Cached int64 `json:"price_cached"`
	Output int64 `json:"price_output"`
}

// The price ratios used for a model without prices: a cached prompt token
// at a tenth of an input token, an output token at four. docs/pricing.md
// suggests them.
const (
	defaultCachedRatio = 0.1
	defaultOutputRatio = 4
)

// Throughput is what one instance served at the highest load that met the
// promised latency.
type Throughput struct {
	Concurrency     int     `json:"concurrency"`
	InputPerSecond  float64 `json:"input_per_s"`
	CachedPerSecond float64 `json:"cached_per_s"`
	OutputPerSecond float64 `json:"output_per_s"`
	// CachedShare is the part of the prompt tokens the instance took from
	// its cache, or nil when it does not say. CachedPerSecond is then 0.
	CachedShare *float64 `json:"cached_share"`
}

// ThroughputOf returns what a step served.
func ThroughputOf(s Step) Throughput {
	t := Throughput{Concurrency: s.Concurrency, InputPerSecond: s.InputPerSecond(), OutputPerSecond: s.OutputPerSecond(), CachedShare: s.CachedShare()}
	if t.CachedShare != nil {
		t.CachedPerSecond = t.InputPerSecond * *t.CachedShare
	}
	return t
}

// PlanInput is what is known of one model across the fleet.
type PlanInput struct {
	// Unit is what the model's quotas count: tokens, or credits once it
	// has prices. Prices is nil for a model counted in tokens.
	Unit   string
	Prices *Prices
	// CostExpression is the model's own cost expression for a model in
	// tokens. The plan assumes a counter of input plus output tokens.
	CostExpression string
	Throughput     Throughput
	Settings       Settings
	// Instances is the number of ready instances on every site.
	Instances float64
	// AllocatedPerDay is every tenant's limit on the model, as an amount per
	// day, in Unit. A limit per hour counts 24 times.
	AllocatedPerDay int64
	// BusiestDay is the most the model was used in one day lately, in Unit.
	BusiestDay int64
}

// Plan is what a model can be given.
type Plan struct {
	Unit string `json:"unit"`
	// PerInstanceDay is what one instance serves in a day busy at the
	// measured load all day long, in Unit.
	PerInstanceDay int64   `json:"per_instance_day"`
	Instances      float64 `json:"instances"`
	// CapacityDay is what every instance serves in a day at the expected
	// utilization: what the model can be given.
	CapacityDay  int64 `json:"capacity_day"`
	AllocatedDay int64 `json:"allocated_day"`
	// Overcommit is the allocated amount divided by CapacityDay. Above 1,
	// the budgets together exceed what the model serves; that is safe only
	// as long as tenants do not use them all on the same day.
	Overcommit *float64 `json:"overcommit"`
	// BusiestDay is the most the model was used in one day, and BusiestShare
	// that amount as a part of what every instance serves busy all day.
	BusiestDay   int64    `json:"busiest_day"`
	BusiestShare *float64 `json:"busiest_share"`
	// CostPerInstanceHour is the cost of one instance an hour, in credits,
	// and BreakEven the prices that pay it at the expected utilization.
	// Both are nil when the cost is not known.
	CostPerInstanceHour *int64  `json:"cost_per_instance_hour"`
	BreakEven           *Prices `json:"break_even"`
	// ChargedPerInstanceHour is what tenants are charged for one instance
	// busy for an hour at the model's prices. Nil without prices.
	ChargedPerInstanceHour *int64 `json:"charged_per_instance_hour"`
	// Notes say what the figures assume.
	Notes []string `json:"notes"`
}

// MakePlan works out what a model can be given from what one instance
// serves.
func MakePlan(in PlanInput) Plan {
	t := in.Throughput
	p := Plan{Unit: in.Unit, Instances: in.Instances, AllocatedDay: in.AllocatedPerDay, BusiestDay: in.BusiestDay, Notes: []string{}}
	uncached := t.InputPerSecond - t.CachedPerSecond
	if t.CachedShare == nil {
		p.Notes = append(p.Notes, "The instance does not report the prompt tokens it took from its cache, so every prompt token is valued as uncached.")
	}

	var perSecond float64
	if in.Prices != nil {
		perSecond = (uncached*float64(in.Prices.Input) + t.CachedPerSecond*float64(in.Prices.Cached) + t.OutputPerSecond*float64(in.Prices.Output)) / 1e6
	} else {
		perSecond = t.InputPerSecond + t.OutputPerSecond
		if in.CostExpression != "" {
			p.Notes = append(p.Notes, "The model has its own cost expression; the amounts assume a counter of input plus output tokens.")
		}
	}
	p.PerInstanceDay = int64(math.Round(perSecond * 86400))
	full := float64(p.PerInstanceDay) * in.Instances
	p.CapacityDay = int64(math.Round(full * in.Settings.Utilization))
	if p.CapacityDay > 0 {
		o := float64(p.AllocatedDay) / float64(p.CapacityDay)
		p.Overcommit = &o
	}
	if full > 0 {
		b := float64(p.BusiestDay) / full
		p.BusiestShare = &b
	}
	if in.Instances == 0 {
		p.Notes = append(p.Notes, "No instance of the model is ready on any site, so it can be given nothing.")
	}

	cachedRatio, outputRatio := defaultCachedRatio, float64(defaultOutputRatio)
	if in.Prices != nil && in.Prices.Input > 0 {
		cachedRatio = float64(in.Prices.Cached) / float64(in.Prices.Input)
		outputRatio = float64(in.Prices.Output) / float64(in.Prices.Input)
		hour := int64(math.Round(perSecond * 3600))
		p.ChargedPerInstanceHour = &hour
	}
	if in.Settings.CostPerHour > 0 {
		cost := int64(math.Round(in.Settings.CostPerHour * render.CreditsPerDollar))
		p.CostPerInstanceHour = &cost
		// Input-equivalent tokens per second: what the input price is
		// multiplied by to give the charge.
		weighted := uncached + cachedRatio*t.CachedPerSecond + outputRatio*t.OutputPerSecond
		if weighted > 0 {
			input := float64(cost) / (3600 * weighted * in.Settings.Utilization) * 1e6
			p.BreakEven = &Prices{
				Input:  max(int64(math.Ceil(input)), 1),
				Cached: int64(math.Ceil(input * cachedRatio)),
				Output: int64(math.Ceil(input * outputRatio)),
			}
		}
	}
	return p
}

// PerDay returns how many times a window fits in a day: a limit per hour
// can be used 24 times a day.
func PerDay(window string) int64 {
	switch window {
	case "1s":
		return 86400
	case "1m":
		return 1440
	case "1h":
		return 24
	}
	return 1
}
