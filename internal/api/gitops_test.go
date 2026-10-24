package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"aigw-ui/internal/gitops"
	"aigw-ui/internal/store"
)

// dirRepo stands in for the git repository: the platform's layout in a
// directory, and each commit applied to it, refused like git when a file is
// no longer the version the change was made on.
type dirRepo struct {
	root     string
	messages []string
}

func (d *dirRepo) Read(context.Context) (gitops.Snapshot, error) {
	return gitops.ReadDir(d.root, "abc")
}
func (d *dirRepo) Where() (string, string, string) {
	return "https://git.example/popai.git", "main", "gitops/app-of-apps"
}
func (d *dirRepo) Commit(_ context.Context, changes []gitops.Change, message string) (string, error) {
	for _, c := range changes {
		have := ""
		if raw, err := os.ReadFile(filepath.Join(d.root, c.Path)); err == nil {
			have = gitops.BlobID(raw)
		}
		if have != c.Base {
			return "", gitops.ErrConflict
		}
	}
	for _, c := range changes {
		full := filepath.Join(d.root, c.Path)
		switch {
		case c.Dir:
			os.RemoveAll(filepath.Dir(full))
		case c.Content == nil:
			os.Remove(full)
		default:
			os.MkdirAll(filepath.Dir(full), 0o755)
			os.WriteFile(full, c.Content, 0o644)
		}
	}
	d.messages = append(d.messages, message)
	return "c0ffee", nil
}

type fakeApps map[string]gitops.App

func (f fakeApps) Apps(context.Context) (map[string]gitops.App, error) { return f, nil }

