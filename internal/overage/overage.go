// Package overage moves a tenant whose budget for a model is nearly spent to
// the model's best-effort route, for the models that are set to serve such
// tenants instead of refusing them.
//
// The gateway decides whether a request is within budget in a filter that
// answers 429 itself, before a route is chosen, so the route has to be
// chosen ahead of the request. The hub reads the same counters the gateway
// enforces and writes the tenant into the best-effort route.
package overage

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aigw-ui/internal/store"
	"aigw-ui/internal/usage"
)

// Store is what the loop reads and writes in Postgres.
type Store interface {
	ListModels(ctx context.Context) ([]store.Model, error)
	ActiveOverage(ctx context.Context) ([]store.Overage, error)
	StartOverage(ctx context.Context, modelID, tenantID string, until time.Time) (bool, error)
}

// Usage reports the tokens every tenant has used in its current window.
type Usage interface {
	Report(ctx context.Context, tenantID string) (usage.Report, error)
}

// Syncer is told when the set of tenants in overage changed.
type Syncer interface {
	Changed(ctx context.Context, action, summary string)
}

type Service struct {
	st        Store
	usage     Usage
	sy        Syncer
	every     time.Duration
	threshold float64

	mu sync.Mutex
	// last is the set after the previous tick, to tell which periods ended.
	// It is nil before the first tick.
	last map[period]string
}

// period identifies a tenant on a model.
type period struct{ modelID, tenantID string }

func New(st Store, us Usage, sy Syncer, every time.Duration, threshold float64) *Service {
	return &Service{st: st, usage: us, sy: sy, every: every, threshold: threshold}
}

// Every is how often the counters are read.
func (s *Service) Every() time.Duration { return s.every }

// Run reads the counters every interval until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.every)
	defer ticker.Stop()
	for {
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("overage check failed; the tenants in overage stay as they are", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Movable reports whether a quota with this window can be moved to
// best-effort. A shorter window is over before the loop and a sync can act.
func Movable(window string) bool { return window == "1h" || window == "1d" }

// Tick moves every tenant that has used the threshold share of its budget
// on a best-effort model, and notices the tenants whose window ended. A
// tenant stays moved until its window ends: the counter of the next window
// starts at 0, so it is back within budget on its own.
//
// When the counters cannot be read nobody is moved. "Could not ask" is
// never taken as "has used nothing" or "has used everything".
func (s *Service) Tick(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	bestEffort, err := s.bestEffortModels(ctx)
	if err != nil {
		return err
	}
	active, err := s.st.ActiveOverage(ctx)
	if err != nil {
		return err
	}
	now := make(map[period]string, len(active))
	for _, o := range active {
		now[period{o.ModelID, o.TenantID}] = o.TenantSlug + " on " + o.ModelName
	}
	moves := s.ended(now, len(bestEffort) > 0)
	started, err := s.start(ctx, bestEffort, now)
	moves = append(moves, started...)
	s.last = now
	if len(moves) > 0 {
		// One task and one sync for the tick, however many tenants moved.
		// The periods themselves are kept in the overage table.
		s.sy.Changed(ctx, "overage", strings.Join(moves, " "))
	}
	return err
}

// ended returns a line for every period of the previous tick that is not in
// now. The first tick has no previous one: a period may have ended while the
// server was not running, so the clusters are synced once to be sure.
func (s *Service) ended(now map[period]string, anyBestEffort bool) []string {
	if s.last == nil {
		if !anyBestEffort {
			return nil
		}
		return []string{"The server started: the tenants served as best-effort are applied again."}
	}
	var moves []string
	for p, label := range s.last {
		if _, ok := now[p]; !ok {
			moves = append(moves, label+" is served as standard again: its overage period ended.")
		}
	}
	sort.Strings(moves)
	return moves
}

// bestEffortModels returns the IDs of the models that serve a spent budget
// as best-effort.
func (s *Service) bestEffortModels(ctx context.Context) (map[string]bool, error) {
	models, err := s.st.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range models {
		if m.Fleet && m.SpentMode == store.SpentBestEffort {
			out[m.ID] = true
		}
	}
	return out, nil
}

// start moves the tenants that are past the threshold and not moved yet,
// adds them to now and returns a line for each.
func (s *Service) start(ctx context.Context, bestEffort map[string]bool, now map[period]string) ([]string, error) {
	if len(bestEffort) == 0 {
		// Nothing to move, so Redis is not asked at all.
		return nil, nil
	}
	rep, err := s.usage.Report(ctx, "")
	if err != nil {
		return nil, err
	}
	var moves []string
	for _, q := range rep.Quotas {
		p := period{q.ModelID, q.TenantID}
		if _, moved := now[p]; moved || !bestEffort[q.ModelID] || q.Shadow || !Movable(q.Window) {
			continue
		}
		if float64(q.Used) < s.threshold*float64(q.Limit) {
			continue
		}
		ok, err := s.st.StartOverage(ctx, q.ModelID, q.TenantID, q.ResetsAt)
		if err != nil {
			return moves, err
		}
		if !ok {
			continue
		}
		label := q.TenantSlug + " on " + q.ModelName
		now[p] = label
		moves = append(moves, label+" has used "+share(q.Used, q.Limit)+" of its budget and is served as best-effort until "+q.ResetsAt.UTC().Format("2006-01-02 15:04 UTC")+".")
	}
	return moves, nil
}

func share(used, limit int64) string {
	if limit <= 0 {
		return "all"
	}
	return strconv.FormatInt(used*100/limit, 10) + "%"
}
