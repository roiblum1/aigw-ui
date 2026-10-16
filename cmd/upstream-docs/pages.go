package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Index is index.json: every project in the image and its pages.
type Index struct {
	Generated string  `json:"generated"`
	Projects  []Entry `json:"projects"`
}

// Entry is one project in the index.
type Entry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Repo    string `json:"repo"`
	Ref     string `json:"ref"`
	Version string `json:"version"`
	Site    string `json:"site"`
	// License names the license of the documentation, as its LICENSE file
	// does, when the converter knows it; the file goes with the pages.
	License string `json:"license,omitempty"`
	// Roots are the folders of the pages, the top of the project's tree.
	Roots []string `json:"roots"`
	Pages []Page   `json:"pages"`
	// Images is how many images the pages show, and Bytes the size of
	// everything written for the project.
	Images int   `json:"images"`
	Bytes  int64 `json:"bytes"`
}

// Page is one page: its path in the repository without the extension, as
// the hub serves it with ".md".
type Page struct {
	Path   string `json:"path"`
	Title  string `json:"title"`
	Weight int    `json:"weight,omitempty"`
}

// write converts the project's pages, writes them and the images they show
// under dir, and returns the project's entry in the index.
func write(p Project, files map[string][]byte, dir string) (Entry, error) {
	e := Entry{ID: p.ID, Name: p.Name, Repo: p.Repo, Ref: p.Ref, Version: p.Version, Site: p.Site, Roots: p.roots()}
	c := converter{p: p, files: files, pages: map[string]bool{}, used: map[string]bool{}}
	for name := range files {
		if isPage(name) && c.under(name) {
			c.pages[strings.TrimSuffix(name, path.Ext(name))] = true
		}
	}
	for _, root := range e.Roots {
		found := false
		for page := range c.pages {
			if strings.HasPrefix(page, root+"/") {
				found = true
				break
			}
		}
		if !found {
			return e, fmt.Errorf("no pages under %s at %s", root, p.Ref)
		}
	}
	var names []string
	for page := range c.pages {
		names = append(names, page)
	}
	sort.Strings(names)
	for _, page := range names {
		src := page + ".md"
		if _, ok := files[src]; !ok {
			src = page + ".mdx"
		}
		text, title, weight := c.convert(page, string(files[src]))
		if err := put(dir, page+".md", []byte(text), &e.Bytes); err != nil {
			return e, err
		}
		e.Pages = append(e.Pages, Page{Path: page, Title: title, Weight: weight})
	}
	for name := range c.used {
		if err := put(dir, name, files[name], &e.Bytes); err != nil {
			return e, err
		}
		e.Images++
	}
	e.License = licenseOf(files["LICENSE"])
	for name, data := range files {
		if !strings.Contains(name, "/") && !isPage(name) && !isImage(name) {
			if err := put(dir, name, data, &e.Bytes); err != nil {
				return e, err
			}
		}
	}
	return e, nil
}

// licenseOf names the license a LICENSE file holds, when it is one the
// converter knows.
func licenseOf(text []byte) string {
	head := string(text[:min(len(text), 400)])
	switch {
	case strings.Contains(head, "Apache License") && strings.Contains(head, "Version 2.0"):
		return "Apache License 2.0"
	case strings.Contains(head, "MIT License"):
		return "MIT License"
	}
	return ""
}

func put(dir, name string, data []byte, total *int64) error {
	file := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	*total += int64(len(data))
	return os.WriteFile(file, data, 0o644)
}

// converter turns one project's pages into plain Markdown that the hub's
// Docs page shows.
type converter struct {
	p     Project
	files map[string][]byte
	// pages are the paths of the pages, without the extension.
	pages map[string]bool
	// used are the images the pages show.
	used map[string]bool
}

func (c *converter) under(name string) bool {
	for _, root := range c.p.roots() {
		if strings.HasPrefix(name, root+"/") {
			return true
		}
	}
	return false
}