// The Deployments page lists the models, the clusters and each model on
// each cluster from the platform's repository, with Argo CD's view and what
// discovery last saw. A save writes the cluster's own layer only after the
// checks pass: a cluster directory and a model that exist, values the chart
// takes, the app's namespace. A change names the version it was made on
// (409 for an old one). A new model copies an existing one's app config and
// can be deployed in the same commit; a removal takes the cluster's
// directory of the model away.
func TestDeploymentsInGit(t *testing.T) {
	st, ctx := openStore(t)
	lab, err := st.CreateCluster(ctx, store.Cluster{Name: "lab", Namespace: "envoy-ai-gateway-system", GatewayName: "envoy-ai-gateway"}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	modelID, err := st.CreateModel(ctx, store.ModelInput{Name: "qwen-tiny", DefaultLimit: 1, DefaultWindow: "1d",
		Endpoints: []store.Endpoint{{ClusterID: lab.ID, Host: "qwen-tiny.vllm-lab.svc", Port: 8000, UpstreamModel: "qwen-tiny"}}})
	if err != nil {
		t.Fatal(err)
	}
	one := 1.0
	if err := st.ApplyCapacity(ctx, lab.ID, []store.Capacity{{Model: "qwen-tiny", Capacity: 1, Step: 1, Known: true,
		Deployments: []store.Deployment{{Namespace: "vllm-lab", Name: "qwen-tiny", Topology: "single", Instances: &one, DecodeReplicas: 1}}}}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for path, content := range map[string]string{
		"operators/qwen-tiny/qwen-tiny.yaml":                        "name: qwen-tiny\nprojectNamespace: vllm-lab\nrepourl: https://git.example/doomsday.git\ntargetRevision: main\n",
		"operators/qwen-tiny/values.yaml":                           "namespace: vllm-lab\nmodel:\n  name: qwen-tiny\n  uri: hf://Qwen/Qwen2.5-0.5B-Instruct\nllmisvc:\n  replicas: 1\n",
		"sites/site-lab/lab/mces/mce-lab/lab/qwen-tiny/values.yaml": "",
		"sites/site-a/prep/mces/mce-prep/prep/gpu/values.yaml":      "",
	} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(path)), 0o755)
		os.WriteFile(filepath.Join(root, path), []byte(content), 0o644)
	}
	repo := &dirRepo{root: root}
	s := (&Server{st: st}).WithGitOps(repo, fakeApps{"popai-lab-qwen-tiny-deploy": {Sync: "Synced", Health: "Healthy"}}, "")

	view := func() gitopsView {
		t.Helper()
		w := httptest.NewRecorder()
		s.getGitOps(w, httptest.NewRequest(http.MethodGet, "/api/v1/gitops", nil))
		var v gitopsView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return v
	}
	v := view()
	if !v.Enabled || !v.ArgoCD || v.Dir != "gitops/app-of-apps" || len(v.Models) != 1 || len(v.Clusters) != 2 || len(v.Deployments) != 1 {
		t.Fatalf("view: %+v", v)
	}
	d := v.Deployments[0]
	if d.ClusterID != lab.ID || d.AppName != "popai-lab-qwen-tiny-deploy" || d.App == nil || d.App.Health != "Healthy" ||
		d.Live == nil || *d.Live.Instances != 1 || d.Live.ModelID != modelID {
		t.Fatalf("qwen-tiny: %+v app %+v live %+v", d, d.App, d.Live)
	}

	send := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		rest := strings.Split(strings.Split(path, "?")[0], "/")
		w := httptest.NewRecorder()
		switch {
		case method == http.MethodPost && strings.HasSuffix(path, "/deployments"):
			s.createDeployment(w, r)
		case method == http.MethodPost:
			s.createRecipe(w, r)
		case strings.Contains(path, "/models/"):
			r.SetPathValue("model", rest[len(rest)-1])
			s.updateRecipe(w, r)
		default:
			r.SetPathValue("cluster", rest[len(rest)-2])
			r.SetPathValue("model", rest[len(rest)-1])
			if method == http.MethodPut {
				s.updateDeployment(w, r)
			} else {
				s.deleteDeployment(w, r)
			}
		}
		return w
	}
	values := func(replicas int, extra string) string {
		return `{"namespace":"vllm-lab","model":{"name":"qwen-tiny","uri":"hf://Qwen/Qwen2.5-0.5B-Instruct"},"llmisvc":{"replicas":` +
			string(rune('0'+replicas)) + extra + `}}`
	}
	for want, call := range map[string][3]string{
		"no directory for cluster": {"POST", "/api/v1/gitops/deployments", `{"cluster":"gone","model":"qwen-tiny","values":` + values(1, "") + `}`},
		"no model":                 {"POST", "/api/v1/gitops/deployments", `{"cluster":"prep","model":"glm","values":` + values(1, "") + `}`},
		"not one plain":            {"PUT", "/api/v1/gitops/deployments/lab/qwen-tiny", `{"revision":"x","values":` + values(1, `,"args":["--a b"]`) + `}`},
		"projectNamespace":         {"PUT", "/api/v1/gitops/deployments/lab/qwen-tiny", `{"revision":"x","values":{"namespace":"other","model":{"name":"q","uri":"hf://a/b"}}}`},
		"revision is required":     {"DELETE", "/api/v1/gitops/deployments/lab/qwen-tiny", ""},
		"from must name":           {"POST", "/api/v1/gitops/models", `{"name":"qwen-small","namespace":"llm-small","from":"none","values":{}}`},
		"the gateway's":            {"POST", "/api/v1/gitops/models", `{"name":"qwen-small","namespace":"envoy-ai-gateway-system","from":"qwen-tiny","values":{}}`},
	} {
		if w := send(call[0], call[1], call[2]); w.Code != 400 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s", want, w.Code, w.Body)
		}
	}
	if len(repo.messages) != 0 {
		t.Fatalf("refused changes were committed: %v", repo.messages)
	}

	// A new deployment's form starts from the layers below the cluster's.
	w := httptest.NewRecorder()
	s.getGitOpsBase(w, httptest.NewRequest(http.MethodGet, "/api/v1/gitops/base?cluster=prep&model=qwen-tiny", nil))
	if !strings.Contains(w.Body.String(), `"replicas":1`) || !strings.Contains(w.Body.String(), "operators/qwen-tiny/values.yaml") {
		t.Fatalf("base: %d %s", w.Code, w.Body)
	}

	// Two replicas on the lab: the cluster's file holds just that.
	if w := send("PUT", "/api/v1/gitops/deployments/lab/qwen-tiny", `{"revision":"stale","values":`+values(2, "")+`}`); w.Code != 409 {
		t.Fatalf("an old revision: %d %s", w.Code, w.Body)
	}
	if w := send("PUT", "/api/v1/gitops/deployments/lab/qwen-tiny", `{"revision":"`+d.Revision+`","values":`+values(2, "")+`}`); w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	raw, _ := os.ReadFile(filepath.Join(root, d.Path))
	var layer map[string]any
	yaml.Unmarshal(raw, &layer)
	if len(layer) != 1 || layer["llmisvc"].(map[string]any)["replicas"] != 2.0 || repo.messages[0] != "aigw-ui: change qwen-tiny on lab" {
		t.Fatalf("the lab's layer: %v (%q)", layer, repo.messages)
	}

	// A new model from qwen-tiny's app config, deployed on prep at once.
	if w := send("POST", "/api/v1/gitops/models", `{"name":"qwen-small","namespace":"llm-small","from":"qwen-tiny","cluster":"prep",`+
		`"values":{"namespace":"llm-small","model":{"name":"qwen-small","uri":"hf://a/b"}}}`); w.Code != 200 {
		t.Fatalf("new model: %d %s", w.Code, w.Body)
	}
	v = view()
	if len(v.Models) != 2 || len(v.Deployments) != 2 || v.Deployments[0].Model != "qwen-small" || v.Deployments[0].Cluster != "prep" ||
		repo.messages[1] != "aigw-ui: add the model qwen-small and deploy it on prep" {
		t.Fatalf("after the new model: %+v %q", v.Deployments, repo.messages)
	}
	if w := send("POST", "/api/v1/gitops/models", `{"name":"qwen-small","namespace":"llm-small","from":"qwen-tiny","values":{}}`); w.Code != 409 {
		t.Fatalf("the same model twice: %d", w.Code)
	}
	// Its recipe changed for every cluster.
	recipe := v.Models[0]
	if w := send("PUT", "/api/v1/gitops/models/qwen-small", `{"revision":"`+recipe.Revision+`","values":{"namespace":"llm-small","model":{"name":"qwen-small","uri":"hf://a/c"}}}`); w.Code != 200 {
		t.Fatalf("recipe: %d %s", w.Code, w.Body)
	}
	small := view().Deployments[0]
	if small.Values["model"].(map[string]any)["uri"] != "hf://a/c" {
		t.Fatalf("recipe change not on prep: %v", small.Values)
	}
	if w := send("DELETE", "/api/v1/gitops/deployments/prep/qwen-small?revision="+small.Revision, ""); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if v := view(); len(v.Deployments) != 1 || len(v.Models) != 2 {
		t.Fatalf("after the removal: %d deployments, %d models", len(v.Deployments), len(v.Models))
	}
	if tasks, err := st.ListTasks(ctx, 10); err != nil || len(tasks) != 4 {
		t.Fatalf("task log: %d tasks, %v", len(tasks), err)
	}

	// Off without a repository: the page says so, a change is refused.
	off := &Server{st: st}
	w = httptest.NewRecorder()
	off.getGitOps(w, httptest.NewRequest(http.MethodGet, "/api/v1/gitops", nil))
	if !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("off: %s", w.Body)
	}
	w = httptest.NewRecorder()
	off.createDeployment(w, httptest.NewRequest(http.MethodPost, "/api/v1/gitops/deployments", strings.NewReader(`{"cluster":"lab","model":"q","values":{}}`)))
	if w.Code != 404 {
		t.Fatalf("off create: %d", w.Code)
	}
}

