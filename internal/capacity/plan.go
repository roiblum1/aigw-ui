package capacity

import (
	"math"
	"strconv"

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

// InputEquivalent is what one instance serves a second in input-equivalent
// tokens: uncached prompt tokens, cached ones and output tokens weighted by
// what each costs against an input token. It is what the input price is
// multiplied by to give the charge, and the one figure that compares two
// shapes of a model.
func InputEquivalent(t Throughput, cachedRatio, outputRatio float64) float64 {
	return t.InputPerSecond - t.CachedPerSecond + cachedRatio*t.CachedPerSecond + outputRatio*t.OutputPerSecond
}

// Ratios returns what a cached prompt token and an output token cost against
// an input token: the model's own prices, or a tenth and four without them.
func Ratios(p *Prices) (cached, output float64) {
	if p == nil || p.Input <= 0 {
		return defaultCachedRatio, defaultOutputRatio
	}
	return float64(p.Cached) / float64(p.Input), float64(p.Output) / float64(p.Input)
}

// Budget is a limit in the model's unit over a window: a tenant's quota, or
// the model's shared pool.
type Budget struct {
	Limit  int64
	Window string
}

// PerDay is the most the budget lets be used in a day: a budget per hour can
// be used 24 times.
func (b Budget) PerDay() int64 {
	return b.Limit * PerDay(b.Window)
}

// DefaultOvercommitLimit is how far the budgets may add up beyond what the
// GPUs serve a day, until it is set for the model.
const DefaultOvercommitLimit = 1.5

// ShapeInput is one shape a model runs in, across every site.
type ShapeInput struct {
	Shape Shape
	// Instances are its ready instances on the sites that are not drained.
	Instances float64
	Sites     []string
	// Measured is what one instance serves, nil until it is measured.
	Measured *Throughput
	// GPUPrice is what one of its GPUs costs an hour, in credits, nil when
	// no price is set for its GPU.
	GPUPrice *int64
}

// PlanInput is what is known of one model across the fleet.
type PlanInput struct {
	// Unit is what the model's quotas count: tokens, or credits once it has
	// prices. Prices is nil for a model counted in tokens.
	Unit   string
	Prices *Prices
	// CostExpression is the model's own cost expression for a model in
	// tokens. The plan assumes a counter of input plus output tokens.
	CostExpression string
	// Utilization is the share of the day the instances are expected to be
	// busy: what the prices that pay for them are worked out at.
	Utilization float64
	// OvercommitLimit is how far the tenants' budgets may add up beyond what
	// the model can be given a day.
	OvercommitLimit float64
	Shapes          []ShapeInput
	// Budgets are the enforced quotas of the enabled tenants on the model,
	// in Unit. Pool is the model's shared pool, nil when it is the strict 1.
	Budgets []Budget
	Pool    *Budget
	// The model's use over the last Days days, in Unit: the most in one day
	// and all of it.
	BusiestDay int64
	TotalUsed  int64
	Days       int
}

// ShapePlan is what one shape gives.
type ShapePlan struct {
	Shape     Shape    `json:"shape"`
	Instances float64  `json:"instances"`
	Sites     []string `json:"sites"`
	Measured  bool     `json:"measured"`
	// Concurrency is the requests in flight one instance serves within the
	// targets.
	Concurrency int `json:"concurrency"`
	// PerInstanceDay is what one instance serves in a day busy at that load
	// all day, in the model's unit.
	PerInstanceDay int64 `json:"per_instance_day"`
	// CostPerInstanceHour is its GPUs times their price, in credits.
	CostPerInstanceHour *int64 `json:"cost_per_instance_hour"`
	// CostPerMillion is what a million input-equivalent tokens cost on this
	// shape busy all day, in credits: which hardware serves the model for
	// the least.
	CostPerMillion *int64 `json:"cost_per_million"`
}

// Plan is what a model can be given.
type Plan struct {
	Unit        string      `json:"unit"`
	Utilization float64     `json:"utilization"`
	Shapes      []ShapePlan `json:"shapes"`
	// Instances are every ready instance; MeasuredInstances those whose
	// shape is measured. Only those are counted below.
	Instances         float64 `json:"instances"`
	MeasuredInstances float64 `json:"measured_instances"`
	// FullDay is what the measured instances serve in a day busy all day,
	// CapacityDay that at the expected utilization: what the model can be
	// given.
	FullDay     int64 `json:"full_day"`
	CapacityDay int64 `json:"capacity_day"`
	// GivenDay is what the tenants' budgets let them use in a day: a budget
	// per hour or minute at the most it allows in a day. ShortWindows counts
	// those.
	GivenDay     int64 `json:"given_day"`
	ShortWindows int   `json:"short_windows"`
	// Overcommit is GivenDay over CapacityDay.
	Overcommit *float64 `json:"overcommit"`
	// OvercommitLimit is how far GivenDay may go beyond CapacityDay,
	// CeilingDay is CapacityDay times it, and LeftDay what can still be given
	// under it: negative when the budgets are past it.
	OvercommitLimit float64 `json:"overcommit_limit"`
	CeilingDay      int64   `json:"ceiling_day"`
	LeftDay         int64   `json:"left_day"`
	// PoolDay is the most the model's shared pool lets be used in a day,
	// nil when it is the strict 1.
	PoolDay *int64 `json:"pool_day"`
	// BusiestDay is the most used in one day, BusiestShare that over
	// FullDay, and ObservedUtilization the average day over FullDay.
	BusiestDay          int64    `json:"busiest_day"`
	BusiestShare        *float64 `json:"busiest_share"`
	ObservedUtilization *float64 `json:"observed_utilization"`
	// SafeAllocatedDay is what the daily budgets could add up to before a
	// day like the busiest one reaches CapacityDay, if tenants keep using
	// the same share of them.
	SafeAllocatedDay *int64 `json:"safe_allocated_day"`
	// CostPerHour is what the measured instances cost an hour, in credits,
	// and BreakEven the one set of prices that pays for them at the
	// expected utilization. Nil when a measured shape's GPU has no price.
	CostPerHour *int64  `json:"cost_per_hour"`
	BreakEven   *Prices `json:"break_even"`
	// ChargedPerHour is what tenants are charged for the measured instances
	// busy for an hour at the model's prices. Nil without prices.
	ChargedPerHour *int64 `json:"charged_per_hour"`
	// Notes say what the figures leave out or assume.
	Notes []string `json:"notes"`
}

// MakePlan works out what a model can be given from what each of its
// shapes serves.
func MakePlan(in PlanInput) Plan {
	p := Plan{Unit: in.Unit, Utilization: in.Utilization, OvercommitLimit: in.OvercommitLimit, Shapes: []ShapePlan{},
		BusiestDay: in.BusiestDay, Notes: []string{}}
	if p.OvercommitLimit < 1 {
		p.OvercommitLimit = DefaultOvercommitLimit
	}
	for _, b := range in.Budgets {
		p.GivenDay += b.PerDay()
		if PerDay(b.Window) > 1 {
			p.ShortWindows++
		}
	}
	if in.Pool != nil {
		d := in.Pool.PerDay()
		p.PoolDay = &d
	}
	cachedRatio, outputRatio := Ratios(in.Prices)

	var full, charged, weighted, cost float64
	costKnown, noCache, gpuUnknown := true, false, false
	for _, sh := range in.Shapes {
		sp := ShapePlan{Shape: sh.Shape, Instances: sh.Instances, Sites: sh.Sites, Measured: sh.Measured != nil}
		p.Instances += sh.Instances
		if sh.GPUPrice != nil && sh.Shape.GPUsPerInstance > 0 {
			c := int64(math.Round(float64(*sh.GPUPrice) * sh.Shape.GPUsPerInstance))
			sp.CostPerInstanceHour = &c
		}
		if t := sh.Measured; t != nil {
			sp.Concurrency = t.Concurrency
			noCache = noCache || t.CachedShare == nil
			uncached := t.InputPerSecond - t.CachedPerSecond
			var perSecond float64
			if in.Prices != nil {
				perSecond = (uncached*float64(in.Prices.Input) + t.CachedPerSecond*float64(in.Prices.Cached) + t.OutputPerSecond*float64(in.Prices.Output)) / 1e6
				charged += perSecond * 3600 * sh.Instances
			} else {
				perSecond = t.InputPerSecond + t.OutputPerSecond
			}
			sp.PerInstanceDay = int64(math.Round(perSecond * 86400))
			w := InputEquivalent(*t, cachedRatio, outputRatio)
			if sp.CostPerInstanceHour != nil && w > 0 {
				m := int64(math.Ceil(float64(*sp.CostPerInstanceHour) / (3600 * w) * 1e6))
				sp.CostPerMillion = &m
			}
			if sh.Instances > 0 {
				p.MeasuredInstances += sh.Instances
				full += float64(sp.PerInstanceDay) * sh.Instances
				weighted += w * sh.Instances
				if sp.CostPerInstanceHour == nil {
					costKnown = false
					gpuUnknown = gpuUnknown || sh.Shape.GPU == "" || sh.Shape.GPUsPerInstance <= 0
				} else {
					cost += float64(*sp.CostPerInstanceHour) * sh.Instances
				}
			}
		}
		p.Shapes = append(p.Shapes, sp)
	}

	p.FullDay = int64(math.Round(full))
	p.CapacityDay = int64(math.Round(full * in.Utilization))
	p.CeilingDay = int64(math.Round(float64(p.CapacityDay) * p.OvercommitLimit))
	p.LeftDay = p.CeilingDay - p.GivenDay
	if p.CapacityDay > 0 {
		o := float64(p.GivenDay) / float64(p.CapacityDay)
		p.Overcommit = &o
	}
	if p.FullDay > 0 {
		b := float64(p.BusiestDay) / float64(p.FullDay)
		p.BusiestShare = &b
		if in.Days > 0 {
			u := float64(in.TotalUsed) / float64(in.Days) / float64(p.FullDay)
			p.ObservedUtilization = &u
		}
	}
	if p.BusiestDay > 0 && p.GivenDay > 0 && p.CapacityDay > 0 {
		safe := int64(math.Round(float64(p.CapacityDay) * float64(p.GivenDay) / float64(p.BusiestDay)))
		p.SafeAllocatedDay = &safe
	}
	if in.Prices != nil && p.MeasuredInstances > 0 {
		c := int64(math.Round(charged))
		p.ChargedPerHour = &c
	}
	if costKnown && p.MeasuredInstances > 0 {
		c := int64(math.Round(cost))
		p.CostPerHour = &c
		if weighted > 0 && in.Utilization > 0 {
			input := cost / (3600 * weighted * in.Utilization) * 1e6
			p.BreakEven = &Prices{
				Input:  max(int64(math.Ceil(input)), 1),
				Cached: int64(math.Ceil(input * cachedRatio)),
				Output: int64(math.Ceil(input * outputRatio)),
			}
		}
	}

	switch {
	case p.Instances == 0:
		p.Notes = append(p.Notes, "No instance of the model is ready on any site that is not drained, so it can be given nothing now.")
	case p.MeasuredInstances < p.Instances:
		p.Notes = append(p.Notes, fmtInstances(p.Instances-p.MeasuredInstances)+" in shapes not measured yet "+isAre(p.Instances-p.MeasuredInstances)+" not counted. Measure them to count them.")
	}
	switch {
	case gpuUnknown:
		// Nothing to price: its nodes carry no nvidia.com/gpu.product, or
		// its pods ask for no GPU.
		p.Notes = append(p.Notes, "The GPU of a measured shape is not known, so the cost and the prices that pay for it are not worked out. The hub reads it from the label nvidia.com/gpu.product of the nodes its pods run on and from the pods' requests for nvidia.com/gpu, and must be allowed to list both; the cluster's discovery message says when it is not.")
	case !costKnown:
		p.Notes = append(p.Notes, "A measured shape runs on a GPU without a price, so the cost and the prices that pay for it are not worked out. Set it under GPU prices.")
	}
	if noCache {
		p.Notes = append(p.Notes, "An instance did not report the prompt tokens it took from its cache, so its prompt tokens are valued as uncached.")
	}
	if in.Prices == nil && in.CostExpression != "" {
		p.Notes = append(p.Notes, "The model has its own cost expression; the amounts assume a counter of input plus output tokens.")
	}
	if p.ShortWindows > 0 {
		p.Notes = append(p.Notes, fmtCount(p.ShortWindows, "budget is", "budgets are")+" per minute or hour, and counted at the most "+itThey(p.ShortWindows)+" let a tenant use in a day.")
	}
	if p.CapacityDay > 0 && p.LeftDay < 0 {
		p.Notes = append(p.Notes, "The budgets are past the overcommit limit: what is given is more than "+strconvFloat(p.OvercommitLimit)+" times what the GPUs serve a day.")
	}
	if p.PoolDay != nil && p.CapacityDay > 0 && *p.PoolDay > p.CapacityDay {
		p.Notes = append(p.Notes, "The shared pool lets tenants use more in a day than the GPUs serve at the expected busy share; tenants past their budget are then served until it runs out.")
	}
	return p
}

func fmtInstances(n float64) string {
	if n == 1 {
		return "1 instance"
	}
	return strconvFloat(n) + " instances"
}

func isAre(n float64) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func itThey(n int) string {
	if n == 1 {
		return "it"
	}
	return "they"
}

func fmtCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconvFloat(float64(n)) + " " + many
}

func strconvFloat(n float64) string {
	return strconv.FormatFloat(n, 'f', -1, 64)
}

// CreditsPerHour turns dollars an hour into credits an hour.
func CreditsPerHour(usd float64) int64 {
	return int64(math.Round(usd * render.CreditsPerDollar))
}

// PerDay returns how many times a window fits in a day.
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
