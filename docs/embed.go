// Package docs holds the documents the server shows in the UI.
package docs

import (
	"embed"
	"sort"
	"strconv"
	"strings"
)

// PlatformArchitecture is the design of the whole multi-site platform, as a
// self-contained HTML page: no scripts and nothing loaded from the network.
//
//go:embed platform-architecture.html
var PlatformArchitecture []byte

//go:embed *.md release-notes/*.md
var files embed.FS

// Doc is one document of the Docs page.
type Doc struct {
	// Name is the file's path without ".md", such as "user-guide" or
	// "release-notes/v0.8.0".
	Name  string `json:"name"`
	Title string `json:"title"`
	// Group is "Guides" or "Release notes".
	Group string `json:"group"`
}

// guides is the order of the guides: what a new operator reads first.
var guides = []string{"how-it-works", "user-guide", "optimization", "pricing", "performance", "architecture", "operations", "deployment", "security", "api", "platform-architecture"}

// List returns every document: the guides in reading order, then the release
// notes, newest first.
func List() []Doc {
	var out []Doc
	for _, name := range guides {
		if text, err := Read(name); err == nil {
			out = append(out, Doc{Name: name, Title: title(text, name), Group: "Guides"})
		}
	}
	entries, _ := files.ReadDir("release-notes")
	var notes []string
	for _, e := range entries {
		if name := strings.TrimSuffix(e.Name(), ".md"); strings.HasPrefix(name, "v") {
			notes = append(notes, name)
		}
	}
	sort.Slice(notes, func(i, j int) bool { return newer(notes[i], notes[j]) })
	for _, name := range notes {
		text, _ := Read("release-notes/" + name)
		out = append(out, Doc{Name: "release-notes/" + name, Title: title(text, name), Group: "Release notes"})
	}
	return out
}

// Read returns a document's Markdown. The name is one that List returns.
func Read(name string) (string, error) {
	data, err := files.ReadFile(name + ".md")
	return string(data), err
}

// title is the document's first heading.
func title(text, fallback string) string {
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return fallback
}

// newer compares two versions such as "v0.10.0" and "v0.9.1" by number.
func newer(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, _ := strconv.Atoi(pa[i])
		nb, _ := strconv.Atoi(pb[i])
		if na != nb {
			return na > nb
		}
	}
	return len(pa) > len(pb)
}
