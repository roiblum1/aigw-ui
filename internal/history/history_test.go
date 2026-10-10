package history

import (
	"context"
	"errors"
	"testing"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
	"aigw-ui/internal/usage"
)

// fake holds the counters by window start, like Redis, and the history.
type fake struct {
	window   string
	counters map[time.Time][2]int64
	cursors  map[store.UsageKey]store.UsageCursor
	hours    map[time.Time][2]int64
	redis    error
}

var key = store.UsageKey{TenantID: "team-a", ModelID: "glm"}

func (f *fake) ReportAt(_ context.Context, _ string, at time.Time) (usage.Report, error) {
	if f.redis != nil {
		return usage.Report{}, f.redis
	}
	c := f.counters[render.WindowStart(f.window, at)]
	return usage.Report{Quotas: []usage.Quota{{TenantID: key.TenantID, ModelID: key.ModelID, Window: f.window, Unit: store.UnitCredits, Used: c[0], OverageUsed: c[1]}}}, nil
}

func (f *fake) RecordUsage(_ context.Context, look func(map[store.UsageKey]store.UsageCursor) ([]store.UsageDelta, []store.UsageCursor, error)) error {
	deltas, next, err := look(f.cursors)
	if err != nil {
		return err
	}
	for _, d := range deltas {
		h := f.hours[d.Hour.UTC().Truncate(time.Hour)]
		f.hours[d.Hour.UTC().Truncate(time.Hour)] = [2]int64{h[0] + d.Used, h[1] + d.BestEffort}
	}
	for _, c := range next {
		f.cursors[c.UsageKey] = c
	}
	return nil
}

func (f *fake) PruneUsage(context.Context, time.Time) error { return nil }

func setup(window string) (*fake, *Service) {
	f := &fake{window: window, counters: map[time.Time][2]int64{}, cursors: map[store.UsageKey]store.UsageCursor{}, hours: map[time.Time][2]int64{}}
	return f, New(f, f, time.Minute)
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

// A daily counter is spread over the hours in which it grew.
func TestTickAddsWhatWasUsedSince(t *testing.T) {
	f, s := setup("1d")
	day := at("00:00")
	for _, look := range []struct {
		clock string
		used  [2]int64
	}{{"09:10", [2]int64{100, 0}}, {"09:11", [2]int64{100, 0}}, {"09:50", [2]int64{250, 0}}, {"10:05", [2]int64{400, 30}}} {
		f.counters[day] = look.used
		if err := s.Tick(context.Background(), at(look.clock)); err != nil {
			t.Fatal(err)
		}
	}
	f.check(t, map[string][2]int64{"09:00": {250, 0}, "10:00": {150, 30}})
}

// What was used between the last look and the end of a window is read from
// the window's counter after it ended, and belongs to its last hour.
func TestTickReadsTheEndOfAWindow(t *testing.T) {
	f, s := setup("1h")
	f.counters[at("09:00")] = [2]int64{100, 0}
	s.Tick(context.Background(), at("09:59"))
	f.counters[at("09:00")] = [2]int64{130, 5}
	f.counters[at("10:00")] = [2]int64{7, 0}
	s.Tick(context.Background(), at("10:00"))
	f.check(t, map[string][2]int64{"09:00": {130, 5}, "10:00": {7, 0}})

	// The server was down over several windows: the counter of the window
	// it last saw is gone, and nothing is made up for it.
	f.counters = map[time.Time][2]int64{at("14:00"): {9, 0}}
	s.Tick(context.Background(), at("14:30"))
	f.check(t, map[string][2]int64{"09:00": {130, 5}, "10:00": {7, 0}, "14:00": {9, 0}})
}

// After a reset the counter starts at 0 again within the same window.
func TestTickAfterAReset(t *testing.T) {
	f, s := setup("1d")
	f.counters[at("00:00")] = [2]int64{500, 0}
	s.Tick(context.Background(), at("09:00"))
	f.counters[at("00:00")] = [2]int64{40, 0}
	s.Tick(context.Background(), at("09:01"))
	f.check(t, map[string][2]int64{"09:00": {540, 0}})
}

// Without Redis nothing is written, and the next look adds all of it.
func TestTickWithoutRedis(t *testing.T) {
	f, s := setup("1d")
	f.counters[at("00:00")] = [2]int64{100, 0}
	s.Tick(context.Background(), at("09:00"))
	f.redis = errors.New("connection refused")
	f.counters[at("00:00")] = [2]int64{300, 0}
	if err := s.Tick(context.Background(), at("09:01")); err == nil {
		t.Error("no error although Redis is down")
	}
	f.check(t, map[string][2]int64{"09:00": {100, 0}})
	f.redis = nil
	s.Tick(context.Background(), at("10:01"))
	f.check(t, map[string][2]int64{"09:00": {100, 0}, "10:00": {200, 0}})
}
