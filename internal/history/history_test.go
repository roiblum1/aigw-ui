package history

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
	"aigw-ui/internal/usage"
)

// fake is Redis, which counters the hub knows of, and the history.
type fake struct {
	redis map[string]int64
	// counters returns the counters the hub knows of at a point in time,
	// without their values.
	counters func(now time.Time) []usage.HistoryCounter
	cursors  map[string]store.CounterCursor
	legacy   map[store.UsageKey]store.UsageCursor
	hours    map[time.Time][2]int64
	tokens   map[time.Time]store.TokenDelta
	down     error
}

var key = store.UsageKey{TenantID: "team-a", ModelID: "glm"}

func (f *fake) HistoryCounters(_ context.Context, now time.Time, _ func(store.UsageKey) bool) ([]usage.HistoryCounter, error) {
	if f.down != nil {
		return nil, f.down
	}
	out := f.counters(now)
	for i := range out {
		out[i].Value = f.redis[out[i].Key]
	}
	return out, nil
}

func (f *fake) Values(_ context.Context, keys []string) (map[string]int64, error) {
	if f.down != nil {
		return nil, f.down
	}
	out := map[string]int64{}
	for _, k := range keys {
		if v, ok := f.redis[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (f *fake) RecordUsage(_ context.Context, look func(store.Before) (store.Look, error)) error {
	found, err := look(store.Before{Cursors: f.cursors, Legacy: f.legacy})
	if err != nil {
		return err
	}
	for _, d := range found.Usage {
		h := f.hours[d.Hour]
		f.hours[d.Hour] = [2]int64{h[0] + d.Used, h[1] + d.BestEffort}
	}
	for _, d := range found.Tokens {
		h := f.tokens[d.Hour]
		h.Requests, h.Input, h.Cached, h.Output = h.Requests+d.Requests, h.Input+d.Input, h.Cached+d.Cached, h.Output+d.Output
		f.tokens[d.Hour] = h
	}
	f.cursors, f.legacy = map[string]store.CounterCursor{}, nil
	for _, c := range found.Cursors {
		f.cursors[c.Key] = c
	}
	return nil
}

func (f *fake) PruneUsage(context.Context, time.Time) error { return nil }

// quota is a tenant's quota of one window: a counter of its use and one of
// its best-effort use, named after the window's start.
func quota(window string) func(time.Time) []usage.HistoryCounter {
	return func(now time.Time) []usage.HistoryCounter {
		start := render.WindowStart(window, now).Unix()
		return []usage.HistoryCounter{
			{Key: fmt.Sprintf("used-%d", start), UsageKey: key, Kind: store.KindUsed, Unit: store.UnitCredits, Window: window},
			{Key: fmt.Sprintf("be-%d", start), UsageKey: key, Kind: store.KindBestEffort, Unit: store.UnitCredits, Window: window},
		}
	}
}

func setup(window string) (*fake, *Service) {
	f := &fake{redis: map[string]int64{}, counters: quota(window), cursors: map[string]store.CounterCursor{},
		hours: map[time.Time][2]int64{}, tokens: map[time.Time]store.TokenDelta{}}
	return f, New(f, f, time.Minute)
}

// set puts what a quota's counters of the window that began at start hold.
func (f *fake) set(start time.Time, used, bestEffort int64) {
	f.redis[fmt.Sprintf("used-%d", start.Unix())] = used
	f.redis[fmt.Sprintf("be-%d", start.Unix())] = bestEffort
}

func at(clock string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", "2026-10-10 "+clock)
	if err != nil {
		panic(err)
	}
	return t
}

func (f *fake) check(t *testing.T, want map[string][2]int64) {
	t.Helper()
	if len(f.hours) != len(want) {
		t.Errorf("hours = %v, want %v", f.hours, want)
	}
	for clock, w := range want {
		if got := f.hours[at(clock)]; got != w {
			t.Errorf("%s = %v, want %v", clock, got, w)
		}
	}
}

func tick(t *testing.T, s *Service, clock string) {
	t.Helper()
	if err := s.Tick(context.Background(), at(clock)); err != nil {
		t.Fatal(err)
	}
}

// A daily counter is spread over the hours in which it grew.
func TestTickAddsWhatWasUsedSince(t *testing.T) {
	f, s := setup("1d")
	day := at("00:00")
	for _, look := range []struct {
		clock string
		used  [2]int64
	}{{"09:10", [2]int64{100, 0}}, {"09:11", [2]int64{100, 0}}, {"09:50", [2]int64{250, 0}}, {"10:05", [2]int64{400, 30}}} {
		f.set(day, look.used[0], look.used[1])
		tick(t, s, look.clock)
	}
	f.check(t, map[string][2]int64{"09:00": {250, 0}, "10:00": {150, 30}})
}

// What was used between the last look and the end of a window is read from
// the window's counter after it ended, and belongs to its last hour.
func TestTickReadsTheEndOfAWindow(t *testing.T) {
	f, s := setup("1h")
	f.set(at("09:00"), 100, 0)
	tick(t, s, "09:59")
	f.set(at("09:00"), 130, 5)
	f.set(at("10:00"), 7, 0)
	tick(t, s, "10:00")
	f.check(t, map[string][2]int64{"09:00": {130, 5}, "10:00": {7, 0}})
	for k, c := range f.cursors {
		if !c.WindowStart.Equal(at("10:00")) {
			t.Errorf("the ended window keeps a cursor: %s", k)
		}
	}

	// The server was down over several windows: the counter of the window
	// it last saw is gone, and nothing is made up for it.
	f.redis = map[string]int64{}
	f.set(at("14:00"), 9, 0)
	tick(t, s, "14:30")
	f.check(t, map[string][2]int64{"09:00": {130, 5}, "10:00": {7, 0}, "14:00": {9, 0}})
}

// After a reset the counter starts at 0 again within the same window.
func TestTickAfterAReset(t *testing.T) {
	f, s := setup("1d")
	f.set(at("00:00"), 500, 0)
	tick(t, s, "09:00")
	f.set(at("00:00"), 40, 0)
	tick(t, s, "09:01")
	f.check(t, map[string][2]int64{"09:00": {540, 0}})
}

// Without Redis nothing is written, and the next look adds all of it.
func TestTickWithoutRedis(t *testing.T) {
	f, s := setup("1d")
	f.set(at("00:00"), 100, 0)
	tick(t, s, "09:00")
	f.down = errors.New("connection refused")
	f.set(at("00:00"), 300, 0)
	if err := s.Tick(context.Background(), at("09:01")); err == nil {
		t.Error("no error although Redis is down")
	}
	f.check(t, map[string][2]int64{"09:00": {100, 0}})
	f.down = nil
	tick(t, s, "10:01")
	f.check(t, map[string][2]int64{"09:00": {100, 0}, "10:00": {200, 0}})
}

// A counter whose rule moved within its window, because a new amount came
// before it, is read once more under its old name: what it counted after
// the last look is not lost (the lab, 2026-10-17: 99 recorded, 147 counted).
// Its new counter starts at 0.
func TestTickFollowsAMovedRule(t *testing.T) {
	f, s := setup("1d")
	f.redis["rule-5"] = 99
	f.counters = func(time.Time) []usage.HistoryCounter {
		return []usage.HistoryCounter{{Key: "rule-5", UsageKey: key, Kind: store.KindUsed, Unit: store.UnitCredits, Window: "1d"}}
	}
	tick(t, s, "09:10")
	f.redis["rule-5"] = 147
	f.redis["rule-6"] = 0
	f.counters = func(time.Time) []usage.HistoryCounter {
		return []usage.HistoryCounter{{Key: "rule-6", UsageKey: key, Kind: store.KindUsed, Unit: store.UnitCredits, Window: "1d"}}
	}
	tick(t, s, "09:11")
	f.redis["rule-6"] = 20
	tick(t, s, "09:12")
	f.check(t, map[string][2]int64{"09:00": {167, 0}})
}

// A tenant whose amount changed is counted under both amounts while a token
// with the old one is still in use, and the history adds both.
func TestTickAddsEveryAmountOfATenant(t *testing.T) {
	f, s := setup("1h")
	f.counters = func(time.Time) []usage.HistoryCounter {
		return []usage.HistoryCounter{
			{Key: "9-1h", UsageKey: key, Kind: store.KindUsed, Unit: store.UnitCredits, Window: "1h"},
			{Key: "12-1h", UsageKey: key, Kind: store.KindUsed, Unit: store.UnitCredits, Window: "1h"},
		}
	}
	f.redis["9-1h"], f.redis["12-1h"] = 3, 1
	tick(t, s, "09:10")
	f.redis["12-1h"] = 2
	tick(t, s, "09:20")
	f.check(t, map[string][2]int64{"09:00": {5, 0}})
}

// The token counters go to the token history, by hour.
func TestTickRecordsTokens(t *testing.T) {
	f, s := setup("1h")
	f.counters = func(now time.Time) []usage.HistoryCounter {
		var out []usage.HistoryCounter
		for _, kind := range render.TokenKinds {
			out = append(out, usage.HistoryCounter{Key: fmt.Sprintf("%s-%d", kind, render.WindowStart("1h", now).Unix()),
				UsageKey: key, Kind: string(kind), Window: "1h"})
		}
		return out
	}
	put := func(start time.Time, requests, input, cached, output int64) {
		for kind, v := range map[render.TokenKind]int64{render.TokenRequests: requests, render.TokenInput: input, render.TokenCached: cached, render.TokenOutput: output} {
			f.redis[fmt.Sprintf("%s-%d", kind, start.Unix())] = v
		}
	}
	put(at("09:00"), 2, 3000, 2000, 100)
	tick(t, s, "09:30")
	// An answer that ended after the hour is counted in the next one.
	put(at("09:00"), 3, 3500, 2000, 120)
	put(at("10:00"), 0, 0, 0, 40)
	tick(t, s, "10:01")
	want := map[time.Time]store.TokenDelta{
		at("09:00"): {Requests: 3, Input: 3500, Cached: 2000, Output: 120},
		at("10:00"): {Output: 40},
	}
	for hour, w := range want {
		if got := f.tokens[hour]; got.Requests != w.Requests || got.Input != w.Input || got.Cached != w.Cached || got.Output != w.Output {
			t.Errorf("%s = %+v, want %+v", hour.Format("15:04"), got, w)
		}
	}
	if len(f.hours) != 0 {
		t.Errorf("tokens in the usage history: %v", f.hours)
	}
}

// The first look after the upgrade takes what a cursor from before 0.30.0
// held for the running window off its counters, and then forgets it.
func TestTickFromACursorOfBefore(t *testing.T) {
	f, s := setup("1d")
	f.legacy = map[store.UsageKey]store.UsageCursor{key: {UsageKey: key, Window: "1d", WindowStart: at("00:00"), Used: 100, BestEffort: 10}}
	f.set(at("00:00"), 130, 10)
	tick(t, s, "09:00")
	f.check(t, map[string][2]int64{"09:00": {30, 0}})
	if f.legacy != nil {
		t.Error("the cursor from before was kept")
	}

	// One of a window that is over accounts for nothing now.
	f, s = setup("1h")
	f.legacy = map[store.UsageKey]store.UsageCursor{key: {UsageKey: key, Window: "1h", WindowStart: at("08:00"), Used: 100}}
	f.set(at("09:00"), 30, 0)
	tick(t, s, "09:10")
	f.check(t, map[string][2]int64{"09:00": {30, 0}})
}
