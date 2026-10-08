package api

import (
	"net/http/httptest"
	"testing"
)

func TestClip(t *testing.T) {
	for in, want := range map[string]string{
		"alice":        "alice",
		"a\nb\tc\x7f":  "abc",
		"abcdefghijkl": "abcdefghij",
		"abcdefghiבzz": "abcdefghi", // a cut inside a character drops it
	} {
		if got := clip(in, 10); got != want {
			t.Errorf("clip(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOnBehalfOf(t *testing.T) {
	for in, want := range map[string]string{
		"alice":             "alice",
		"roi%20%D7%91":      "roi ב",
		"50%":               "50%", // not valid percent-encoding: taken as it is
		"portal/team-a+bob": "portal/team-a+bob",
	} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set(OnBehalfOfHeader, in)
		if got := onBehalfOf(r); got != want {
			t.Errorf("onBehalfOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStatusWriterKeepsFirstStatus(t *testing.T) {
	w := &statusWriter{ResponseWriter: httptest.NewRecorder()}
	w.Write([]byte("ok"))
	if w.status != 200 {
		t.Errorf("status after a bare Write = %d, want 200", w.status)
	}
	w = &statusWriter{ResponseWriter: httptest.NewRecorder()}
	w.WriteHeader(409)
	w.WriteHeader(500)
	if w.status != 409 {
		t.Errorf("status = %d, want the first one, 409", w.status)
	}
}
