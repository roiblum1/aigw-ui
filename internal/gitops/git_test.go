package gitops

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// clone is someone else's clone of the repository.
type clone struct {
	dir string
	git func(args ...string) string
}

// write changes files in the clone, commits and pushes them.
func (c clone) write(t *testing.T, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(c.dir, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c.git("add", "-A")
	c.git("commit", "-q", "-m", "by hand")
	c.git("push", "-q", "origin", "main")
}

// platform is a values repository in the platform's layout under
// gitops/app-of-apps: two models (qwen-tiny on the lab, glm53 on prep), an
// operator that is not a model (gpu), and a site layer under the lab.
var platform = map[string]string{
	"gitops/app-of-apps/operators/qwen-tiny/qwen-tiny.yaml":                        "name: qwen-tiny\nprojectNamespace: vllm-lab\nrepourl: https://git.example/doomsday.git\nchartPath: charts/model-deployer\ntargetRevision: main\n",
	"gitops/app-of-apps/operators/qwen-tiny/values.yaml":                           "namespace: vllm-lab\nmodel:\n  name: qwen-tiny\n  uri: hf://Qwen/Qwen2.5-0.5B-Instruct\nllmisvc:\n  replicas: 1\n  args: [--a, --b]\n  env:\n    X: \"1\"\n",
	"gitops/app-of-apps/operators/glm53/glm53.yaml":                                "name: glm53\nprojectNamespace: llm-glm53\nrepourl: https://git.example/model-deployer-chart.git\ntargetRevision: main\n",
	"gitops/app-of-apps/operators/glm53/values.yaml":                               "namespace: llm-glm53\nmodel:\n  name: glm-5.3\n  subPath: GLM-5.3\nllmisvc:\n  name: glm53-b200\n  replicas: 2\n",
	"gitops/app-of-apps/operators/gpu/gpu.yaml":                                    "name: gpu\nprojectNamespace: nvidia-gpu-operator\nrepourl: https://git.example/gpu-chart.git\ntargetRevision: main\n",
	"gitops/app-of-apps/operators/gpu/values.yaml":                                 "",
	"gitops/app-of-apps/sites/site-lab/lab/values.yaml":                            "llmisvc:\n  env:\n    SITE: lab\n",
	"gitops/app-of-apps/sites/site-lab/lab/mces/mce-lab/lab/qwen-tiny/values.yaml": "",
	"gitops/app-of-apps/sites/site-lab/lab/mces/mce-lab/lab/gpu/values.yaml":       "",
	"gitops/app-of-apps/sites/site-a/prep/mces/mce-prep/prep/glm53/values.yaml":    "llmisvc:\n  replicas: 1\n",
	"gitops/app-of-apps/sites/site-a/prep/mces/mce-prep/gpu-only/gpu/values.yaml":  "",
	"README.md": "not ours\n",
}

// seedRepo makes a bare repository on disk with the platform's layout, and
// returns its URL and someone else's clone.
func seedRepo(t *testing.T) (string, clone) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "repo.git")
	other := filepath.Join(root, "other")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "HOME="+root, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(root, "init", "-q", "--bare", "-b", "main", bare)
	run(root, "clone", "-q", bare, other)
	run(other, "checkout", "-q", "-b", "main")
	c := clone{dir: other, git: func(args ...string) string { return run(other, args...) }}
	c.write(t, platform)
	return "file://" + bare, c
}

