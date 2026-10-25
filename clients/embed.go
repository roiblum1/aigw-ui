// Package clients holds the tools for the people who use the models. The
// hub serves them under /clients/, so that a machine in a network with no
// internet installs them from the hub and not from GitHub.
package clients

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"
)

//go:embed aigw-token/aigw-token aigw-token/aigw-token.ps1 install/install.sh install/install.ps1 omp/aigw-usage.ts
var files embed.FS

// The Claude Code plugin, with its hidden .claude-plugin directory.
//
//go:embed all:claude-code
var plugin embed.FS

// A file the hub serves: its name under /clients/, where it is in this
// directory, and its content type.
type File struct {
	Name, Path, Type string
}

// Files are the files served as they are. The .ps1 files are Windows'
// installer and aigw-token: text, so that PowerShell's irm reads them as a
// script and not as bytes.
var Files = []File{
	{"aigw-token", "aigw-token/aigw-token", "text/plain; charset=utf-8"},
	{"aigw-token.ps1", "aigw-token/aigw-token.ps1", "text/plain; charset=utf-8"},
	{"install.sh", "install/install.sh", "text/plain; charset=utf-8"},
	{"install.ps1", "install/install.ps1", "text/plain; charset=utf-8"},
	{"aigw-usage.ts", "omp/aigw-usage.ts", "text/plain; charset=utf-8"},
}

// PluginArchive is the name under /clients/ of the plugin's marketplace.
const PluginArchive = "claude-code.tar.gz"

// Read returns a file of Files by its name.
func Read(name string) (File, []byte, bool) {
	for _, f := range Files {
		if f.Name == name {
			data, err := files.ReadFile(f.Path)
			return f, data, err == nil
		}
	}
	return File{}, nil, false
}

// Marketplace is the name Claude Code knows the marketplace by, and Plugin
// the plugin's: a person installs aigw-usage@aigw, as from GitHub.
const (
	Marketplace = "aigw"
	Plugin      = "aigw-usage"
)

var version struct {
	once sync.Once
	text string
}

// Version names the client files this hub serves: the first 12 hex digits
// of a SHA-256 over each file's name and bytes, the plugin's archive with
// them. A machine the installer set up keeps the version it installed, and
// aigw-token runs the installer again when the hub's differs, so a hub
// upgrade that changes a client reaches every machine by itself, and one
// that changes none moves nobody. Empty if the archive cannot be built.
func Version() string {
	version.once.Do(func() {
		h := sha256.New()
		for _, f := range Files {
			_, data, _ := Read(f.Name)
			fmt.Fprintf(h, "%s\x00%d\x00", f.Name, len(data))
			h.Write(data)
		}
		data, err := Archive()
		if err != nil {
			return
		}
		fmt.Fprintf(h, "%s\x00%d\x00", PluginArchive, len(data))
		h.Write(data)
		version.text = hex.EncodeToString(h.Sum(nil))[:12]
	})
	return version.text
}

var archive struct {
	once sync.Once
	data []byte
	err  error
}

// Archive is a marketplace with the one plugin, as a gzipped tar: its root
// holds .claude-plugin/marketplace.json and the plugin in aigw-usage/.
// `claude plugin marketplace add` takes the directory it is unpacked to. It
// is the same bytes on every call, and the plugin's tests are left out.
func Archive() ([]byte, error) {
	archive.once.Do(func() { archive.data, archive.err = build() })
	return archive.data, archive.err
}

func build() ([]byte, error) {
	var manifest struct {
		Description string `json:"description"`
	}
	raw, err := plugin.ReadFile("claude-code/.claude-plugin/plugin.json")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	market, err := json.MarshalIndent(map[string]any{
		"name":     Marketplace,
		"owner":    map[string]string{"name": "AI Gateway Control"},
		"metadata": map[string]string{"description": "Tools for the users of an AI Gateway Control hub"},
		"plugins":  []map[string]string{{"name": Plugin, "source": "./" + Plugin, "description": manifest.Description}},
	}, "", "  ")
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// One time for every entry, so that the bytes depend on the files only.
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(name string, data []byte, mode int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: at, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add(".claude-plugin/marketplace.json", append(market, '\n'), 0o644); err != nil {
		return nil, err
	}
	err = fs.WalkDir(plugin, "claude-code", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".test.mjs") {
			return err
		}
		data, err := plugin.ReadFile(p)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if path.Ext(p) == ".mjs" {
			mode = 0o755
		}
		return add(Plugin+strings.TrimPrefix(p, "claude-code"), data, mode)
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
