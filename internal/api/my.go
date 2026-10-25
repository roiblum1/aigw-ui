package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
	"aigw-ui/internal/token"
	"aigw-ui/internal/usage"
)

// The tenants' own page: a tenant signs in with one of its API keys or
// tokens and sees its budgets and its usage, and nothing of another tenant.
// The key is only compared, and a token checked, here. No request to a
// model passes through the server.

type tenantKey struct{}

// maxKeyFailures is how many wrong keys one address may send in a minute.
const maxKeyFailures = 10

// keyFailures counts wrong keys by address in the running minute.
type keyFailures struct {
	mu     sync.Mutex
	minute int64
	count  map[string]int
}

func (f *keyFailures) blocked(addr string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.minute == now.Unix()/60 && f.count[addr] >= maxKeyFailures
}

func (f *keyFailures) add(addr string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := now.Unix() / 60; f.minute != m || f.count == nil {
		f.minute, f.count = m, map[string]int{}
	}
	f.count[addr]++
}

// clientAddr is the address a request came from: the last hop the platform's
// router added, which a client cannot choose, or the peer.
func clientAddr(r *http.Request) string {
	if hops := strings.Split(r.Header.Get("X-Forwarded-For"), ","); hops[len(hops)-1] != "" {
		return strings.TrimSpace(hops[len(hops)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tenantOf returns the enabled tenant an API key or a token belongs to. A
// token is checked on the hub, against its own public keys: its signature,
// issuer, audience and expiry, then that it is one of the tenant's keys and
// not revoked. store.ErrNotFound for anything else.
func (s *Server) tenantOf(ctx context.Context, key string, now time.Time) (store.Tenant, error) {
	if strings.HasPrefix(key, store.PersonalKeyPrefix) {
		return s.st.TenantOfPersonalKey(ctx, key)
	}
	if !token.LooksLike(key) {
		return s.st.TenantOfKey(ctx, key)
	}
	keys, err := s.st.ListSigningKeys(ctx)
	if err != nil {
		return store.Tenant{}, err
	}
	set := s.st.Tokens()
	c, err := token.Verify(key, store.PublicKeys(keys), set.Issuer, set.Audience, now)
	if err != nil {
		return store.Tenant{}, store.ErrNotFound
	}
	return s.st.TenantOfToken(ctx, c)
}

// tenantAuth lets a request through that carries an API key or a token of
// an enabled tenant, and hands the tenant on.
func (s *Server) tenantAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.tenantPage {
			writeError(w, http.StatusNotFound, "the tenants' page is turned off")
			return
		}
		// No key: a person who signed in with OpenShift, for one of the
		// tenants linked to their groups.
		if bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); strings.TrimSpace(bearer) == "" && s.sso != nil {
			if ctx, ok := s.sessionTenant(w, r); ok {
				next.ServeHTTP(w, r.WithContext(ctx))
			}
			return
		}
		addr, now := clientAddr(r), time.Now()
		if s.keyFailures.blocked(addr, now) {
			writeError(w, http.StatusTooManyRequests, "too many wrong keys; try again in a minute")
			return
		}
		key, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		t, err := s.tenantOf(r.Context(), key, now)
		if errors.Is(err, store.ErrNotFound) {
			s.keyFailures.add(addr, now)
			slog.Warn("tenant page: wrong key", "remote_addr", addr)
			writeError(w, http.StatusUnauthorized, "that is not an API key of an enabled tenant")
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, t)))
	})
}

// myBudget is a tenant's budget on one model and what it has used of it.
type myBudget struct {
	ModelName string `json:"model_name"`
	// Unit is "tokens" or "credits". A credit is 0.00001 dollars.
	Unit     string    `json:"unit"`
	Limit    int64     `json:"limit"`
	Window   string    `json:"window"`
	Used     int64     `json:"used"`
	ResetsAt time.Time `json:"resets_at"`
	// Enforced is false while the budget is only counted: nobody is refused.
	Enforced bool `json:"enforced"`
	// BestEffortUsed is what was used after the budget was spent, on a
	// model that goes on serving at the lowest priority.
	BestEffortUsed   int64      `json:"best_effort_used"`
	BestEffortUntil  *time.Time `json:"best_effort_until,omitempty"`
	BestEffortLimit  *int64     `json:"best_effort_limit,omitempty"`
	BestEffortCapped bool       `json:"best_effort_capped,omitempty"`
}

// myPrice is what a model costs, in credits per million tokens.
type myPrice struct {
	ModelName string `json:"model_name"`
	Input     int64  `json:"price_input"`
	Cached    int64  `json:"price_cached"`
	Output    int64  `json:"price_output"`
}

// myTenantBudget is a tenant's budget across every priced model, in
// credits, and what it has used of it.
type myTenantBudget struct {
	Limit    int64     `json:"limit"`
	Window   string    `json:"window"`
	Used     int64     `json:"used"`
	ResetsAt time.Time `json:"resets_at"`
	// Enforced is false while the gateways do not hold the tenant to it.
	Enforced bool `json:"enforced"`
}