// The vLLM command is checked before anything is committed: the check
// route answers what a save would find, a save with an error is refused
// with the checks (nothing committed), and a recipe edit is checked on each
// cluster that runs the model, with the cluster's own layer.
func TestFlagsAreCheckedBeforeASave(t *testing.T) {
	root := t.TempDir()
	for path, content := range map[string]string{
		"operators/qwen-tiny/qwen-tiny.yaml": "name: qwen-tiny\nprojectNamespace: vllm-lab\n",
		"operators/qwen-tiny/values.yaml": "namespace: vllm-lab\nimages:\n  vllm: docker.io/vllm/vllm-openai-cpu:v0.32.0\n" +
			"model:\n  name: qwen-tiny\n  uri: hf://Qwen/Qwen2.5-0.5B-Instruct\nllmisvc:\n  args: [--enable-prompt-tokens-details, --enable-force-include-usage]\n",
		"sites/site-lab/lab/mces/mce-lab/lab/qwen-tiny/values.yaml": "llmisvc:\n  extraArgs: [--max-num-seqs=8]\n",
	} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(path)), 0o755)
		os.WriteFile(filepath.Join(root, path), []byte(content), 0o644)
	}
	repo := &dirRepo{root: root}
	s := (&Server{}).WithGitOps(repo, nil, "")
	snap, err := repo.Read(context.Background())
	if err != nil || len(snap.Deployments) != 1 {
		t.Fatalf("snapshot: %v %+v", err, snap.Deployments)
	}
	values := snap.Deployments[0].Values
	llmisvc := values["llmisvc"].(map[string]any)
	llmisvc["extraArgs"] = []any{"--max-num-seqs=8", "--enable-prefix-cache"}
	body, _ := json.Marshal(map[string]any{"cluster": "lab", "model": "qwen-tiny", "values": values, "revision": snap.Deployments[0].Revision})

	type answer struct {
		Error  string                `json:"error"`
		Checks []gitops.ClusterCheck `json:"checks"`
	}
	call := func(h http.HandlerFunc, method, path string, body []byte, pathValues ...string) (int, answer) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(string(body)))
		for i := 0; i+1 < len(pathValues); i += 2 {
			r.SetPathValue(pathValues[i], pathValues[i+1])
		}
		w := httptest.NewRecorder()
		h(w, r)
		var a answer
		json.Unmarshal(w.Body.Bytes(), &a)
		return w.Code, a
	}

	check, _ := json.Marshal(map[string]any{"cluster": "lab", "model": "qwen-tiny", "values": values})
	code, a := call(s.checkGitOps, http.MethodPost, "/api/v1/gitops/check", check)
	if code != 200 || len(a.Checks) != 1 || len(a.Checks[0].Errors()) != 1 || !a.Checks[0].Exact || a.Checks[0].Command[0].Text != "/mnt/models" {
		t.Fatalf("check: %d %+v", code, a)
	}
	if p := a.Checks[0].Errors()[0]; p.Source != "llmisvc.extraArgs" || !strings.Contains(p.Message, "did you mean --enable-prefix-caching?") {
		t.Fatalf("problem: %+v", p)
	}

	code, a = call(s.updateDeployment, http.MethodPut, "/api/v1/gitops/deployments/lab/qwen-tiny", body, "cluster", "lab", "model", "qwen-tiny")
	if code != 400 || !strings.HasPrefix(a.Error, "the vLLM command would not start on lab: --enable-prefix-cache is not a flag") || len(a.Checks) != 1 || len(repo.messages) != 0 {
		t.Fatalf("save: %d %+v, commits %v", code, a, repo.messages)
	}

	// The recipe sets --max-num-seqs too: on the lab its value wins over the
	// cluster's (llmisvc.args comes after llmisvc.extraArgs).
	recipe := snap.Models[0].Values
	recipe["llmisvc"] = map[string]any{"args": []any{"--enable-prompt-tokens-details", "--enable-force-include-usage", "--max-num-seqs=64"}}
	rbody, _ := json.Marshal(map[string]any{"values": recipe, "revision": snap.Models[0].Revision})
	code, a = call(s.updateRecipe, http.MethodPut, "/api/v1/gitops/models/qwen-tiny", rbody, "model", "qwen-tiny")
	if code != 400 || !strings.HasPrefix(a.Error, "the vLLM command would not start on lab: --max-num-seqs is set twice") || len(repo.messages) != 0 {
		t.Fatalf("recipe: %d %+v", code, a)
	}
	cbody, _ := json.Marshal(map[string]any{"model": "qwen-tiny", "recipe": true, "values": recipe})
	if code, a = call(s.checkGitOps, http.MethodPost, "/api/v1/gitops/check", cbody); code != 200 || len(a.Checks) != 1 || a.Checks[0].Cluster != "lab" || len(a.Checks[0].Errors()) != 1 {
		t.Fatalf("recipe check: %d %+v", code, a)
	}
}
