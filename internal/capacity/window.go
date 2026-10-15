package capacity

import (
	"fmt"
	"strings"
	"time"
	// The image may have no time zone database; the window names a zone.
	_ "time/tzdata"
)

// Window is when the hub may measure by itself: once a week, such as
// "Fri 22:00 - Sat 06:00 Asia/Jerusalem", or every day, such as
// "01:00 - 05:00 UTC". It may run past midnight or the end of the week.
type Window struct {
	text  string
	daily bool
	// start and end are minutes from Sunday 00:00, or from midnight for a
	// daily window, in loc.
	start, end int
	loc        *time.Location
}

var weekdays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// ParseWindow reads a window; "" is none, and gives nil.
func ParseWindow(text string) (*Window, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	bad := fmt.Errorf("capacity window %q: write it as \"Fri 22:00 - Sat 06:00 Asia/Jerusalem\" or \"01:00 - 05:00 UTC\"", text)
	from, rest, ok := strings.Cut(text, "-")
	if !ok {
		return nil, bad
	}
	to := strings.Fields(rest)
	if len(to) < 2 {
		return nil, bad
	}
	loc, err := time.LoadLocation(to[len(to)-1])
	if err != nil {
		return nil, fmt.Errorf("capacity window %q: unknown time zone %q", text, to[len(to)-1])
	}
	w := &Window{text: text, loc: loc}
	start, startDaily, err := minutes(strings.Fields(from))
	if err != nil {
		return nil, bad
	}
	end, endDaily, err := minutes(to[:len(to)-1])
	if err != nil || startDaily != endDaily || start == end {
		return nil, bad
	}
	w.start, w.end, w.daily = start, end, startDaily
	return w, nil
}

// minutes reads "Fri 22:00" as minutes from Sunday 00:00, or "22:00" as
// minutes from midnight, and says which.
func minutes(fields []string) (int, bool, error) {
	day := 0
	daily := len(fields) == 1
	switch len(fields) {
	case 1:
	case 2:
		d, ok := weekdays[strings.ToLower(fields[0])[:min(3, len(fields[0]))]]
		if !ok {
			return 0, false, fmt.Errorf("day %q", fields[0])
		}
		day = d
	default:
		return 0, false, fmt.Errorf("fields %v", fields)
	}
	t, err := time.Parse("15:04", fields[len(fields)-1])
	if err != nil {
		return 0, false, err
	}
	return day*24*60 + t.Hour()*60 + t.Minute(), daily, nil
}

func (w *Window) String() string { return w.text }

// period is the length of the cycle the window repeats in, in minutes.
func (w *Window) period() int {
	if w.daily {
		return 24 * 60
	}
	return 7 * 24 * 60
}

// at is how far into the cycle t is, in minutes.
func (w *Window) at(t time.Time) int {
	t = t.In(w.loc)
	m := t.Hour()*60 + t.Minute()
	if !w.daily {
		m += int(t.Weekday()) * 24 * 60
	}
	return m
}

// Open says whether t is inside the window.
func (w *Window) Open(t time.Time) bool {
	m := w.at(t)
	if w.start < w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end // past the end of the cycle
}

// Next is when the window opens next after t, or t when it is open.
func (w *Window) Next(t time.Time) time.Time {
	if w.Open(t) {
		return t
	}
	wait := (w.start - w.at(t) + w.period()) % w.period()
	return t.In(w.loc).Truncate(time.Minute).Add(time.Duration(wait) * time.Minute)
}
