package clients

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"testing"
)

// Every file the hub serves is in the binary, as it is in the repository.
func TestFiles(t *testing.T) {
	for _, f := range Files {
		_, data, ok := Read(f.Name)
		want, err := os.ReadFile(f.Path)
		if !ok || err != nil || !bytes.Equal(data, want) {
			t.Fatalf("%s: %v %v", f.Name, ok, err)
		}
	}
	if _, _, ok := Read("../go.mod"); ok {
		t.Fatal("read a file that is not listed")
	}
}

// The archive is a marketplace Claude Code takes: its manifest at the root
// names the plugin in aigw-usage/, which holds the plugin's files without
// its tests, executable scripts included. It is the same bytes every time.
func TestArchive(t *testing.T) {
	data, err := Archive()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := build(); !bytes.Equal(data, again) {
		t.Fatal("the archive changes from one build to the next")
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]int64{}
	var market []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = h.Mode
		if h.Name == ".claude-plugin/marketplace.json" {
			market, _ = io.ReadAll(tr)
		}
	}
	want := map[string]int64{
		".claude-plugin/marketplace.json":       0o644,
		"aigw-usage/.claude-plugin/plugin.json": 0o644,
		"aigw-usage/commands/setup.md":          0o644,
		"aigw-usage/hooks/hooks.json":           0o644,
		"aigw-usage/scripts/hook.mjs":           0o755,
		"aigw-usage/scripts/statusline.mjs":     0o755,
		"aigw-usage/scripts/store.mjs":          0o755,
		"aigw-usage/scripts/usage.mjs":          0o755,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %v", got)
	}
	var m struct {
		Name    string `json:"name"`
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(market, &m); err != nil || m.Name != "aigw" || len(m.Plugins) != 1 ||
		m.Plugins[0].Name != "aigw-usage" || m.Plugins[0].Source != "./aigw-usage" {
		t.Fatalf("marketplace = %s, %v", market, err)
	}
}
