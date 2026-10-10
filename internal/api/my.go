package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"aigw-ui/internal/store"
)

// The tenants' own page: a tenant signs in with one of its API keys and
// sees its budgets and its usage, and nothing of another tenant. The key is
// only compared here. No request to a model passes through the server.

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

// tenantAuth lets a request through that carries an API key of an enabled
// tenant, and hands the tenant on.
func (s *Server) tenantAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.tenantPage {
			writeError(w, http.StatusNotFound, "the tenants' page is turned off")
			return
		}
		addr, now := clientAddr(r), time.Now()
		if s.keyFailures.blocked(addr, now) {
			writeError(w, http.StatusTooManyRequests, "too many wrong keys; try again in a minute")
			return
		}
		key, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		t, err := s.st.TenantOfKey(r.Context(), key)
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

type myUsage struct {
	Tenant      string    `json:"tenant"`
	DisplayName string    `json:"display_name"`
	At          time.Time `json:"at"`
	// UsageEnabled is false when the server reads no usage counters.
	UsageEnabled bool       `json:"usage_enabled"`
	Budgets      []myBudget `json:"budgets"`
	Prices       []myPrice  `json:"prices"`
	// Days is the usage of the last 30 days, Hours of the last 2 days.
	// Steps in which nothing was used are left out.
	DaysFrom  time.Time          `json:"days_from"`
	Days      []store.UsagePoint `json:"days"`
	HoursFrom time.Time          `json:"hours_from"`
	Hours     []store.UsagePoint `json:"hours"`
}

// getMyUsage answers a tenant with its own budgets, usage and prices.
func (s *Server) getMyUsage(w http.ResponseWriter, r *http.Request) {
	t := r.Context().Value(tenantKey{}).(store.Tenant)
	now := time.Now().UTC()
	today := now.Truncate(24 * time.Hour)
	out := myUsage{Tenant: t.Slug, DisplayName: t.DisplayName, At: now, Budgets: []myBudget{}, Prices: []myPrice{},
		DaysFrom: today.AddDate(0, 0, -29), HoursFrom: today.AddDate(0, 0, -1)}

	mine := map[string]bool{}
	if s.usage != nil {
		rep, err := s.usage.Report(r.Context(), t.ID)
		if err != nil {
			failUsage(w, err)
			return
		}
		out.UsageEnabled = true
		for _, q := range rep.Quotas {
			mine[q.ModelID] = true
			out.Budgets = append(out.Budgets, myBudget{
				ModelName: q.ModelName, Unit: q.Unit, Limit: q.Limit, Window: q.Window, Used: q.Used, ResetsAt: q.ResetsAt,
				Enforced: !q.Shadow && !q.DryRun, BestEffortUsed: q.OverageUsed, BestEffortUntil: q.BestEffortUntil,
				BestEffortLimit: q.BestEffortLimit, BestEffortCapped: q.BestEffortCapped,
			})
		}
	}
	var err error
	if out.Days, err = s.st.UsageHistory(r.Context(), out.DaysFrom, store.StepDay, t.ID); err != nil {
		fail(w, err)
		return
	}
	if out.Hours, err = s.st.UsageHistory(r.Context(), out.HoursFrom, store.StepHour, t.ID); err != nil {
		fail(w, err)
		return
	}
	for _, p := range out.Days {
		mine[p.ModelID] = true
	}
	models, err := s.st.ListModels(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	for _, m := range models {
		if mine[m.ID] && m.Prices != nil {
			out.Prices = append(out.Prices, myPrice{ModelName: m.Name, Input: m.Prices.Input, Cached: m.Prices.Cached, Output: m.Prices.Output})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
