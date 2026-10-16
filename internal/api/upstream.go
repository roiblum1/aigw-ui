package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"aigw-ui/docs"
)

// WithUpstreamDocs serves the documentation of the projects the platform is
// built on from dir, which the image fills at build time
// (cmd/upstream-docs). A missing dir leaves the Docs page without it.
func (s *Server) WithUpstreamDocs(dir string) *Server {
	s.upstreamDir = dir
	return s
}

// upstreamDocs serves index.json, the pages as Markdown and the images they
// show. It is public documentation, as free to read as the UI's own files,
// so it needs no token. Nothing it serves may run: every file goes with a
// policy under which no script runs and nothing is loaded from elsewhere,
// an SVG image opened on its own included.
func (s *Server) upstreamDocs(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/upstream-docs/"))
	if s.upstreamDir == "" || clean == "/" {
		http.NotFound(w, r)
		return
	}
	file := filepath.Join(s.upstreamDir, filepath.FromSlash(clean))
	if info, err := os.Stat(file); err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
	// The files change only with the image, which index.json is read
	// again on every load to notice.
	h.Set("Cache-Control", "no-cache")
	switch strings.ToLower(path.Ext(clean)) {
	case ".md":
		h.Set("Content-Type", "text/markdown; charset=utf-8")
	case ".json":
		h.Set("Content-Type", "application/json")
	case "":
		// LICENSE and NOTICE.
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	http.ServeFile(w, r, file)
}

// upstreamCorpus is the upstream documentation in memory, for search: read
// once, at the first search.
type upstreamCorpus struct {
	once sync.Once
	docs []searchDoc
}

type searchDoc struct {
	name, title, group, text, lower string
}

func (s *Server) upstreamCorpus() []searchDoc {
	s.upstream.once.Do(func() {
		if s.upstreamDir == "" {
			return
		}
		data, err := os.ReadFile(filepath.Join(s.upstreamDir, "index.json"))
		if err != nil {
			return
		}
		var idx struct {
			Projects []struct {
				ID, Name string
				Pages    []struct{ Path, Title string }
			}
		}
		if json.Unmarshal(data, &idx) != nil {
			return
		}
		for _, p := range idx.Projects {
			for _, page := range p.Pages {
				text, err := os.ReadFile(filepath.Join(s.upstreamDir, p.ID, filepath.FromSlash(page.Path)+".md"))
				if err != nil {
					continue
				}
				s.upstream.docs = append(s.upstream.docs, searchDoc{name: "upstream/" + p.ID + "/" + page.Path, title: page.Title,
					group: p.Name, text: string(text), lower: strings.ToLower(string(text))})
			}
		}
	})
	return s.upstream.docs
}

// searchHit is one document a search found, with the line it found first.
type searchHit struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Group   string `json:"group"`
	Snippet string `json:"snippet"`
}

// maxHits is the most documents a search returns.
const maxHits = 50

// searchDocs finds a text, in any case, in the guides, the release notes
// and the upstream documentation: the documents with it in their title
// first, then the ones that have it most often.
func (s *Server) searchDocs(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if len([]rune(q)) < 2 {
		fail(w, invalid("q needs at least two characters"))
		return
	}
	corpus := []searchDoc{}
	for _, d := range docs.List() {
		text, err := docs.Read(d.Name)
		if err != nil {
			continue
		}
		corpus = append(corpus, searchDoc{name: d.Name, title: d.Title, group: d.Group, text: text, lower: strings.ToLower(text)})
	}
	corpus = append(corpus, s.upstreamCorpus()...)

	type scored struct {
		hit          searchHit
		inTitle      bool
		count, order int
	}
	var found []scored
	for i, d := range corpus {
		inTitle := strings.Contains(strings.ToLower(d.title), q)
		count := strings.Count(d.lower, q)
		if !inTitle && count == 0 {
			continue
		}
		found = append(found, scored{hit: searchHit{Name: d.name, Title: d.title, Group: d.group, Snippet: snippet(d.text, d.lower, q)},
			inTitle: inTitle, count: count, order: i})
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].inTitle != found[j].inTitle {
			return found[i].inTitle
		}
		return found[i].count > found[j].count
	})
	hits := make([]searchHit, 0, min(len(found), maxHits))
	for _, f := range found[:min(len(found), maxHits)] {
		hits = append(hits, f.hit)
	}
	writeJSON(w, http.StatusOK, hits)
}

// snippet returns the line of text where q is first, cut to about 160
// characters around it.
func snippet(text, lower, q string) string {
	at := strings.Index(lower, q)
	if at < 0 {
		return ""
	}
	start := strings.LastIndex(lower[:at], "\n") + 1
	end := strings.Index(lower[at:], "\n")
	if end < 0 {
		end = len(lower)
	} else {
		end += at
	}
	from, to := max(start, at-80), min(end, at+len(q)+80)
	// Cut on whole characters.
	for from > start && !utf8Start(text[from]) {
		from--
	}
	for to < end && !utf8Start(text[to]) {
		to++
	}
	out := strings.TrimSpace(text[from:to])
	if from > start {
		out = "…" + out
	}
	if to < end {
		out += "…"
	}
	return out
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