var (
	fence       = regexp.MustCompile("^\\s*(```|~~~)")
	mdxImport   = regexp.MustCompile(`^import\s+.+\s+from\s+['"][^'"]+['"];?\s*$`)
	shortcode   = regexp.MustCompile(`^\s*\{\{[<%]\s*(/?)\s*([\w-]+)([^}]*?)\s*[%>]\}\}\s*$`)
	refCode     = regexp.MustCompile(`\{\{[<%]\s*(?:rel)?ref\s+"([^"]+)"\s*[%>]\}\}`)
	anyCode     = regexp.MustCompile(`\{\{[<%].*?[%>]\}\}`)
	attr        = regexp.MustCompile(`(\w+)="([^"]*)"`)
	docusaurus  = regexp.MustCompile(`^:::\s*(\w+)\s*(.*)$`)
	mkdocsBlock = regexp.MustCompile(`^(\s*)(!!!|\?\?\?\+?)\s+(\w+)(?:\s+"([^"]*)")?\s*$`)
	mkdocsTab   = regexp.MustCompile(`^(\s*)===\+?\s+"([^"]*)"\s*$`)
	snippet     = regexp.MustCompile(`^\s*--8<--`)
	mdImage     = regexp.MustCompile(`(!\[[^\]]*\]\()([^)\s]+)((?:\s+"[^"]*")?\))`)
	mdLink      = regexp.MustCompile(`((?:^|[^!])\[[^\]]*\]\()([^)\s]+)((?:\s+"[^"]*")?\))`)
	htmlSrc     = regexp.MustCompile(`(<img\b[^>]*\bsrc=")([^"]+)(")`)
	htmlHref    = regexp.MustCompile(`(<a\b[^>]*\bhref=")([^"]+)(")`)
	refDef      = regexp.MustCompile(`^(\s*\[[^\]]+\]:\s*)(\S+)(.*)$`)
	heading     = regexp.MustCompile(`^#\s+(.+?)\s*#*\s*$`)
)

// convert returns the page as plain Markdown, its title and its weight in
// the project's own order.
func (c *converter) convert(page, text string) (string, string, int) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	meta, body := frontMatter(text)
	lines := strings.Split(body, "\n")
	lines = components(lines)
	lines = c.blocks(lines)
	inFence := false
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		bare := strings.TrimLeft(line, "> ")
		if fence.MatchString(bare) {
			inFence = !inFence
			out = append(out, line)
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		if mdxImport.MatchString(bare) || snippet.MatchString(bare) {
			continue
		}
		if m := shortcode.FindStringSubmatch(bare); m != nil {
			if s, ok := shortcodeLine(m); ok {
				out = append(out, s)
			}
			continue
		}
		line = refCode.ReplaceAllString(line, "$1")
		line = anyCode.ReplaceAllString(line, "")
		out = append(out, c.links(page, line))
	}
	title := meta["title"]
	if title == "" {
		title = meta["linktitle"]
	}
	// The first heading outside the code blocks: in a shell block "# " is a
	// comment.
	h1 := ""
	code := false
	for _, line := range out {
		if fence.MatchString(line) {
			code = !code
		} else if m := heading.FindStringSubmatch(line); m != nil && !code {
			h1 = m[1]
			break
		}
	}
	if title == "" {
		title = h1
	}
	if title == "" {
		title = nameOf(page)
	}
	title = strings.Trim(title, `"'`)
	// A site generator shows the title from the front matter above the
	// page; here the page shows it itself.
	if h1 == "" {
		out = append([]string{"# " + title, ""}, out...)
	}
	weight, _ := strconv.Atoi(meta["weight"])
	if weight == 0 {
		weight, _ = strconv.Atoi(meta["sidebar_position"])
	}
	return strings.Join(out, "\n"), title, weight
}

