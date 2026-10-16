package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// lateAfter is how long after its day a set of prices may start before the
// task line says it was late.
const lateAfter = 5 * time.Minute

// RunPrices starts using a model's new prices when their day begins, until
// ctx is cancelled. It also looks once at the start, for prices whose day
// began while the server was not running.
func (s *Syncer) RunPrices(ctx context.Context) {
	for {
		if _, err := s.ApplyPrices(ctx); err != nil && ctx.Err() == nil {
			slog.Error("apply prices", "err", err)
		}
		timer := time.NewTimer(time.Until(NextDay(time.Now())))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// NextDay returns the next 00:00 UTC after t. Every quota window starts
// anew there, so prices that change at that moment never share a counter
// with the ones before.
func NextDay(t time.Time) time.Time {
	return t.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
}

// ApplyPrices starts using every set of prices whose day has come, and
// returns how many. Each gets a line in the task log, and the clusters are
// synced once.
func (s *Syncer) ApplyPrices(ctx context.Context) (int, error) {
	applied, err := s.st.ApplyDuePrices(ctx)
	if err != nil || len(applied) == 0 {
		return 0, err
	}
	lines := make([]string, 0, len(applied))
	for _, a := range applied {
		lines = append(lines, priceLine(a, time.Now()))
	}
	s.Changed(ctx, "model.prices", strings.Join(lines, " "))
	return len(applied), nil
}

// priceLine describes prices that have just started, for the task log.
func priceLine(a store.AppliedPrice, now time.Time) string {
	p := a.Price
	line := fmt.Sprintf("%s now costs %s for a million input tokens, %s cached and %s output.",
		a.ModelName, render.Dollars(p.Input), render.Dollars(p.Cached), render.Dollars(p.Output))
	if a.First {
		line += " It is counted in money from now on: its limits were converted at the input price, and it is in dry-run, so nobody is refused until that is switched off."
	}
	if late := now.Sub(p.EffectiveAt); late > lateAfter {
		line += fmt.Sprintf(" The prices were due %s ago, so the counters of the running windows hold amounts at the old and the new prices.", late.Round(time.Minute))
	}
	return line
}