type myUsage struct {
	Tenant      string    `json:"tenant"`
	DisplayName string    `json:"display_name"`
	At          time.Time `json:"at"`
	// UsageEnabled is false when the server reads no usage counters.
	UsageEnabled bool `json:"usage_enabled"`
	// GatewayURL is the address to send requests to, for the setup the page
	// shows; "" when the hub is not told it.
	GatewayURL string     `json:"gateway_url"`
	Budgets    []myBudget `json:"budgets"`
	// Budget is the tenant's budget across models, nil when it has none.
	Budget *myTenantBudget `json:"budget"`
	// Models names the models the tenant may call, nil for every model.
	// Budgets and Prices then cover those only.
	Models []string  `json:"models"`
	Prices []myPrice `json:"prices"`
	// Days is the usage of the last 30 days, Hours of the last 2 days.
	// Steps in which nothing was used are left out.
	DaysFrom  time.Time          `json:"days_from"`
	Days      []store.UsagePoint `json:"days"`
	HoursFrom time.Time          `json:"hours_from"`
	Hours     []store.UsagePoint `json:"hours"`
	// TokenCounts is set when the gateways count the tenant's requests and
	// tokens. TokenDays and TokenHours are what they counted, as Days and
	// Hours are: numbers only, nothing of a prompt or an answer.
	TokenCounts bool               `json:"token_counts"`
	TokenDays   []store.TokenPoint `json:"token_days"`
	TokenHours  []store.TokenPoint `json:"token_hours"`
}

// getMyUsage answers a tenant with its own budgets, usage and prices.
func (s *Server) getMyUsage(w http.ResponseWriter, r *http.Request) {
	t := r.Context().Value(tenantKey{}).(store.Tenant)
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	out := myUsage{Tenant: t.Slug, DisplayName: t.DisplayName, At: now, GatewayURL: s.gatewayURL, Budgets: []myBudget{}, Prices: []myPrice{},
		DaysFrom: today.AddDate(0, 0, -29), HoursFrom: today.AddDate(0, 0, -1)}

	// A tenant held to some models is shown a budget or a price on those
	// only: one on a model it is refused reads as something it can spend.
	calls := func(modelID string) bool { return t.Models == nil || slices.Contains(t.Models, modelID) }
	mine := map[string]bool{}
	if t.Budget != nil {
		out.Budget = &myTenantBudget{Limit: t.Budget.Credits, Window: t.Budget.Window, ResetsAt: render.WindowEnd(t.Budget.Window, now)}
	}
	// Everything the page shows is read side by side.
	ctx := r.Context()
	var rep usage.Report
	var repErr error
	var models []store.Model
	errs := make([]error, 5)
	var wg sync.WaitGroup
	if s.usage != nil {
		wg.Go(func() { rep, repErr = s.usage.Report(ctx, t.ID) })
	}
	wg.Go(func() { out.Days, errs[0] = s.st.UsageHistory(ctx, out.DaysFrom, store.StepDay, t.ID) })
	wg.Go(func() { out.Hours, errs[1] = s.st.UsageHistory(ctx, out.HoursFrom, store.StepHour, t.ID) })
	wg.Go(func() { out.TokenDays, errs[2] = s.st.TokenHistory(ctx, out.DaysFrom, store.StepDay, t.ID) })
	wg.Go(func() { out.TokenHours, errs[3] = s.st.TokenHistory(ctx, out.HoursFrom, store.StepHour, t.ID) })
	wg.Go(func() { models, errs[4] = s.st.ListModels(ctx) })
	wg.Wait()
	if repErr != nil {
		failUsage(w, repErr)
		return
	}
	for _, err := range errs {
		if err != nil {
			fail(w, err)
			return
		}
	}
	if s.usage != nil {
		out.UsageEnabled = true
		for _, b := range rep.Budgets {
			if out.Budget != nil {
				// The budget is in the report only where a gateway enforces it.
				out.Budget.Used, out.Budget.Enforced = b.Used, true
			}
		}
		for _, q := range rep.Quotas {
			// Where the budget across models holds the tenant, the model's
			// limit is not its own: the budget is shown instead.
			if !calls(q.ModelID) || q.Budget {
				continue
			}
			mine[q.ModelID] = true
			out.Budgets = append(out.Budgets, myBudget{
				ModelName: q.ModelName, Unit: q.Unit, Limit: q.Limit, Window: q.Window, Used: q.Used, ResetsAt: q.ResetsAt,
				Enforced: !q.Shadow && !q.DryRun, BestEffortUsed: q.OverageUsed, BestEffortUntil: q.BestEffortUntil,
				BestEffortLimit: q.BestEffortLimit, BestEffortCapped: q.BestEffortCapped,
			})
		}
	}
	for _, p := range out.Days {
		mine[p.ModelID] = true
	}
	out.TokenCounts = s.usage != nil && s.st.TokenCounts()
	out.TokenDays, out.TokenHours = callable(out.TokenDays, calls), callable(out.TokenHours, calls)
	for _, p := range out.TokenDays {
		mine[p.ModelID] = true
	}
	if t.Models != nil {
		out.Models = []string{}
	}
	for _, m := range models {
		if t.Models != nil && calls(m.ID) {
			out.Models = append(out.Models, m.Name)
		}
		if mine[m.ID] && calls(m.ID) && m.Prices != nil {
			out.Prices = append(out.Prices, myPrice{ModelName: m.Name, Input: m.Prices.Input, Cached: m.Prices.Cached, Output: m.Prices.Output})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// callable keeps the points of the models the tenant may call: what it sent
// before it was held to some is not shown as a model it can use.
func callable(points []store.TokenPoint, calls func(modelID string) bool) []store.TokenPoint {
	out := []store.TokenPoint{}
	for _, p := range points {
		if calls(p.ModelID) {
			out = append(out, p)
		}
	}
	return out
}