// frontMatter splits the YAML front matter off a page and returns its
// top-level keys, lower case, without quotes.
func frontMatter(text string) (map[string]string, string) {
	meta := map[string]string{}
	if !strings.HasPrefix(text, "---\n") {
		return meta, text
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return meta, text
	}
	for _, line := range strings.Split(text[4:4+end], "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && !strings.HasPrefix(line, " ") {
			meta[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	rest := text[4+end+4:]
	return meta, strings.TrimPrefix(rest, "\n")
}

// nameOf makes a title of a page's file name: the folder's for an index.
func nameOf(page string) string {
	base := path.Base(page)
	switch strings.ToLower(base) {
	case "readme", "index", "_index":
		base = path.Base(path.Dir(page))
	}
	base = strings.NewReplacer("-", " ", "_", " ").Replace(base)
	if base == "" {
		return page
	}
	return strings.ToUpper(base[:1]) + base[1:]
}

// shortcodeLine turns a line that is only a Hugo shortcode into Markdown: a
// tab's header and an alert's title become a bold line, anything else goes.
func shortcodeLine(m []string) (string, bool) {
	if m[1] == "/" {
		return "", false
	}
	attrs := map[string]string{}
	for _, a := range attr.FindAllStringSubmatch(m[3], -1) {
		attrs[a[1]] = a[2]
	}
	switch m[2] {
	case "tab":
		if h := attrs["header"]; h != "" {
			return "**" + h + "**", true
		}
	case "alert":
		t := attrs["title"]
		if t == "" {
			t = "Note"
		}
		return "**" + t + ":**", true
	}
	return "", false
}

var (
	tabsTag   = regexp.MustCompile(`^\s*</?Tabs\b[^>]*>\s*$|^\s*</TabItem>\s*$`)
	tabItem   = regexp.MustCompile(`^\s*<TabItem\b([^>]*)>\s*$`)
	codeBlock = regexp.MustCompile(`^\s*<CodeBlock\b([^>]*)>\s*$`)
	jsxAttr   = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

// components turns the Docusaurus components the projects use into
// Markdown: a tab's label becomes a bold line, an API field a list item, a
// code block a fenced one; the tabs around them go.
func components(lines []string) []string {
	var out []string
	inFence := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if fence.MatchString(line) {
			inFence = !inFence
		}
		if inFence {
			out = append(out, line)
			continue
		}
		switch {
		case tabsTag.MatchString(line):
			continue
		case tabItem.MatchString(line):
			attrs := attrsOf(tabItem.FindStringSubmatch(line)[1])
			label := attrs["label"]
			if label == "" {
				label = attrs["value"]
			}
			out = append(out, "", "**"+label+"**", "")
			continue
		case strings.HasPrefix(strings.TrimSpace(line), "<ApiField"):
			// Its attributes run to the line that closes it.
			tag := line
			for !strings.Contains(tag, "/>") && i+1 < len(lines) {
				i++
				tag += " " + strings.TrimSpace(lines[i])
			}
			a := attrsOf(tag)
			item := "- **" + a["name"] + "**"
			if a["type"] != "" {
				item += " (" + a["type"]
				if a["required"] == "true" {
					item += ", required"
				}
				item += ")"
			}
			if a["description"] != "" {
				item += ": " + a["description"]
			}
			out = append(out, item)
			continue
		case codeBlock.MatchString(line):
			lang := attrsOf(codeBlock.FindStringSubmatch(line)[1])["language"]
			out = append(out, "```"+lang)
			for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "</CodeBlock>" {
				i++
				code := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(lines[i]), "{`"), "`}")
				out = append(out, code)
			}
			i++ // </CodeBlock>
			out = append(out, "```")
			continue
		}
		out = append(out, line)
	}
	return out
}

func attrsOf(s string) map[string]string {
	attrs := map[string]string{}
	for _, a := range jsxAttr.FindAllStringSubmatch(s, -1) {
		attrs[a[1]] = a[2]
	}
	return attrs
}

// blocks turns Docusaurus admonitions (":::note" … ":::") and MkDocs
// admonitions ("!!! note", "??? note") into quotes, and MkDocs tabs
// ("=== \"pip\"") into a bold line, taking the four spaces off their
// indented content. Code blocks are left alone.
func (c *converter) blocks(lines []string) []string {
	var out []string
	inFence, quote := false, false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if fence.MatchString(line) {
			inFence = !inFence
		}
		if inFence && !fence.MatchString(line) {
			out = append(out, prefix(quote, line))
			continue
		}
		if m := docusaurus.FindStringSubmatch(strings.TrimSpace(line)); m != nil && !inFence {
			label := strings.ToUpper(m[1][:1]) + m[1][1:]
			if t := strings.Trim(strings.TrimSpace(m[2]), "[]"); t != "" {
				label = t
			}
			out = append(out, "> **"+label+"**", ">")
			quote = true
			continue
		}
		if strings.TrimSpace(line) == ":::" && quote && !inFence {
			quote = false
			out = append(out, "")
			continue
		}
		if m := mkdocsBlock.FindStringSubmatch(line); m != nil && !inFence {
			body, next := indented(lines, i+1, len(m[1])+4)
			label := m[4]
			if label == "" {
				label = strings.ToUpper(m[3][:1]) + m[3][1:]
			}
			out = append(out, prefix(quote, m[1]+"> **"+label+"**"), prefix(quote, m[1]+">"))
			for _, b := range c.blocks(body) {
				out = append(out, prefix(quote, m[1]+"> "+b))
			}
			out = append(out, "")
			i = next - 1
			continue
		}
		if m := mkdocsTab.FindStringSubmatch(line); m != nil && !inFence {
			body, next := indented(lines, i+1, len(m[1])+4)
			out = append(out, prefix(quote, m[1]+"**"+m[2]+"**"), "")
			for _, b := range c.blocks(body) {
				out = append(out, prefix(quote, m[1]+b))
			}
			i = next - 1
			continue
		}
		out = append(out, prefix(quote, line))
	}
	return out
}

