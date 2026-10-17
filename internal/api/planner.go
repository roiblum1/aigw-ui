package api

import (
	"context"
	"net/http"
	"time"

	"aigw-ui/internal/budget"
	"aigw-ui/internal/store"
)

// plannerLLMI is one LLMInferenceService as the planner counts it.
type plannerLLMI struct {
	ClusterID   string  `json:"cluster_id"`
	ClusterName string  `json:"cluster_name"`
	Namespace   string  `json:"namespace"`
	Name        string  `json:"name"`
	Ready       float64 `json:"ready"`
	// ValuePerSecond is r_i, credits a second for one instance; nil when it
	// is not measured.
	ValuePerSecond *float64   `json:"value_per_second"`
	MeasuredAt     *time.Time `json:"measured_at"`
	ProfileVersion *int       `json:"profile_version"`
	Mode           string     `json:"mode"`
	Stale          string     `json:"stale"`
}

// marketValue is what the Overview shows of the fleet's spend and cost.
type marketValue struct {
	// YesterdaySpend is what the priced models were spent on yesterday
	// (UTC), at their prices: what the same traffic would have cost on
	// OpenRouter when the prices are the market's.
	YesterdaySpend int64 `json:"yesterday_spend"`
	// CostDay is what the measured instances' GPUs cost a day, with GPU
	// costs entered; nil without. CostPartial says some are left out.
	CostDay     *int64 `json:"cost_day"`
	CostPartial bool   `json:"cost_partial"`
}

type plannerModel struct {
	plan  budget.Plan
	llmis []plannerLLMI
}

type plannerResult struct {
	models map[string]plannerModel
	fleet  budget.Fleet
	// byPeak lists the priced models by their busiest hour's share, the
	// highest first: where the next GPU should go.
	byPeak []string
	market marketValue
}

// planner works out the planner's figures from the capacity plans, the
// tenants' budgets and the last 30 days of spend.
func (s *Server) planner(ctx context.Context, models []capacityModel) (plannerResult, error) {
	out := plannerResult{models: map[string]plannerModel{}, byPeak: []string{}}
	now := time.Now().UTC()
	spend, err := s.st.SpendSince(ctx, now.Truncate(24*time.Hour).AddDate(0, 0, -recentDays))
	if err != nil {
		return out, err
	}
	quotas, err := s.st.AllQuotas(ctx)
	if err != nil {
		return out, err
	}
	var plans []budget.Plan
	priced := map[string]bool{}
	for _, m := range models {
		var cost int64
		if c := m.Plan.CostPerHour; c != nil {
			cost = *c * 24
			out.market.CostDay = add(out.market.CostDay, cost)
		} else if m.Plan.MeasuredInstances > 0 {
			out.market.CostPartial = true
		}
		in := budget.Model{ID: m.ModelID, Prices: m.prices, BusyShare: m.TargetUtilization, OvercommitLimit: m.Plan.OvercommitLimit,
			BusiestHour: spend.BusiestHour[m.ModelID]}
		for i, l := range m.LLMIs {
			row := budget.LLMI{Ready: m.Plan.Shapes[i].Instances}
			if l.Run != nil {
				row.PerInstance = l.Run.PerInstance()
			}
			in.LLMIs = append(in.LLMIs, row)
		}
		p, ok := budget.PlanModel(in)
		if !ok {
			continue
		}
		priced[m.ModelID] = true
		pm := plannerModel{plan: p}
		for i, l := range m.LLMIs {
			row := plannerLLMI{ClusterID: l.ClusterID, ClusterName: l.ClusterName, Namespace: l.Namespace, Name: l.Name,
				Ready: in.LLMIs[i].Ready, ValuePerSecond: p.Values[i], Stale: l.Stale}
			if r := l.Run; r != nil {
				at := r.CreatedAt
				if r.FinishedAt != nil {
					at = *r.FinishedAt
				}
				row.MeasuredAt, row.ProfileVersion, row.Mode = &at, r.ProfileVersion, r.Mode
			}
			pm.llmis = append(pm.llmis, row)
		}
		out.models[m.ModelID] = pm
		plans = append(plans, p)
	}
	for _, p := range budget.ByPeak(plans) {
		out.byPeak = append(out.byPeak, p.ModelID)
	}
	out.fleet = budget.PlanFleet(plans, tenantsOf(quotas, priced), budget.History{Days: spend.Days, BusiestDay: spend.BusiestDay})
	yesterday := now.Truncate(24 * time.Hour)
	if out.market.YesterdaySpend, err = s.st.SpentBetween(ctx, yesterday.AddDate(0, 0, -1), yesterday); err != nil {
		return out, err
	}
	return out, nil
}

func add(total *int64, n int64) *int64 {
	if total == nil {
		return &n
	}
	*total += n
	return total
}

// tenantsOf groups the enforced budgets of enabled tenants on priced models
// by tenant. A model in tokens has no dollars to add. A quota of a tenant
// with a budget across models is not enforced there: the budget holds the
// tenant in its place, as the Usage page shows.
func tenantsOf(quotas []store.Quota, priced map[string]bool) []budget.Tenant {
	byID := map[string]*budget.Tenant{}
	var order []string
	for _, q := range quotas {
		if q.Shadow || q.Budgeted || !priced[q.ModelID] {
			continue
		}
		t := byID[q.TenantID]
		if t == nil {
			t = &budget.Tenant{ID: q.TenantID}
			byID[q.TenantID] = t
			order = append(order, q.TenantID)
		}
		t.Budgets = append(t.Budgets, budget.Budget{ModelID: q.ModelID, Limit: q.TokenLimit, Window: q.Window})
	}
	out := make([]budget.Tenant, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

// splitBody asks for equal shares of an amount a day among tenants.
type splitBody struct {
	Amount    int64    `json:"amount"`
	TenantIDs []string `json:"tenant_ids"`
	// ModelID is the model the budgets are on.
	ModelID string `json:"model_id"`
}

// split proposes equal budgets on one model among tenants, of an amount no
// more than what is left to give on it and in the fleet. It changes
// nothing: the page shows the proposal and an operator applies it.
func (s *Server) split(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var b splitBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	switch {
	case b.ModelID == "":
		fail(w, invalid("model_id is required: a split is of budgets on one model"))
		return
	case len(b.TenantIDs) == 0 || b.Amount <= 0:
		fail(w, invalid("amount must be more than 0, and tenant_ids must name at least one tenant"))
		return
	}
	models, _, err := s.plans(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	plan, err := s.planner(ctx, models)
	if err != nil {
		fail(w, err)
		return
	}
	var left int64
	found := false
	for _, m := range models {
		if m.ModelID == b.ModelID {
			found, left = m.prices != nil, budgetOf(m).LeftDay
		}
	}
	switch {
	case !found:
		fail(w, invalid("the model has no prices, so there are no dollars to split"))
		return
	case b.Amount > left || b.Amount > plan.fleet.LeftDay:
		fail(w, invalid("the amount is more than is left to give: %s on the model, %s in the fleet",
			store.Amount(max(left, 0), store.UnitCredits), store.Amount(max(plan.fleet.LeftDay, 0), store.UnitCredits)))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": budget.Split(b.Amount, b.TenantIDs, b.ModelID)})
}
