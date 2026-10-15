package capacity

import (
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	jlm, _ := time.LoadLocation("Asia/Jerusalem")
	w, err := ParseWindow("Fri 22:00 - Sat 06:00 Asia/Jerusalem")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-16 is a Friday.
	for _, tc := range []struct {
		at   time.Time
		open bool
	}{
		{time.Date(2026, 10, 16, 21, 59, 0, 0, jlm), false},
		{time.Date(2026, 10, 16, 22, 0, 0, 0, jlm), true},
		{time.Date(2026, 10, 17, 5, 59, 0, 0, jlm), true},
		{time.Date(2026, 10, 17, 6, 0, 0, 0, jlm), false},
		{time.Date(2026, 10, 16, 19, 30, 0, 0, time.UTC), true}, // 22:30 in Jerusalem
	} {
		if got := w.Open(tc.at); got != tc.open {
			t.Errorf("%v: open = %v", tc.at, got)
		}
	}
	if next := w.Next(time.Date(2026, 10, 14, 12, 0, 0, 0, jlm)); !next.Equal(time.Date(2026, 10, 16, 22, 0, 0, 0, jlm)) {
		t.Errorf("next = %v", next)
	}

	// Over the end of the week, and every day over midnight.
	sun, _ := ParseWindow("Sat 23:00 - Sun 02:00 UTC")
	if !sun.Open(time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)) || sun.Open(time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)) {
		t.Error("week wrap")
	}
	nightly, _ := ParseWindow("23:00 - 05:00 UTC")
	if !nightly.Open(time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)) || nightly.Open(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)) {
		t.Error("daily wrap")
	}
	if next := nightly.Next(time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)); !next.Equal(time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC)) {
		t.Errorf("nightly next = %v", next)
	}

	if w, err := ParseWindow(""); w != nil || err != nil {
		t.Error("empty is no window")
	}
	for _, bad := range []string{"Fri 22:00", "Fri 22:00 - Sat 06:00", "Fri 22:00 - Sat 06:00 Mars/Base", "Fri 22:00 - 06:00 UTC", "Xyz 1:00 - Sat 2:00 UTC", "10:00 - 10:00 UTC"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// An hour overlaps the window when the window is open at any time in it.
func TestWindowOverlaps(t *testing.T) {
	w, err := ParseWindow("01:30 - 02:00 UTC")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h int) time.Time { return time.Date(2026, 10, 15, h, 0, 0, 0, time.UTC) }
	if !w.Overlaps(at(1), at(2)) || w.Overlaps(at(2), at(3)) || w.Overlaps(at(0), at(1)) {
		t.Fatal("overlaps")
	}
}
