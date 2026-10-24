package gitops

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"
)

// The values repository of the Argo CD platform (argocd-day2-platform), as
// the hub reads and writes it. Config.Dir is its root.
//
//	operators/<model>/<model>.yaml     app config: chart repo and revision,
//	                                   projectNamespace, sync options
//	operators/<model>/values.yaml      the model's recipe
//	sites/<site>/<env>/mces/<mce>/<cluster>/<model>/values.yaml
//	                                   the model on one cluster: the
//	                                   directory registers it, the file is the
//	                                   cluster's own layer
//
// A model is an operator whose values hold model.name; the other operators
// (gpu, envoy-ai, ...) are the platform's and the hub leaves them alone.

// Model is one model's operator: its app config and recipe.
type Model struct {
	// Name is the operator's name; Namespace its projectNamespace.
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Chart is where its chart comes from: repourl, targetRevision and, on
	// the lab, chartPath.
	Chart string `json:"chart"`
	// ModelName is the recipe's model.name: what tenants call.
	ModelName string `json:"model_name"`
	// Values is the recipe, operators/<model>/values.yaml.
	Values map[string]any `json:"values"`
	// Revision and ConfigRevision are the recipe's and the app config's git
	// blobs; a change names the version it was made on.
	Revision       string `json:"revision"`
	ConfigRevision string `json:"config_revision"`
	Path           string `json:"path"`
	// config is the app config as read.
	config map[string]any
}

// Cluster is a cluster's directory: sites/<site>/<env>/mces/<mce>/<cluster>.
type Cluster struct {
	Name string `json:"name"`
	Site string `json:"site"`
	Env  string `json:"env"`
	MCE  string `json:"mce"`
	Path string `json:"path"`
}

// Deployment is one model on one cluster.
type Deployment struct {
	Cluster string `json:"cluster"`
	// Model is the operator; ModelName what tenants call; Object the name
	// of its LLMInferenceService.
	Model     string `json:"model"`
	ModelName string `json:"model_name"`
	Object    string `json:"object"`
	Namespace string `json:"namespace"`
	// Values is what Argo CD renders the chart with: every layer merged.
	// Override is the cluster's own layer.
	Values   map[string]any `json:"values"`
	Override map[string]any `json:"override"`
	// Layers are the files that hold values for it, lowest first.
	Layers []string `json:"layers"`
	// Revision is the cluster's own file's git blob ("" when the directory
	// has none).
	Revision string `json:"revision"`
	Path     string `json:"path"`
	// Problem says why Argo CD cannot deploy it as it is (a file written
	// by hand), "" when nothing is wrong.
	Problem string `json:"problem,omitempty"`
}

// Snapshot is what the repository holds at one commit.
type Snapshot struct {
	Commit      string       `json:"commit"`
	Models      []Model      `json:"models"`
	Clusters    []Cluster    `json:"clusters"`
	Deployments []Deployment `json:"deployments"`
	// files are the values files read, by path relative to Config.Dir, and
	// raw what they hold (a change patches it, keeping its comments).
	files map[string]map[string]any
	raw   map[string][]byte
}

// Read returns the models, clusters and deployments at the branch's head,
// fetched at most fetchEvery ago.
func (r *Repo) Read(ctx context.Context) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.sync(ctx, false); err != nil {
		return Snapshot{}, err
	}
	commit, err := r.head(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	root := filepath.Join(r.dir, filepath.FromSlash(r.cfg.Dir))
	blob := func(rel string) (string, error) { return r.blob(ctx, r.path(rel)) }
	return readLayout(root, commit, blob)
}

// ReadDir reads the layout from a directory that is not a clone (the API's
// tests), with each file's blob ID computed as git computes it.
func ReadDir(root, commit string) (Snapshot, error) {
	return readLayout(root, commit, func(rel string) (string, error) {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return BlobID(raw), nil
	})
}

