package api

import (
	"net/http/httptest"
	"testing"
)

func TestQueryLimit(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
		ok    bool
	}{
		{"", 50, true},
		{"?limit=1", 1, true},
		{"?limit=500", 500, true},
		{"?limit=0", 0, false},
		{"?limit=501", 0, false},
		{"?limit=-3", 0, false},
		{"?limit=many", 0, false},
	} {
		got, err := queryLimit(httptest.NewRequest("GET", "/api/v1/tasks"+tc.query, nil), 50)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("%q: limit = %d, %v, want %d and ok = %v", tc.query, got, err, tc.want, tc.ok)
		}
	}
}
