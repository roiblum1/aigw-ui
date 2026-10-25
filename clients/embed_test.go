package clients

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"strings"
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

// Windows PowerShell 5.1 reads a script file without a byte order mark in
// the system's code page, so a character past ASCII in a .ps1 would turn
// into another one on some machines and could break the script.
func TestPowerShellFilesAreASCII(t *testing.T) {
	n := 0
	for _, f := range Files {
		if path.Ext(f.Name) != ".ps1" {
			continue
		}
		n++
		_, data, _ := Read(f.Name)
		for i, b := range data {
			if b > 0x7f {
				t.Fatalf("%s: byte %d is 0x%x, not ASCII", f.Path, i, b)
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d .ps1 files are served, want 2", n)
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

// The version is the files' and the archive's: the same on every call, 12
// hex digits, and another one for any other bytes.
func TestVersion(t *testing.T) {
	v := Version()
	if len(v) != 12 || strings.Trim(v, "0123456789abcdef") != "" || Version() != v {
		t.Fatalf("version = %q", v)
	}
	h := sha256.New()
	for _, f := range Files {
		data, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", f.Name, len(data))
		h.Write(data)
	}
	data, _ := Archive()
	fmt.Fprintf(h, "%s\x00%d\x00", PluginArchive, len(data))
	h.Write(data)
	if want := hex.EncodeToString(h.Sum(nil))[:12]; v != want {
		t.Fatalf("version = %s, want %s", v, want)
	}
	h.Write([]byte("x"))
	if hex.EncodeToString(h.Sum(nil))[:12] == v {
		t.Fatal("other bytes give the same version")
	}
}