func prefix(quote bool, line string) string {
	if quote {
		return "> " + line
	}
	return line
}

// indented returns the lines from start that are blank or indented by at
// least n spaces, with n spaces taken off, and the index after them.
func indented(lines []string, start, n int) ([]string, int) {
	pad := strings.Repeat(" ", n)
	var body []string
	i := start
	for ; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			body = append(body, "")
			continue
		}
		if !strings.HasPrefix(line, pad) {
			break
		}
		body = append(body, line[n:])
	}
	// Blank lines after the block belong to what follows.
	for len(body) > 0 && body[len(body)-1] == "" {
		body = body[:len(body)-1]
		i--
	}
	return body, i
}

// links points the line's images at the hub and its links at the page in
// the hub, the project's site or the file on GitHub.
func (c *converter) links(page, line string) string {
	img := func(m []string) string { return m[1] + c.image(page, m[2]) + m[3] }
	link := func(m []string) string { return m[1] + c.link(page, m[2]) + m[3] }
	line = replace(mdImage, line, img)
	line = replace(htmlSrc, line, img)
	line = replace(mdLink, line, link)
	line = replace(htmlHref, line, link)
	return replace(refDef, line, func(m []string) string {
		if isImage(m[2]) {
			return m[1] + c.image(page, m[2]) + m[3]
		}
		return m[1] + c.link(page, m[2]) + m[3]
	})
}

func replace(re *regexp.Regexp, s string, f func([]string) string) string {
	return re.ReplaceAllStringFunc(s, func(match string) string { return f(re.FindStringSubmatch(match)) })
}

func external(url string) bool {
	return strings.HasPrefix(url, "#") || strings.Contains(url, "://") || strings.HasPrefix(url, "mailto:") || strings.HasPrefix(url, "data:")
}

// image returns where the hub serves an image the page shows, or the
// image on GitHub when it is not in the archive.
func (c *converter) image(page, url string) string {
	if external(url) {
		return url
	}
	clean, _, _ := strings.Cut(url, "?")
	clean, _, _ = strings.Cut(clean, "#")
	var name string
	if strings.HasPrefix(clean, "/") {
		name = path.Join(c.p.Static, clean)
	} else {
		name = path.Join(path.Dir(page), clean)
	}
	if _, ok := c.files[name]; ok {
		c.used[name] = true
		return "/upstream-docs/" + c.p.ID + "/" + name
	}
	return "https://raw.githubusercontent.com/" + c.p.Repo + "/" + c.p.Ref + "/" + name
}

// link returns where a link of the page goes: the page in the hub
// ("#docs/upstream/<project>/<path>"), the project's site for a path of the
// site, or the file on GitHub.
func (c *converter) link(page, url string) string {
	if external(url) {
		return url
	}
	target, _, _ := strings.Cut(url, "#")
	target, _, _ = strings.Cut(target, "?")
	if target == "" {
		return url
	}
	if strings.HasPrefix(target, "/") {
		return strings.TrimRight(c.p.Site, "/") + url
	}
	// A link is relative to the page's folder, or, on a site that serves a
	// page as a folder (Hugo), to the page itself.
	for _, base := range []string{path.Dir(page), page} {
		p := path.Clean(path.Join(base, target))
		for _, cand := range []string{strings.TrimSuffix(strings.TrimSuffix(p, ".md"), ".mdx"), p + "/index", p + "/README", p + "/_index"} {
			if c.pages[cand] {
				return "#docs/upstream/" + c.p.ID + "/" + cand
			}
		}
	}
	return "https://github.com/" + c.p.Repo + "/blob/" + c.p.Ref + "/" + path.Clean(path.Join(path.Dir(page), target))
}
