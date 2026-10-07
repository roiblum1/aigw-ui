package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestListModels(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Write([]byte(`{"object":"list","data":[
			{"id":"judge","object":"model"},{"id":"glm-5.3","object":"model"},
			{"id":"publishers/llm-glm53/models/glm-5.3","object":"model"},{"id":"judge","object":"model"}]}`))
	}))
	defer srv.Close()

	got, err := ListModels(context.Background(), srv.URL+"/", "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"glm-5.3", "judge", "publishers/llm-glm53/models/glm-5.3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if gotPath != "/v1/models" || gotAuth != "Bearer sk-test" {
		t.Errorf("requested %q with auth %q", gotPath, gotAuth)
	}
}

func TestListModelsErrors(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"unauthorized": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		"not json":     func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>")) },
	} {
		srv := httptest.NewServer(handler)
		if _, err := ListModels(context.Background(), srv.URL, ""); err == nil {
			t.Errorf("%s: want an error", name)
		}
		srv.Close()
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://192.168.1.9":        "http://192.168.1.9",
		" https://gw.example:8443/": "https://gw.example:8443",
	} {
		if got, err := NormalizeURL(in); err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"192.168.1.9", "ftp://x", "http://x/v1/models", "http://u:p@x", "http://x?a=1", "http://"} {
		if _, err := NormalizeURL(in); err == nil {
			t.Errorf("NormalizeURL(%q): want an error", in)
		}
	}
}