// BlobID is git's object ID of a file with this content.
func BlobID(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// readLayout reads the layout under root. blob gives a file's git blob.
func readLayout(root, commit string, blob func(rel string) (string, error)) (Snapshot, error) {
	snap := Snapshot{Commit: commit, Models: []Model{}, Clusters: []Cluster{}, Deployments: []Deployment{},
		files: map[string]map[string]any{}, raw: map[string][]byte{}}
	read := func(rel string) map[string]any {
		if v, ok := snap.files[rel]; ok {
			return v
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil
		}
		v := map[string]any{}
		if yaml.Unmarshal(raw, &v) != nil || v == nil {
			v = map[string]any{}
		}
		snap.files[rel], snap.raw[rel] = v, raw
		return v
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}

	ops, _ := filepath.Glob(filepath.Join(root, "operators", "*"))
	sort.Strings(ops)
	byName := map[string]*Model{}
	for _, dir := range ops {
		name := filepath.Base(dir)
		configRel := "operators/" + name + "/" + name + ".yaml"
		config := read(configRel)
		if config == nil {
			continue
		}
		m := Model{Name: name, config: config, Path: "operators/" + name + "/values.yaml"}
		m.Values = read(m.Path)
		if m.Values == nil {
			m.Values = map[string]any{}
		}
		m.Namespace, _ = config["projectNamespace"].(string)
		repo, _ := config["repourl"].(string)
		rev, _ := config["targetRevision"].(string)
		m.Chart = repo + "@" + rev
		if p, _ := config["chartPath"].(string); p != "" {
			m.Chart += " (" + p + ")"
		}
		m.ModelName = str(m.Values, "model", "name")
		var err error
		if m.Revision, err = blob(m.Path); err != nil {
			return Snapshot{}, err
		}
		if m.ConfigRevision, err = blob(configRel); err != nil {
			return Snapshot{}, err
		}
		byName[name] = &m
	}

	dirs, _ := filepath.Glob(filepath.Join(root, "sites", "*", "*", "mces", "*", "*"))
	sort.Strings(dirs)
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		rel, _ := filepath.Rel(root, dir)
		seg := strings.Split(filepath.ToSlash(rel), "/")
		c := Cluster{Site: seg[1], Env: seg[2], MCE: seg[4], Name: seg[5], Path: filepath.ToSlash(rel)}
		snap.Clusters = append(snap.Clusters, c)
		for _, name := range sortedKeys(byName) {
			m := byName[name]
			if !exists(c.Path + "/" + name) {
				continue
			}
			d := Deployment{Cluster: c.Name, Model: name, Namespace: m.Namespace, Path: c.Path + "/" + name + "/values.yaml"}
			for _, l := range Layers(c, name) {
				if v := read(l); v != nil {
					d.Layers = append(d.Layers, l)
				}
			}
			d.Values = snap.merged(d.Layers)
			d.Override = read(d.Path)
			if d.Override == nil {
				d.Override = map[string]any{}
			}
			d.ModelName = str(d.Values, "model", "name")
			if d.ModelName == "" {
				// Not a model: an operator of the platform's.
				continue
			}
			d.Object = ObjectName(d.Values)
			var err error
			if d.Revision, err = blob(d.Path); err != nil {
				return Snapshot{}, err
			}
			if ns := str(d.Values, "namespace"); ns != m.Namespace {
				d.Problem = fmt.Sprintf("the values' namespace is %q, but the app's projectNamespace is %q", ns, m.Namespace)
			}
			if m.ModelName == "" {
				m.ModelName = d.ModelName
			}
			snap.Deployments = append(snap.Deployments, d)
		}
	}
	for _, name := range sortedKeys(byName) {
		if m := byName[name]; m.ModelName != "" {
			snap.Models = append(snap.Models, *m)
		}
	}
	return snap, nil
}

// Layers are the values files of a model on a cluster, lowest first, as the
// platform's deploy chart layers them (operators/<model>/ocp-versions/ is
// left out: the hub does not know a cluster's OpenShift version).
func Layers(c Cluster, model string) []string {
	site, env := "sites/"+c.Site, "sites/"+c.Site+"/"+c.Env
	defaults := "defaults/hosted-clusters/" + model
	mce := env + "/mces/" + c.MCE
	return []string{
		"operators/" + model + "/values.yaml",
		site + "/values.yaml",
		env + "/values.yaml",
		defaults + "/values.yaml",
		defaults + "/values-" + c.Env + ".yaml",
		defaults + "/values-" + c.Name + ".yaml",
		mce + "/values.yaml",
		mce + "/" + c.Name + "/values.yaml",
		mce + "/" + c.Name + "/" + model + "/values.yaml",
	}
}

// merged is the layers' values merged in order.
func (s Snapshot) merged(layers []string) map[string]any {
	out := map[string]any{}
	for _, l := range layers {
		out = Merge(out, s.files[l])
	}
	return out
}

// Base is what a model's values are on a cluster without the cluster's own
// layer: what an override is computed against.
func (s Snapshot) Base(c Cluster, model string) map[string]any {
	layers := Layers(c, model)
	return s.merged(layers[:len(layers)-1])
}

// With is the snapshot with one values file as it will be: a new model's
// recipe, before a deployment of it is computed against it.
func (s Snapshot) With(rel string, values map[string]any) Snapshot {
	files := make(map[string]map[string]any, len(s.files)+1)
	for k, v := range s.files {
		files[k] = v
	}
	files[rel] = values
	s.files = files
	return s
}

// Merge is Helm's merge of values files: maps merge key by key, anything
// else replaces, and null removes the key.
func Merge(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		switch {
		case v == nil:
			delete(out, k)
		case isMap(v) && isMap(out[k]):
			out[k] = Merge(out[k].(map[string]any), v.(map[string]any))
		default:
			out[k] = v
		}
	}
	return out
}

// Override is the smallest layer that makes base into want under Merge:
// what differs, and null for what want leaves out.
func Override(base, want map[string]any) map[string]any {
	out := map[string]any{}
	for k, w := range want {
		b, ok := base[k]
		if isMap(w) && isMap(b) {
			if d := Override(b.(map[string]any), w.(map[string]any)); len(d) > 0 {
				out[k] = d
			}
			continue
		}
		if !ok || !reflect.DeepEqual(b, w) {
			out[k] = w
		}
	}
	for k := range base {
		if _, ok := want[k]; !ok {
			out[k] = nil
		}
	}
	return out
}

func isMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func str(v map[string]any, path ...string) string {
	var cur any = v
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}

func sortedKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var notSlug = regexp.MustCompile(`[^a-z0-9-]`)

// ObjectName is the name the chart gives the LLMInferenceService:
// llmisvc.name, or the slug of model.name.
func ObjectName(values map[string]any) string {
	if n := str(values, "llmisvc", "name"); n != "" {
		return n
	}
	return strings.Trim(notSlug.ReplaceAllString(strings.ToLower(str(values, "model", "name")), "-"), "-")
}

// The header of every file the hub writes.
const (
	clusterHeader = "# Written by aigw-ui (Deployments): this cluster's own values for the\n" +
		"# model, on top of operators/%s/values.yaml and the site's layers. aigw-ui\n" +
		"# reads it back: a change by hand shows on its page.\n"
	recipeHeader = "# The recipe of %s for charts/model-deployer, the first layer on every\n" +
		"# cluster. Written by aigw-ui (Deployments); a change by hand shows there.\n"
)

// encode is a values file: its header, then the values with keys in
// alphabetical order ({} writes the header alone).
func encode(header string, values map[string]any) ([]byte, error) {
	if len(values) == 0 {
		return []byte(header), nil
	}
	// The same style as Patch writes (lists indented under their key), so
	// the first patch of a file the hub made does not re-indent it.
	var out bytes.Buffer
	out.WriteString(header)
	enc := yamlv3.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(values); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// DeploymentChange is the change that makes a model's values on a cluster
// want: the cluster's own layer, written as the difference from the layers
// below it. base is the file's version the change was made on.
func (s Snapshot) DeploymentChange(c Cluster, model string, want map[string]any, base string) (Change, error) {
	path := c.Path + "/" + model + "/values.yaml"
	content, err := Patch(s.raw[path], fmt.Sprintf(clusterHeader, model), Override(s.Base(c, model), want))
	if err != nil {
		return Change{}, err
	}
	return Change{Path: path, Content: content, Base: base}, nil
}

// RecipeChange rewrites a model's recipe, keeping its comments.
func (s Snapshot) RecipeChange(m Model, values map[string]any, base string) (Change, error) {
	content, err := Patch(s.raw[m.Path], fmt.Sprintf(recipeHeader, m.Name), values)
	if err != nil {
		return Change{}, err
	}
	return Change{Path: m.Path, Content: content, Base: base}, nil
}

// objectNames are the values that name a model's own objects. Some are
// cluster-wide (SCC, StorageClass, PVs): a copy that kept them would fight
// its source over them on a cluster that runs both.
var objectNames = [][]string{
	{"nameOverride"}, {"llmisvc", "name"}, {"scc", "name"}, {"lmcache", "name"}, {"lmcache", "serviceAccountName"},
	{"storage", "className"}, {"storage", "models", "pvcName"}, {"storage", "cache", "pvcName"}, {"storage", "l2", "pvcName"},
	{"aiperf", "name"}, {"peer", "name"}, {"peer", "healthName"},
}

// renamed is values with every object name that starts with from starting
// with to instead.
func renamed(values map[string]any, from, to string) map[string]any {
	out := Merge(map[string]any{}, values)
	for _, path := range objectNames {
		cur := out
		for i, key := range path {
			if i < len(path)-1 {
				next, ok := cur[key].(map[string]any)
				if !ok {
					break
				}
				next = Merge(map[string]any{}, next)
				cur[key], cur = next, next
				continue
			}
			switch s, ok := cur[key].(string); {
			case ok && strings.HasPrefix(s, from):
				cur[key] = to + strings.TrimPrefix(s, from)
			case ok && s != "" && key == "className":
				// A StorageClass named for no model (local-static) is the
				// source's: the copy takes the chart's default,
				// <name>-local-static.
				delete(cur, key)
			}
		}
	}
	return out
}

// NewModelChanges add a model's operator: its app config, a copy of from's
// with the name and projectNamespace changed, and its recipe, whose object
// names that start with from's name start with the new name.
func NewModelChanges(name, namespace string, from Model, values map[string]any) ([]Change, error) {
	values = renamed(values, from.Name, name)
	config := make(map[string]any, len(from.config))
	for k, v := range from.config {
		config[k] = v
	}
	config["name"], config["projectNamespace"] = name, namespace
	header := fmt.Sprintf("# Written by aigw-ui (Deployments): the app config of %s, from %s's.\n", name, from.Name)
	raw, err := encode("", config)
	if err != nil {
		return nil, err
	}
	recipe, err := Snapshot{}.RecipeChange(Model{Name: name, Path: "operators/" + name + "/values.yaml"}, values, "")
	if err != nil {
		return nil, err
	}
	return []Change{{Path: "operators/" + name + "/" + name + ".yaml", Content: append([]byte(header), raw...)}, recipe}, nil
}
