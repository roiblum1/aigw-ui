package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string, headers ...string) *http.Response {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	var rd io.Reader = res.Body
	if res.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		rd = gz
	}
	b, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCompress(t *testing.T) {
	big := map[string]string{"tenants": strings.Repeat(`{"slug":"team-a","enabled":true},`, 200)}
	h := compress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			writeJSON(w, http.StatusOK, big)
		case "/secret":
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusOK, big)
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/bytes":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte(big["tenants"]))
		}
	}))
	want := body(t, get(t, h, "/json"))

	for _, c := range []struct {
		name, path, accept string
		gzipped            bool
	}{
		{"json, gzip taken", "/json", "gzip, deflate, br", true},
		{"json, gzip with a weight", "/json", "br;q=1.0, gzip;q=0.8", true},
		{"json, no Accept-Encoding", "/json", "", false},
		{"json, gzip refused", "/json", "gzip;q=0, br", false},
		{"json, gzip refused as 0.0", "/json", "gzip; q=0.0", false},
		{"an answer with a secret", "/secret", "gzip", false},
		{"no content", "/empty", "gzip", false},
		{"bytes that do not shrink", "/bytes", "gzip", false},
	} {
		res := get(t, h, c.path, "Accept-Encoding", c.accept)
		if got := res.Header.Get("Content-Encoding") == "gzip"; got != c.gzipped {
			t.Errorf("%s: gzipped = %v, want %v", c.name, got, c.gzipped)
		}
		if !strings.Contains(res.Header.Get("Vary"), "Accept-Encoding") {
			t.Errorf("%s: Vary = %q, want Accept-Encoding in it", c.name, res.Header.Get("Vary"))
		}
		if c.path == "/json" && body(t, res) != want {
			t.Errorf("%s: the body changed", c.name)
		}
	}
}

func TestUIFilesCached(t *testing.T) {
	dir := t.TempDir()
	index := "<!doctype html><script src=/assets/index-abc123.js></script>"
	script := strings.Repeat("console.log('aigw');\n", 200)
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "index-abc123.js"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	h := compress(http.HandlerFunc((&Server{uiDir: dir}).ui))

	res := get(t, h, "/assets/index-abc123.js", "Accept-Encoding", "gzip")
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("a built script: Cache-Control = %q, want it kept for good", cc)
	}
	if res.Header.Get("Content-Encoding") != "gzip" || body(t, res) != script {
		t.Errorf("a built script: not gzipped, or changed")
	}

	// A part of a file is of the file as stored.
	res = get(t, h, "/assets/index-abc123.js", "Accept-Encoding", "gzip", "Range", "bytes=0-9")
	if res.StatusCode != http.StatusPartialContent || res.Header.Get("Content-Encoding") != "" || body(t, res) != script[:10] {
		t.Errorf("a range: status %d, encoding %q", res.StatusCode, res.Header.Get("Content-Encoding"))
	}

	for _, path := range []string{"/", "/tenants", "/assets/gone-old123.js"} {
		res := get(t, h, path, "Accept-Encoding", "gzip")
		if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache: it serves index.html, which names the current scripts", path, cc)
		}
		if b := body(t, res); b != index {
			t.Errorf("%s: served %q, want index.html", path, b)
		}
	}
}
