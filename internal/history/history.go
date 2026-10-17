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
	RecordUsage(ctx context.Context, look func(store.Before) (store.Look, error)) error
	PruneUsage(ctx context.Context, before time.Time) error
}

// Usage reads the counters.
type Usage interface {
	HistoryCounters(ctx context.Context, now time.Time, busy func(store.UsageKey) bool) ([]usage.HistoryCounter, error)
	Values(ctx context.Context, keys []string) (map[string]int64, error)
}

type Service struct {
	st    Store
	usage Usage
	every time.Duration
	// pruned is the day old history was last deleted. Only Run uses it.
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
		now := time.Now()
		if err := s.Tick(ctx, now); err != nil && ctx.Err() == nil {
			slog.Warn("usage history: could not record; it is added at the next look", "err", err)
		}
		if day := now.UTC().Truncate(24 * time.Hour); day.After(s.pruned) {
			if err := s.st.PruneUsage(ctx, now.Add(-Keep)); err != nil && ctx.Err() == nil {
				slog.Warn("usage history: could not delete old history", "err", err)
			} else {
				s.pruned = day
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick adds what was used since the last look to the history. A reset calls
// it first too, so that the use a reset takes away is in the history.
//
// Each counter in Redis is followed on its own, by its key: it only grows
// within its window, so what was used since the last look is the counter
// minus what it held then, and a counter lower than that was reset and
// counts from 0. A counter the last look saw that is no longer among the
// counters (its window ended, its rule moved, or no token names its amount
// any more) is read on to the end of its window: its rule may be gone, but
// the rate limit service keeps it, and what it counted after the last look
// is added. Use in a window that ended goes to its last hour.
//
// When Redis cannot be read nothing is written and the cursors stay, so the
// usage is added at the next look that works.
func (s *Service) Tick(ctx context.Context, now time.Time) error {
	return s.st.RecordUsage(ctx, func(before store.Before) (store.Look, error) {
		// The history follows a tenant's tokens on a model while it has a
		// cursor of them.
		busy := map[store.UsageKey]bool{}
		for _, c := range before.Cursors {
			if !quotaKind(c.Kind) {
				busy[c.UsageKey] = true
			}
		}
		current, err := s.usage.HistoryCounters(ctx, now, func(k store.UsageKey) bool { return busy[k] })
		if err != nil {
			return store.Look{}, err
		}
		var look store.Look
		sum := newSums()
		seen := map[string]bool{}
		legacy := legacyBase(before.Legacy)
		// A token counter that holds 0 keeps a cursor while another of the
		// tenant's on the model does not.
		sent := map[store.UsageKey]bool{}
		for _, c := range current {
			if !quotaKind(c.Kind) && c.Value > 0 {
				sent[c.UsageKey] = true
			}
		}
		for _, c := range current {
			seen[c.Key] = true
			start := render.WindowStart(c.Window, now)
			last, ok := before.Cursors[c.Key]
			used := since(c.Value, last.Value)
			if !ok {
				used = legacy.take(c, start)
			}
			sum.add(c.UsageKey, c.Kind, c.Unit, now, used)
			// A quota's counter keeps its cursor at 0 too: what it counts
			// in the last moments of its window is read after the window.
			if quotaKind(c.Kind) || sent[c.UsageKey] {
				look.Cursors = append(look.Cursors, store.CounterCursor{Key: c.Key, UsageKey: c.UsageKey, Kind: c.Kind, Unit: c.Unit,
					Window: c.Window, WindowStart: start, Value: c.Value})
			}
		}
		var gone []store.CounterCursor
		var keys []string
		for key, c := range before.Cursors {
			if !seen[key] {
				gone, keys = append(gone, c), append(keys, key)
			}
		}
		if len(gone) > 0 {
			values, err := s.usage.Values(ctx, keys)
			if err != nil {
				return store.Look{}, err
			}
			for _, c := range gone {
				v, end, at := values[c.Key], render.WindowEnd(c.Window, c.WindowStart), now
				if !now.Before(end) {
					at = end.Add(-time.Second)
				}
				sum.add(c.UsageKey, c.Kind, c.Unit, at, since(v, c.Value))
				if now.Before(end) {
					c.Value = v
					look.Cursors = append(look.Cursors, c)
				}
			}
		}
		look.Usage, look.Tokens = sum.deltas()
		return look, nil
	})
}

// quotaKind reports whether a counter is a quota's, whose use goes to the
// usage history, rather than a token counter.
func quotaKind(kind string) bool { return kind == store.KindUsed || kind == store.KindBestEffort }

// since is what was used between two looks at a counter of one window.
func since(now, last int64) int64 {
	if now < last {
		return now // the counter was reset
	}
	return now - last
}

// legacy holds the cursors from before 0.30.0, one per tenant and model,
// which the first look after the upgrade starts from: what a tenant's
// counters of a kind held then, in the window that is still running, is
// taken off what they hold now.
type legacy map[legacyKey]int64

type legacyKey struct {
	store.UsageKey
	kind  string
	start time.Time
}

func legacyBase(cursors map[store.UsageKey]store.UsageCursor) legacy {
	out := legacy{}
	for k, c := range cursors {
		start := render.WindowStart(c.Window, c.WindowStart)
		out[legacyKey{k, store.KindUsed, start}] = c.Used
		out[legacyKey{k, store.KindBestEffort, start}] = c.BestEffort
	}
	return out
}

// take returns what a counter without a cursor counted since the last look:
// all it holds, less what a cursor from before 0.30.0 still accounts for.
func (l legacy) take(c usage.HistoryCounter, start time.Time) int64 {
	k := legacyKey{c.UsageKey, c.Kind, start}
	base := min(l[k], c.Value)
	l[k] -= base
	return c.Value - base
}

// sums adds the use of each tenant on each model up by hour.
type sums struct {
	usage  map[usageHour]*store.UsageDelta
	tokens map[tokenHour]*store.TokenDelta
	order  []any
}

type usageHour struct {
	store.UsageKey
	hour time.Time
	unit string
}

type tokenHour struct {
	store.UsageKey
	hour time.Time
}

func newSums() *sums {
	return &sums{usage: map[usageHour]*store.UsageDelta{}, tokens: map[tokenHour]*store.TokenDelta{}}
}

func (s *sums) add(key store.UsageKey, kind, unit string, at time.Time, n int64) {
	if n <= 0 {
		return
	}
	hour := at.UTC().Truncate(time.Hour)
	switch kind {
	case store.KindUsed, store.KindBestEffort:
		k := usageHour{key, hour, unit}
		d := s.usage[k]
		if d == nil {
			d = &store.UsageDelta{UsageKey: key, Hour: hour, Unit: unit}
			s.usage[k] = d
			s.order = append(s.order, d)
		}
		if kind == store.KindUsed {
			d.Used += n
		} else {
			d.BestEffort += n
		}
	default:
		k := tokenHour{key, hour}
		d := s.tokens[k]
		if d == nil {
			d = &store.TokenDelta{UsageKey: key, Hour: hour}
			s.tokens[k] = d
			s.order = append(s.order, d)
		}
		switch render.TokenKind(kind) {
		case render.TokenRequests:
			d.Requests += n
		case render.TokenInput:
			d.Input += n
		case render.TokenCached:
			d.Cached += n
		case render.TokenOutput:
			d.Output += n
		}
	}
}

// deltas returns the sums in the order they were first added.
func (s *sums) deltas() ([]store.UsageDelta, []store.TokenDelta) {
	var usage []store.UsageDelta
	var tokens []store.TokenDelta
	for _, d := range s.order {
		switch d := d.(type) {
		case *store.UsageDelta:
			usage = append(usage, *d)
		case *store.TokenDelta:
			tokens = append(tokens, *d)
		}
	}
	return usage, tokens
}
