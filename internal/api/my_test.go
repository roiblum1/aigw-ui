package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestHistoryFrom(t *testing.T) {
	now := time.Date(2026, 10, 10, 15, 30, 0, 0, time.UTC)
	for query, want := range map[string]string{
		"":                   "day 2026-09-11",
		"?days=1":            "day 2026-10-10",
		"?step=hour":         "hour 2026-10-09",
		"?step=hour&days=31": "hour 2026-09-10",
		"?days=400":          "day 2025-09-06",
	} {
		step, from, err := historyFrom(httptest.NewRequest("GET", "/api/v1/usage/history"+query, nil), now)
		if got := step + " " + from.Format("2006-01-02"); err != nil || got != want {
			t.Errorf("%q: %s, %v, want %s", query, got, err, want)
		}
	}
	for _, query := range []string{"?step=week", "?days=0", "?days=401", "?step=hour&days=32", "?days=x"} {
		if _, _, err := historyFrom(httptest.NewRequest("GET", "/api/v1/usage/history"+query, nil), now); err == nil {
			t.Errorf("%q: accepted", query)
		}
	}
}

func TestKeyFailures(t *testing.T) {
	var f keyFailures
	now := time.Unix(1791400000, 0)
	for range maxKeyFailures {
		if f.blocked("10.0.0.1", now) {
			t.Fatal("blocked before the limit")
		}
		f.add("10.0.0.1", now)
	}
	if !f.blocked("10.0.0.1", now) || f.blocked("10.0.0.2", now) {
		t.Error("want the one address blocked and no other")
	}
	if f.blocked("10.0.0.1", now.Add(time.Minute)) {
		t.Error("still blocked in the next minute")
	}
}

// The address is the hop the platform's router added, not one a client sent.
func TestClientAddr(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/my/usage", nil)
	r.RemoteAddr = "10.128.0.9:41000"
	if got := clientAddr(r); got != "10.128.0.9" {
		t.Errorf("without the header: %s", got)
	}
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.7")
	if got := clientAddr(r); got != "203.0.113.7" {
		t.Errorf("with the header: %s", got)
	}
}
