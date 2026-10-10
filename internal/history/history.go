// Package history keeps what every tenant used of every model, hour by
// hour. The gateways' counters in Redis only hold the running period of a
// quota; this package reads them once a minute and writes what was used
// since the last look to Postgres.
//
// It reads counters and nothing else: no request and no answer ever reaches
// the server.
package history

import (
	"context"
	"log/slog"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
	"aigw-ui/internal/usage"
)

// Keep is how long the history is kept.
const Keep = 400 * 24 * time.Hour

// Store is what the recorder reads and writes in Postgres.
type Store interface {
	RecordUsage(ctx context.Context, look func(map[store.UsageKey]store.UsageCursor) ([]store.UsageDelta, []store.UsageCursor, error)) error
	PruneUsage(ctx context.Context, before time.Time) error
}

// Usage reports the counters of the windows that contain a point in time.
type Usage interface {
	ReportAt(ctx context.Context, tenantID string, at time.Time) (usage.Report, error)
}

type Service struct {
	st    Store
	usage Usage
	every time.Duration
	// pruned is the day old history was last deleted.
	pruned time.Time
}

func New(st Store, us Usage, every time.Duration) *Service {
	return &Service{st: st, usage: us, every: every}
}

// Run records the usage every interval until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.every)
	defer ticker.Stop()
	for {
		if err := s.Tick(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Warn("usage history: could not record; it is added at the next look", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick adds what was used since the last look to the history.
//
// A counter only grows within its window, so the usage since the last look
// is the counter minus what it held then. When a window ended in between,
// its counter is read once more for what was used after the last look: the
// rate limit service keeps it for a while. A counter lower than at the last
// look was reset, and counts from 0.
//
// When Redis cannot be read nothing is written and the cursors stay, so the
// usage is added at the next look that works.
func (s *Service) Tick(ctx context.Context, now time.Time) error {
	err := s.st.RecordUsage(ctx, func(cursors map[store.UsageKey]store.UsageCursor) ([]store.UsageDelta, []store.UsageCursor, error) {
		rep, err := s.usage.ReportAt(ctx, "", now)
		if err != nil {
			return nil, nil, err
		}
		ended := newEnded(ctx, s.usage)
		var deltas []store.UsageDelta
		var next []store.UsageCursor
		for _, q := range rep.Quotas {
			key := store.UsageKey{TenantID: q.TenantID, ModelID: q.ModelID}
			start := render.WindowStart(q.Window, now)
			last, seen := cursors[key]
			if seen && last.Window == q.Window && last.WindowStart.Before(start) {
				// The rest of the window that ended, in its last hour.
				if old, ok := ended.quota(key, last.WindowStart); ok {
					d := store.UsageDelta{UsageKey: key, Unit: q.Unit, Hour: render.WindowEnd(q.Window, last.WindowStart).Add(-time.Second),
						Used: max(0, old.Used-last.Used), BestEffort: max(0, old.OverageUsed-last.BestEffort)}
					if d.Used > 0 || d.BestEffort > 0 {
						deltas = append(deltas, d)
					}
				}
			}
			if !seen || last.Window != q.Window || !last.WindowStart.Equal(start) {
				last = store.UsageCursor{}
			}
			d := store.UsageDelta{UsageKey: key, Unit: q.Unit, Hour: now, Used: since(q.Used, last.Used), BestEffort: since(q.OverageUsed, last.BestEffort)}
			if d.Used > 0 || d.BestEffort > 0 {
				deltas = append(deltas, d)
			}
			next = append(next, store.UsageCursor{UsageKey: key, Window: q.Window, WindowStart: start, Used: q.Used, BestEffort: q.OverageUsed})
		}
		return deltas, next, ended.err
	})
	if err != nil {
		return err
	}
	if day := now.UTC().Truncate(24 * time.Hour); day.After(s.pruned) {
		s.pruned = day
		return s.st.PruneUsage(ctx, now.Add(-Keep))
	}
	return nil
}

// since is what was used between two looks at a counter of one window.
func since(now, last int64) int64 {
	if now < last {
		return now // the counter was reset
	}
	return now - last
}

// ended reads the counters of windows that are over, once per window start.
type ended struct {
	ctx     context.Context
	usage   Usage
	reports map[time.Time]map[store.UsageKey]usage.Quota
	err     error
}

func newEnded(ctx context.Context, us Usage) *ended {
	return &ended{ctx: ctx, usage: us, reports: map[time.Time]map[store.UsageKey]usage.Quota{}}
}

// quota returns the counters of a tenant on a model in the window that
// began at start. After a failure nothing more is asked, and the look as a
// whole fails, so the cursors stay for the next one.
func (e *ended) quota(key store.UsageKey, start time.Time) (usage.Quota, bool) {
	if e.err != nil {
		return usage.Quota{}, false
	}
	byKey, ok := e.reports[start]
	if !ok {
		rep, err := e.usage.ReportAt(e.ctx, "", start)
		if err != nil {
			e.err = err
			return usage.Quota{}, false
		}
		byKey = make(map[store.UsageKey]usage.Quota, len(rep.Quotas))
		for _, q := range rep.Quotas {
			byKey[store.UsageKey{TenantID: q.TenantID, ModelID: q.ModelID}] = q
		}
		e.reports[start] = byKey
	}
	q, ok := byKey[key]
	return q, ok
}
