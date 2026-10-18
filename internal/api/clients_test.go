package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The tools are served without a sign-in, as they are in the repository,
// and asked for again every time; any other name is 404.
func TestClientFiles(t *testing.T) {
	h := (&Server{}).Handler()
	get := func(name string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/clients/"+name, nil))
		return w
	}
	want, err := os.ReadFile("../../clients/aigw-token/aigw-token")
	if err != nil {
		t.Fatal(err)
	}
	if w := get("aigw-token"); w.Code != http.StatusOK || w.Body.String() != string(want) || w.Header().Get("Cache-Control") != "no-cache" ||
		!strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("aigw-token: %d %v", w.Code, w.Header())
	}
	if w := get("install.sh"); w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "#!/bin/sh") {
		t.Fatalf("install.sh: %d", w.Code)
	}
	if w := get("claude-code.tar.gz"); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/gzip" || w.Body.Len() < 1000 {
		t.Fatalf("plugin: %d %v %d", w.Code, w.Header(), w.Body.Len())
	}
	if w := get("embed.go"); w.Code != http.StatusNotFound {
		t.Fatalf("embed.go: %d", w.Code)
	}
}
