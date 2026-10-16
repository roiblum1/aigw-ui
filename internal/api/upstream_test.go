package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The upstream documentation as cmd/upstream-docs writes it.
func upstreamDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"index.json":                 `{"generated":"2026-10-16","projects":[{"id":"eg","name":"Envoy Gateway","pages":[{"path":"site/v1.9/rate-limit","title":"Global Rate Limit"},{"path":"site/v1.9/quickstart","title":"Quickstart"}]}]}`,
		"eg/site/v1.9/rate-limit.md": "# Global Rate Limit\n\nThe ratelimit service counts in Redis.\n",
		"eg/site/v1.9/quickstart.md": "# Quickstart\n\nInstall it. Then add a global rate limit with Redis, and Redis again.\n",
		"eg/img/a.svg":               `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"eg/LICENSE":                 "Apache License",
	}
	for name, text := range files {
		file := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(file), 0o755)
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The files are public, served with their type and a policy under which
// nothing runs, and nothing outside the folder is reachable.
func TestUpstreamDocsFiles(t *testing.T) {
	h := (&Server{uiDir: t.TempDir()}).WithUpstreamDocs(upstreamDir(t)).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	w := get("/upstream-docs/eg/site/v1.9/rate-limit.md")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || !strings.Contains(w.Body.String(), "Redis") {
		t.Fatalf("page: %d %s %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	w = get("/upstream-docs/eg/img/a.svg")
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("svg: %d %v", w.Code, w.Header())
	}
	if w = get("/upstream-docs/eg/LICENSE"); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("license: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w = get("/upstream-docs/index.json"); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("index: %d", w.Code)
	}
	for _, path := range []string{"/upstream-docs/eg", "/upstream-docs/", "/upstream-docs/eg/missing.md", "/upstream-docs/../go.mod", "/upstream-docs/%2e%2e/%2e%2e/etc/passwd"} {
		if w = get(path); w.Code == http.StatusOK {
			t.Errorf("%s: %d %q", path, w.Code, w.Body.String())
		}
	}
	// Without the folder there is nothing.
	h = (&Server{uiDir: t.TempDir()}).Handler()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/upstream-docs/index.json", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("no folder: %d", w.Code)
	}
}

// A search finds the guides and the upstream pages, the ones with the text
// in their title first, each with the line it is on.
func TestSearchDocs(t *testing.T) {
	s := (&Server{}).WithUpstreamDocs(upstreamDir(t))
	search := func(q string) (int, []searchHit) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/docs/search?q="+q, nil)
		w := httptest.NewRecorder()
		s.searchDocs(w, r)
		var hits []searchHit
		json.Unmarshal(w.Body.Bytes(), &hits)
		return w.Code, hits
	}
	code, hits := search("global+rate+limit")
	if code != http.StatusOK || len(hits) < 2 {
		t.Fatalf("%d %+v", code, hits)
	}
	if hits[0].Name != "upstream/eg/site/v1.9/rate-limit" || hits[0].Group != "Envoy Gateway" {
		t.Fatalf("the page with it in its title is not first: %+v", hits[0])
	}
	var quick searchHit
	for _, h := range hits {
		if h.Name == "upstream/eg/site/v1.9/quickstart" {
			quick = h
		}
	}
	if quick.Snippet != "Install it. Then add a global rate limit with Redis, and Redis again." {
		t.Fatalf("snippet %q", quick.Snippet)
	}
	// The guides are searched too.
	if _, hits = search("budget+across+models"); len(hits) == 0 || strings.HasPrefix(hits[0].Name, "upstream/") {
		t.Fatalf("guides: %+v", hits)
	}
	if code, _ = search("a"); code != http.StatusBadRequest {
		t.Fatalf("one character: %d", code)
	}
}

func TestSnippet(t *testing.T) {
	long := strings.Repeat("é", 120) + " needle " + strings.Repeat("ü", 120)
	got := snippet(long, strings.ToLower(long), "needle")
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") || !strings.Contains(got, "needle") || !json.Valid([]byte(`"`+got+`"`)) {
		t.Fatalf("%q", got)
	}
}
