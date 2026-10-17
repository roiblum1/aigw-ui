package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

// An admin request's answer says how long the hub took, gzipped or not,
// so a client tells the hub's time from its link's; a request that is not
// let in learns nothing.
func TestServerTiming(t *testing.T) {
	h := (&Server{uiDir: t.TempDir(), adminToken: "admin-token-for-the-test"}).Handler()
	for _, tc := range []struct {
		token, encoding string
		want            bool
	}{
		{"admin-token-for-the-test", "", true},
		{"admin-token-for-the-test", "gzip", true},
		{"wrong", "", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/no-such-route", nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Accept-Encoding", tc.encoding)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		got := w.Header().Get("Server-Timing")
		if ok := strings.HasPrefix(got, "app;dur="); ok != tc.want {
			t.Errorf("%+v: Server-Timing %q (status %d)", tc, got, w.Code)
		}
	}
}
