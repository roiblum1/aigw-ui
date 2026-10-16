package api

import (
	"net/http"
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

// Every answer, the UI's and the API's, carries the browser headers.
func TestSecurityHeaders(t *testing.T) {
	h := (&Server{uiDir: t.TempDir()}).Handler()
	for _, path := range []string{"/healthz", "/", "/api/v1/tenants"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		for name, want := range map[string]string{
			"X-Content-Type-Options":  "nosniff",
			"X-Frame-Options":         "SAMEORIGIN",
			"Content-Security-Policy": "frame-ancestors 'self'",
			"Referrer-Policy":         "no-referrer",
		} {
			if got := w.Header().Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", path, name, got, want)
			}
		}
	}
}
