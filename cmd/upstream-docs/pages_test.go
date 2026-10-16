package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testProject() Project {
	return Project{ID: "eg", Name: "Envoy Gateway", Repo: "envoyproxy/gateway", Site: "https://gateway.envoyproxy.io",
		Track: "release", Roots: []string{"site/content/en/v{minor}"}, Static: "site/static", Ref: "v1.9.2", Version: "v1.9.2"}
}

// The archive of a repository as GitHub serves it: everything under
// "<repo>-<ref>/".
func archiveOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, text := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "gateway-1.9.2/" + name, Mode: 0o644, Size: int64(len(text)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(text))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestWrite(t *testing.T) {
	root := "site/content/en/v1.9/"
	files, err := readArchive(bytes.NewReader(archiveOf(t, map[string]string{
		"LICENSE":                    "                                 Apache License\n                           Version 2.0, January 2004\n",
		"go.mod":                     "module x",
		"site/static/img/arch.png":   "png",
		"site/static/img/unused.png": "png",
		root + "_index.md":           "---\ntitle: \"Docs\"\nweight: 1\n---\nWelcome.\n",
		root + "tasks/rate-limit.md": "---\ntitle: Global Rate Limit\nweight: 3\n---\n" +
			"{{% alert title=\"Heads up\" color=\"primary\" %}}\nNeeds Redis.\n{{% /alert %}}\n" +
			"{{< tabpane text=true >}}\n{{% tab header=\"Apply from stdin\" %}}\n```shell\nkubectl apply -f -\nexport X=1\n```\n{{% /tab %}}\n{{< /tabpane >}}\n" +
			"![Arch](/img/arch.png) and [quickstart](../quickstart) and [ref]({{< ref \"../quickstart.md\" >}})\n" +
			"[site](/contributions/) [yaml](./example.yaml) [up](https://x.io)\n",
		root + "quickstart.md":        "# Quick start\n\n:::note Before you start\nRead this.\n```\n:::\n```\n:::\n",
		"site/content/en/v1.8/old.md": "# Old version",
		"README.md":                   "# Not docs",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["go.mod"]; ok {
		t.Fatal("kept a file that is neither a page nor an image")
	}
	dir := t.TempDir()
	e, err := write(testProject(), files, dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, p := range e.Pages {
		paths = append(paths, p.Path+"="+p.Title)
	}
	if got := strings.Join(paths, ","); got != "site/content/en/v1.9/_index=Docs,site/content/en/v1.9/quickstart=Quick start,site/content/en/v1.9/tasks/rate-limit=Global Rate Limit" {
		t.Fatalf("pages: %s", got)
	}
	if e.License != "Apache License 2.0" {
		t.Fatalf("license %q", e.License)
	}
	if e.Images != 1 {
		t.Fatalf("images: %d, want only the one a page shows", e.Images)
	}
	if _, err := os.Stat(filepath.Join(dir, "site/static/img/arch.png")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "LICENSE")); err != nil {
		t.Fatal("the license is not there")
	}
	text, _ := os.ReadFile(filepath.Join(dir, root+"tasks/rate-limit.md"))
	got := string(text)
	for _, want := range []string{
		"# Global Rate Limit\n\n",
		"**Heads up:**\nNeeds Redis.",
		"**Apply from stdin**\n```shell\nkubectl apply -f -\nexport X=1\n```",
		"![Arch](/upstream-docs/eg/site/static/img/arch.png)",
		"[quickstart](#docs/upstream/eg/site/content/en/v1.9/quickstart)",
		"[ref](#docs/upstream/eg/site/content/en/v1.9/quickstart)",
		"[site](https://gateway.envoyproxy.io/contributions/)",
		"[yaml](https://github.com/envoyproxy/gateway/blob/v1.9.2/site/content/en/v1.9/tasks/example.yaml)",
		"[up](https://x.io)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rate-limit.md lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "{{") || strings.Contains(got, "title:") {
		t.Errorf("shortcodes or front matter left:\n%s", got)
	}
	text, _ = os.ReadFile(filepath.Join(dir, root+"quickstart.md"))
	if want := "> **Before you start**\n>\n> Read this.\n> ```\n> :::\n> ```\n"; !strings.Contains(string(text), want) {
		t.Errorf("admonition:\n%s", text)
	}
}

// MkDocs admonitions and tabs: their indented content is taken out of what
// would otherwise show as code.
func TestMkDocsBlocks(t *testing.T) {
	c := converter{p: Project{ID: "vllm", Repo: "vllm-project/vllm", Ref: "v0.31.0"}, files: map[string][]byte{}, pages: map[string]bool{}, used: map[string]bool{}}
	text, title, _ := c.convert("docs/serving/x", "# Serving\n\n!!! warning \"GPU only\"\n    Needs a GPU.\n\n    ```bash\n    vllm serve\n    ```\n\nAfter.\n\n=== \"pip\"\n\n    pip install vllm\n\n--8<-- \"docs/snippet.md\"\n")
	if title != "Serving" {
		t.Fatalf("title %q", title)
	}
	for _, want := range []string{"> **GPU only**\n>\n> Needs a GPU.\n> \n> ```bash\n> vllm serve\n> ```", "After.", "**pip**\n\n\npip install vllm"} {
		if !strings.Contains(text, want) {
			t.Errorf("lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "--8<--") {
		t.Errorf("snippet line left:\n%s", text)
	}
	// A comment in a shell block is not the title.
	if text, title, _ = c.convert("docs/x/setup", "Text.\n\n```bash\n# Install it\npip install x\n```\n"); title != "Setup" || !strings.HasPrefix(text, "# Setup\n\n") {
		t.Fatalf("title %q:\n%s", title, text)
	}
}

// A root with no pages at the pinned ref fails the build, so a project that
// moved its docs is noticed.
func TestMissingRoot(t *testing.T) {
	_, err := write(testProject(), map[string][]byte{"site/content/en/latest/a.md": []byte("# A")}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no pages under site/content/en/v1.9") {
		t.Fatalf("err %v", err)
	}
}

func TestRoots(t *testing.T) {
	p := Project{Roots: []string{"versioned_docs/version-{version}/", "site/content/en/v{minor}"}, Version: "v1.10.3"}
	if got := strings.Join(p.roots(), ","); got != "versioned_docs/version-1.10.3,site/content/en/v1.10" {
		t.Fatal(got)
	}
}

// The pinned file names every project the docs page shows.
func TestProjectsFile(t *testing.T) {
	ps, err := load("../../docs/upstream/projects.json")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range ps.Projects {
		if p.Ref == "" || p.Version == "" || len(p.Roots) == 0 || (p.Track != "release" && p.Track != "branch") {
			t.Errorf("incomplete: %+v", p)
		}
		ids = append(ids, p.ID)
	}
	if got := strings.Join(ids, ","); got != "envoy-ai-gateway,envoy-gateway,kserve,vllm,llm-d" {
		t.Fatal(got)
	}
	data, _ := json.Marshal(ps)
	if !bytes.Contains(data, []byte(`"track":"branch","versions":"versions.json"`)) {
		t.Fatal("kserve tracks its branch and versions file")
	}
}

// The Docusaurus components the projects use become Markdown.
func TestComponents(t *testing.T) {
	got := strings.Join(components(strings.Split("<Tabs>\n  <TabItem value=\"genai\" label=\"Generative AI\" default>\n\n| A | B |\n  </TabItem>\n</Tabs>\n"+
		"<ApiField\n  name=\"kind\"\n  type=\"String\"\n  required=\"true\"\n  description=\"The <code>kind</code>.\"\n/>\n"+
		"<CodeBlock language=\"shell\">\n{`kubectl apply -f x.yaml`}\n</CodeBlock>\n```\n<Tabs>\n```", "\n")), "\n")
	want := "\n**Generative AI**\n\n\n| A | B |\n- **kind** (String, required): The <code>kind</code>.\n```shell\nkubectl apply -f x.yaml\n```\n```\n<Tabs>\n```"
	if got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}