func newRepo(t *testing.T, url string) *Repo {
	t.Helper()
	r, err := New(Config{Repo: url, Branch: "main", Dir: "gitops/app-of-apps", WorkDir: t.TempDir(), AuthorName: "aigw-ui", AuthorEmail: "hub@test"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func find(t *testing.T, s Snapshot, cluster, model string) Deployment {
	t.Helper()
	for _, d := range s.Deployments {
		if d.Cluster == cluster && d.Model == model {
			return d
		}
	}
	t.Fatalf("no deployment of %s on %s in %+v", model, cluster, s.Deployments)
	return Deployment{}
}

// The hub reads the platform's layout: the operators whose values hold a
// model are models (gpu is not), every cluster directory is a cluster, and
// a model's directory under a cluster is a deployment, its values merged
// layer by layer as Argo CD renders them (the site's layer between the
// recipe and the cluster's own).
func TestReadThePlatformLayout(t *testing.T) {
	url, _ := seedRepo(t)
	snap, err := newRepo(t, url).Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var models, clusters []string
	for _, m := range snap.Models {
		models = append(models, m.Name+"="+m.ModelName+"@"+m.Namespace)
	}
	for _, c := range snap.Clusters {
		clusters = append(clusters, c.Site+"/"+c.Env+"/"+c.MCE+"/"+c.Name)
	}
	if !reflect.DeepEqual(models, []string{"glm53=glm-5.3@llm-glm53", "qwen-tiny=qwen-tiny@vllm-lab"}) ||
		!reflect.DeepEqual(clusters, []string{"site-a/prep/mce-prep/gpu-only", "site-a/prep/mce-prep/prep", "site-lab/lab/mce-lab/lab"}) ||
		len(snap.Deployments) != 2 || len(snap.Commit) != 40 {
		t.Fatalf("models %v, clusters %v, %d deployments", models, clusters, len(snap.Deployments))
	}
	q := find(t, snap, "lab", "qwen-tiny")
	if q.Namespace != "vllm-lab" || q.Object != "qwen-tiny" || q.ModelName != "qwen-tiny" || q.Problem != "" ||
		q.Path != "sites/site-lab/lab/mces/mce-lab/lab/qwen-tiny/values.yaml" || len(q.Revision) != 40 ||
		!reflect.DeepEqual(q.Layers, []string{"operators/qwen-tiny/values.yaml", "sites/site-lab/lab/values.yaml", q.Path}) {
		t.Fatalf("qwen-tiny: %+v", q)
	}
	if env := q.Values["llmisvc"].(map[string]any)["env"]; !reflect.DeepEqual(env, map[string]any{"X": "1", "SITE": "lab"}) {
		t.Fatalf("merged env %v", env)
	}
	g := find(t, snap, "prep", "glm53")
	if g.Object != "glm53-b200" || g.Values["llmisvc"].(map[string]any)["replicas"] != float64(1) || g.Override["llmisvc"] == nil {
		t.Fatalf("glm53: %+v", g)
	}
}

// Saving a deployment writes only the cluster's own layer: what differs
// from the layers below, and null for what was taken away, so that a change
// to the recipe still reaches the cluster. Read back, the merged values are
// what was saved. A save over a version someone has since changed is
// refused, and so is a new deployment where one exists.
func TestADeploymentIsSavedAsTheClustersOwnLayer(t *testing.T) {
	url, other := seedRepo(t)
	ctx := context.Background()
	r := newRepo(t, url)
	snap, _ := r.Read(ctx)
	q := find(t, snap, "lab", "qwen-tiny")
	lab := snap.Clusters[2]
	want := Merge(q.Values, map[string]any{"llmisvc": map[string]any{"replicas": float64(2), "env": map[string]any{"X": nil, "SITE": "lab"}}})
	change, err := snap.DeploymentChange(lab, "qwen-tiny", want, q.Revision)
	if err != nil {
		t.Fatal(err)
	}
	var written map[string]any
	yaml.Unmarshal(change.Content, &written)
	if !reflect.DeepEqual(written, map[string]any{"llmisvc": map[string]any{"replicas": float64(2), "env": map[string]any{"X": nil}}}) {
		t.Fatalf("written layer %v:\n%s", written, change.Content)
	}
	if !strings.HasPrefix(string(change.Content), "# Written by aigw-ui") {
		t.Fatalf("no header:\n%s", change.Content)
	}
	commit, err := r.Commit(ctx, []Change{change}, "aigw-ui: change qwen-tiny on lab")
	if err != nil {
		t.Fatal(err)
	}
	other.git("pull", "-q", "origin", "main")
	if got := other.git("log", "-1", "--format=%H %an %s"); got != commit+" aigw-ui aigw-ui: change qwen-tiny on lab" {
		t.Fatalf("pushed commit: %s", got)
	}
	r.fetchedAt = r.fetchedAt.AddDate(0, 0, -1)
	snap, _ = r.Read(ctx)
	if got := find(t, snap, "lab", "qwen-tiny"); !reflect.DeepEqual(got.Values, want) {
		t.Fatalf("read back %v, want %v", got.Values, want)
	}
	if _, err := r.Commit(ctx, []Change{change}, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a save over an old version: %v", err)
	}
	if _, err := r.Commit(ctx, []Change{{Path: change.Path, Content: change.Content}}, "new"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a new deployment where one exists: %v", err)
	}
	if other.git("show", "HEAD:README.md") != "not ours" {
		t.Fatal("a file outside the directory changed")
	}
}

// A new model is two files in one commit: its app config, copied from an
// existing model's with its own name and namespace, and its recipe. Then
// it is deployed on a cluster by a directory of its own there, and removed
// from the cluster by removing that directory.
func TestANewModelIsDeployedAndRemoved(t *testing.T) {
	url, other := seedRepo(t)
	ctx := context.Background()
	r := newRepo(t, url)
	snap, _ := r.Read(ctx)
	recipe := map[string]any{"namespace": "llm-small", "model": map[string]any{"name": "qwen-small", "uri": "hf://a/b"}}
	changes, err := NewModelChanges("qwen-small", "llm-small", snap.Models[1], recipe)
	if err != nil {
		t.Fatal(err)
	}
	lab := snap.Clusters[2]
	// The form starts from what the cluster's layers give the new recipe.
	snap = snap.With("operators/qwen-small/values.yaml", recipe)
	want := snap.Base(lab, "qwen-small")
	deploy, err := snap.DeploymentChange(lab, "qwen-small", want, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit(ctx, append(changes, deploy), "aigw-ui: deploy qwen-small on lab"); err != nil {
		t.Fatal(err)
	}
	r.fetchedAt = r.fetchedAt.AddDate(0, 0, -1)
	snap, _ = r.Read(ctx)
	d := find(t, snap, "lab", "qwen-small")
	if d.Namespace != "llm-small" || !reflect.DeepEqual(d.Values, want) || len(d.Override) != 0 || want["llmisvc"] == nil {
		t.Fatalf("new model on the lab: %+v", d)
	}
	other.git("pull", "-q", "origin", "main")
	var config map[string]any
	raw, _ := os.ReadFile(filepath.Join(other.dir, "gitops/app-of-apps/operators/qwen-small/qwen-small.yaml"))
	yaml.Unmarshal(raw, &config)
	if config["name"] != "qwen-small" || config["projectNamespace"] != "llm-small" || config["chartPath"] != "charts/model-deployer" {
		t.Fatalf("app config %v", config)
	}
	if _, err := r.Commit(ctx, []Change{{Path: d.Path, Dir: true, Base: d.Revision}}, "aigw-ui: remove qwen-small from lab"); err != nil {
		t.Fatal(err)
	}
	other.git("pull", "-q", "origin", "main")
	if _, err := os.Stat(filepath.Join(other.dir, "gitops/app-of-apps/sites/site-lab/lab/mces/mce-lab/lab/qwen-small")); !os.IsNotExist(err) {
		t.Fatalf("the registration is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other.dir, "gitops/app-of-apps/operators/qwen-small/values.yaml")); err != nil {
		t.Fatal("removing the deployment removed the model's recipe")
	}
}

// Merge is Helm's: maps merge, lists and scalars replace, null removes. An
// override merged onto its base gives back what was wanted.
func TestMergeAndOverride(t *testing.T) {
	base := map[string]any{"a": map[string]any{"b": 1.0, "c": []any{"x"}, "d": "keep"}, "e": "drop"}
	want := map[string]any{"a": map[string]any{"b": 2.0, "c": []any{"x", "y"}, "d": "keep"}, "f": true}
	over := Override(base, want)
	if !reflect.DeepEqual(over, map[string]any{"a": map[string]any{"b": 2.0, "c": []any{"x", "y"}}, "e": nil, "f": true}) {
		t.Fatalf("override %v", over)
	}
	if got := Merge(base, over); !reflect.DeepEqual(got, want) {
		t.Fatalf("merge %v, want %v", got, want)
	}
	if len(Override(want, want)) != 0 {
		t.Fatal("no change gives no override")
	}
}

func TestObjectName(t *testing.T) {
	if got := ObjectName(map[string]any{"model": map[string]any{"name": "glm-5.3"}}); got != "glm-5-3" {
		t.Fatal(got)
	}
	if got := ObjectName(map[string]any{"model": map[string]any{"name": "glm-5.3"}, "llmisvc": map[string]any{"name": "glm53-b200"}}); got != "glm53-b200" {
		t.Fatal(got)
	}
}

// A file's blob ID outside a clone is the one git gives it, so a version
// read from a plain directory compares with one read from git.
func TestBlobIDIsGits(t *testing.T) {
	url, other := seedRepo(t)
	_ = url
	want := other.git("rev-parse", "HEAD:gitops/app-of-apps/operators/qwen-tiny/values.yaml")
	if got := BlobID([]byte(platform["gitops/app-of-apps/operators/qwen-tiny/values.yaml"])); got != want {
		t.Fatalf("blob %s, git says %s", got, want)
	}
}

// A save changes only what differs in the file: the comments and the order
// of everything else stay, so a recipe that documents itself keeps its
// documentation. New keys go at the end of their map; a removed key goes
// with its comment.
func TestPatchKeepsComments(t *testing.T) {
	original := []byte(`# The recipe of glm53.
model:
  name: glm-5.3 # what tenants call
  # the S3 path
  subPath: GLM-5.3
llmisvc:
  # one pod per node
  replicas: 2
  args:
    - --a
    - --b
  gone: true
`)
	want := map[string]any{
		"model":   map[string]any{"name": "glm-5.3", "subPath": "GLM-5.3-v2"},
		"llmisvc": map[string]any{"replicas": 2.0, "args": []any{"--a", "--b"}, "extraArgs": []any{"--c"}},
		"images":  map[string]any{"vllm": "registry-internal/vllm:v1"},
	}
	out, err := Patch(original, "# header\n", want)
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"# The recipe of glm53.", "# what tenants call", "# the S3 path", "# one pod per node", "subPath: GLM-5.3-v2", "extraArgs:", "images:"} {
		if !strings.Contains(string(out), kept) {
			t.Errorf("%q is missing:\n%s", kept, out)
		}
	}
	if strings.Contains(string(out), "gone") || strings.Contains(string(out), "# header") {
		t.Errorf("a removed key or the new-file header is there:\n%s", out)
	}
	var got map[string]any
	yaml.Unmarshal(out, &got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reads as %v, want %v", got, want)
	}
	if strings.Index(string(out), "model:") > strings.Index(string(out), "llmisvc:") {
		t.Errorf("the order changed:\n%s", out)
	}
	// A file with no values yet gets the header.
	if out, _ := Patch([]byte("# only a comment\n"), "# header\n", map[string]any{"a": 1.0}); string(out) != "# header\na: 1\n" {
		t.Errorf("new file:\n%s", out)
	}
}

// A change of plain values only is made in the text itself: blank lines,
// flow maps and the columns of comments stay, and the diff is the changed
// lines alone.
func TestPatchChangesPlainValuesInPlace(t *testing.T) {
	original := "# recipe\n\nimages:\n  vllm: registry-internal/vllm:v0.5.5\n\nstorage:\n  models:\n    lvSize: 1200G          # LV created by node-prep\n    requests: {cpu: \"1\", memory: 512Mi}\n"
	want := map[string]any{
		"images":  map[string]any{"vllm": "registry-internal/vllm:v0.5.6"},
		"storage": map[string]any{"models": map[string]any{"lvSize": "1500G", "requests": map[string]any{"cpu": "1", "memory": "512Mi"}}},
	}
	out, err := Patch([]byte(original), "", want)
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Replace(strings.Replace(original, "v0.5.5", "v0.5.6", 1), "1200G ", "1500G ", 1)
	if string(out) != expected {
		t.Fatalf("got\n%s\nwant\n%s", out, expected)
	}
}

// A model copied from another gets object names of its own: the SCC,
// StorageClass and PVs are cluster-wide, and two models that shared them
// would fight over them on a cluster that runs both. Other values stay.
func TestANewModelRenamesItsObjects(t *testing.T) {
	from := Model{Name: "glm53", config: map[string]any{"name": "glm53", "repourl": "x"}}
	values := map[string]any{
		"nameOverride": "glm53", "model": map[string]any{"name": "glm-5.3"},
		"llmisvc": map[string]any{"name": "glm53-b200", "replicas": 2.0},
		"scc":     map[string]any{"name": "glm53-model-serving", "create": true},
		"storage": map[string]any{"className": "local-static", "models": map[string]any{"pvcName": "glm53-models", "lvSize": "1200G"}},
		"lmcache": map[string]any{"name": "glm53-cache", "image": map[string]any{"tag": "v0.5.5"}},
		"s3":      map[string]any{"secretName": "glm53-s3"},
	}
	changes, err := NewModelChanges("glm54", "llm-glm54", from, values)
	if err != nil {
		t.Fatal(err)
	}
	var recipe map[string]any
	yaml.Unmarshal(changes[1].Content, &recipe)
	want := map[string]any{
		"nameOverride": "glm54", "model": map[string]any{"name": "glm-5.3"},
		"llmisvc": map[string]any{"name": "glm54-b200", "replicas": 2.0},
		"scc":     map[string]any{"name": "glm54-model-serving", "create": true},
		"storage": map[string]any{"models": map[string]any{"pvcName": "glm54-models", "lvSize": "1200G"}},
		"lmcache": map[string]any{"name": "glm54-cache", "image": map[string]any{"tag": "v0.5.5"}},
		"s3":      map[string]any{"secretName": "glm53-s3"},
	}
	if !reflect.DeepEqual(recipe, want) {
		t.Fatalf("recipe %v\nwant %v", recipe, want)
	}
	if values["scc"].(map[string]any)["name"] != "glm53-model-serving" {
		t.Fatal("the caller's values changed")
	}
	// Two plain values on one line change from the right.
	out, err := Patch([]byte("r: {cpu: \"1\", memory: 1Gi}\n"), "", map[string]any{"r": map[string]any{"cpu": "2", "memory": "2Gi"}})
	if err != nil || string(out) != "r: {cpu: \"1\", memory: 2Gi}\n" && string(out) != "r: {cpu: \"2\", memory: 2Gi}\n" {
		t.Fatalf("one line: %q %v", out, err)
	}
}

// A quoted value is changed in place too, and a file the hub created is
// patched without re-indenting it: one value, one changed line.
func TestPatchQuotedAndHubWrittenFiles(t *testing.T) {
	values := map[string]any{"llmisvc": map[string]any{"args": []any{"--a", "--b"}, "env": map[string]any{"X": "4"}}}
	created, err := encode("# header\n", values)
	if err != nil {
		t.Fatal(err)
	}
	values["llmisvc"].(map[string]any)["env"] = map[string]any{"X": "3"}
	out, err := Patch(created, "", values)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(string(created), `X: "4"`, `X: "3"`, 1); string(out) != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}
