package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"aigw-ui/internal/gitops"
	"aigw-ui/internal/store"
)

// The models the hub deploys: values files in the Argo CD platform's
// repository (internal/gitops), which the platform deploys with the
// model-deployer chart. The hub writes a file when someone saves here and
// reads every file back, so git stays the source of truth. Nothing here
// reaches a cluster directly.

// deployer is the git repository of the platform (gitops.Repo).
type deployer interface {
	Read(ctx context.Context) (gitops.Snapshot, error)
	Commit(ctx context.Context, changes []gitops.Change, message string) (string, error)
	Where() (repo, branch, dir string)
}

// appStatus reads the Argo CD Applications (gitops.ArgoCD).
type appStatus interface {
	Apps(ctx context.Context) (map[string]gitops.App, error)
}

// WithGitOps turns the Deployments page on. apps may be nil: no Argo CD
// status then. appName is how the platform names a model's Application on
// a cluster ({cluster}, {model}); "" is gitops.DefaultAppName.
func (s *Server) WithGitOps(repo deployer, apps appStatus, appName string) *Server {
	if appName == "" {
		appName = gitops.DefaultAppName
	}
	s.gitops, s.argocd, s.appName = repo, apps, appName
	return s
}

// liveDeployment is what discovery last saw of a deployment's
// LLMInferenceService on its cluster.
type liveDeployment struct {
	ModelID string `json:"model_id"`
	// Instances is its ready instances, nil until it reports them; Desired
	// its replicas.
	Instances  *float64  `json:"instances"`
	Desired    int64     `json:"desired"`
	ObservedAt time.Time `json:"observed_at"`
}

type deploymentView struct {
	gitops.Deployment
	// ClusterID is the cluster in the hub, "" when the hub does not know it.
	ClusterID string `json:"cluster_id"`
	// AppName is its Argo CD Application; App what Argo CD says of it, nil
	// when not read or not made yet.
	AppName string      `json:"app_name"`
	App     *gitops.App `json:"app"`
	// Live is nil while discovery has not seen it on the cluster.
	Live *liveDeployment `json:"live"`
}

type clusterView struct {
	gitops.Cluster
	ClusterID string `json:"cluster_id"`
}

type gitopsView struct {
	Enabled bool   `json:"enabled"`
	Repo    string `json:"repo,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Commit  string `json:"commit,omitempty"`
	// ArgoCD says whether the Applications' status is read.
	ArgoCD      bool             `json:"argocd"`
	Models      []gitops.Model   `json:"models"`
	Clusters    []clusterView    `json:"clusters"`
	Deployments []deploymentView `json:"deployments"`
	// Error is why the repository could not be read; ArgoCDError why the
	// Applications could not.
	Error       string `json:"error,omitempty"`
	ArgoCDError string `json:"argocd_error,omitempty"`
}

// getGitOps returns the models, the clusters and the deployments in git,
// each deployment with what Argo CD and the cluster say of it.
func (s *Server) getGitOps(w http.ResponseWriter, r *http.Request) {
	v := gitopsView{Models: []gitops.Model{}, Clusters: []clusterView{}, Deployments: []deploymentView{}}
	if s.gitops == nil {
		writeJSON(w, http.StatusOK, v)
		return
	}
	v.Enabled, v.ArgoCD = true, s.argocd != nil
	v.Repo, v.Branch, v.Dir = s.gitops.Where()
	snap, err := s.gitops.Read(r.Context())
	if err != nil {
		slog.Warn("read the deployments repository", "err", err)
		v.Error = err.Error()
		writeJSON(w, http.StatusOK, v)
		return
	}
	v.Commit, v.Models = snap.Commit, snap.Models
	clusters, err := s.st.ListClusters(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	ids := make(map[string]string, len(clusters))
	for _, c := range clusters {
		ids[c.Name] = c.ID
	}
	for _, c := range snap.Clusters {
		v.Clusters = append(v.Clusters, clusterView{Cluster: c, ClusterID: ids[c.Name]})
	}
	seen, err := s.st.ListDeployments(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	live := make(map[string]liveDeployment, len(seen))
	for _, d := range seen {
		live[d.ClusterName+"/"+d.Namespace+"/"+d.Name] = liveDeployment{ModelID: d.ModelID, Instances: d.Instances, Desired: d.DecodeReplicas, ObservedAt: d.ObservedAt}
	}
	var apps map[string]gitops.App
	if s.argocd != nil {
		if apps, err = s.argocd.Apps(r.Context()); err != nil {
			v.ArgoCDError = err.Error()
		}
	}
	for _, d := range snap.Deployments {
		dv := deploymentView{Deployment: d, ClusterID: ids[d.Cluster], AppName: gitops.AppName(s.appName, d.Cluster, d.Model)}
		if a, ok := apps[dv.AppName]; ok {
			dv.App = &a
		}
		if l, ok := live[d.Cluster+"/"+d.Namespace+"/"+d.Object]; ok {
			dv.Live = &l
		}
		v.Deployments = append(v.Deployments, dv)
	}
	writeJSON(w, http.StatusOK, v)
}

// getGitOpsBase returns what a model's values are on a cluster before the
// cluster's own layer: where the form of a new deployment starts, so that
// saving it keeps what the site's layers set.
func (s *Server) getGitOpsBase(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.readGitOps(w, r)
	if !ok {
		return
	}
	cluster, model, err := placeOf(snap, r.URL.Query().Get("cluster"), r.URL.Query().Get("model"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"values": snap.Base(cluster, model.Name), "layers": gitops.Layers(cluster, model.Name)})
}

type deploymentBody struct {
	Cluster string `json:"cluster"`
	Model   string `json:"model"`
	// Values are the model's whole values on the cluster; the hub writes
	// what differs from the layers below as the cluster's own.
	Values map[string]any `json:"values"`
	// Revision is the version of the file the change was made on.
	Revision string `json:"revision"`
}

// createDeployment registers a model on a cluster: the directory of the
// model under the cluster's, with the cluster's own values.
func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	var b deploymentBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	s.saveDeployment(w, r, b, "", "deployment.create", "deploy %s on %s")
}

// updateDeployment changes a model's values on a cluster.
func (s *Server) updateDeployment(w http.ResponseWriter, r *http.Request) {
	var b deploymentBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	if b.Revision == "" {
		fail(w, invalid("revision is required: the version of the file the change was made on"))
		return
	}
	b.Cluster, b.Model = r.PathValue("cluster"), r.PathValue("model")
	s.saveDeployment(w, r, b, b.Revision, "deployment.update", "change %s on %s")
}

func (s *Server) saveDeployment(w http.ResponseWriter, r *http.Request, b deploymentBody, base, action, verb string) {
	snap, ok := s.readGitOps(w, r)
	if !ok {
		return
	}
	cluster, model, err := placeOf(snap, b.Cluster, b.Model)
	if err != nil {
		fail(w, err)
		return
	}
	if err := checkModelValues(b.Values, model.Namespace); err != nil {
		fail(w, err)
		return
	}
	change, err := snap.DeploymentChange(cluster, model.Name, b.Values, base)
	if err != nil {
		fail(w, invalid("values: %v", err))
		return
	}
	s.commitGitOps(w, r, []gitops.Change{change}, action, "aigw-ui: "+fmt.Sprintf(verb, model.Name, cluster.Name))
}

// deleteDeployment removes a model's directory from a cluster's; the
// platform then deletes its Application and what it deployed. ?revision=
// is the version of the cluster's file removed.
func (s *Server) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	if s.gitops != nil && r.URL.Query().Get("revision") == "" {
		fail(w, invalid("revision is required: the version of the cluster's file that is removed"))
		return
	}
	snap, ok := s.readGitOps(w, r)
	if !ok {
		return
	}
	for _, d := range snap.Deployments {
		if d.Cluster == r.PathValue("cluster") && d.Model == r.PathValue("model") {
			change := gitops.Change{Path: d.Path, Dir: true, Base: r.URL.Query().Get("revision")}
			s.commitGitOps(w, r, []gitops.Change{change}, "deployment.delete", fmt.Sprintf("aigw-ui: remove %s from %s", d.Model, d.Cluster))
			return
		}
	}
	fail(w, store.ErrNotFound)
}

type recipeBody struct {
	// Name and Namespace name a new model's operator and its
	// projectNamespace; From is the model whose app config it copies.
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	From      string `json:"from"`
	// Values is the recipe.
	Values   map[string]any `json:"values"`
	Revision string         `json:"revision"`
	// Cluster, when set, also deploys the new model there, in the same
	// commit.
	Cluster string `json:"cluster"`
}

// createRecipe adds a model: its operator's app config and recipe, and
// with a cluster its deployment there.
func (s *Server) createRecipe(w http.ResponseWriter, r *http.Request) {
	var b recipeBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	snap, ok := s.readGitOps(w, r)
	if !ok {
		return
	}
	var from *gitops.Model
	for i := range snap.Models {
		if snap.Models[i].Name == b.From {
			from = &snap.Models[i]
		}
		if snap.Models[i].Name == b.Name {
			writeError(w, http.StatusConflict, "a model named "+b.Name+" exists")
			return
		}
	}
	if from == nil {
		fail(w, invalid("from must name an existing model: the new model's app config (chart, sync options) is a copy of its"))
		return
	}
	if err := gitops.CheckModelName(b.Name, b.Namespace, "envoy-ai-gateway-system", "envoy-gateway-system"); err != nil {
		fail(w, invalid("%v", err))
		return
	}
	if err := checkModelValues(b.Values, b.Namespace); err != nil {
		fail(w, err)
		return
	}
	changes, err := gitops.NewModelChanges(b.Name, b.Namespace, *from, b.Values)
	if err != nil {
		fail(w, invalid("values: %v", err))
		return
	}
	message := "aigw-ui: add the model " + b.Name
	if b.Cluster != "" {
		var cluster *gitops.Cluster
		for i := range snap.Clusters {
			if snap.Clusters[i].Name == b.Cluster {
				cluster = &snap.Clusters[i]
			}
		}
		if cluster == nil {
			fail(w, invalid("no directory for cluster %q under sites/: add the cluster to the platform first", b.Cluster))
			return
		}
		with := snap.With(changes[1].Path, b.Values)
		deploy, err := with.DeploymentChange(*cluster, b.Name, with.Base(*cluster, b.Name), "")
		if err != nil {
			fail(w, invalid("values: %v", err))
			return
		}
		changes = append(changes, deploy)
		message += " and deploy it on " + b.Cluster
	}
	s.commitGitOps(w, r, changes, "model.add", message)
}

// updateRecipe rewrites a model's recipe, which every cluster's values
// start from.
func (s *Server) updateRecipe(w http.ResponseWriter, r *http.Request) {
	var b recipeBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	if b.Revision == "" {
		fail(w, invalid("revision is required: the version of the recipe the change was made on"))
		return
	}
	snap, ok := s.readGitOps(w, r)
	if !ok {
		return
	}
	for _, m := range snap.Models {
		if m.Name != r.PathValue("model") {
			continue
		}
		if err := checkModelValues(b.Values, m.Namespace); err != nil {
			fail(w, err)
			return
		}
		change, err := snap.RecipeChange(m, b.Values, b.Revision)
		if err != nil {
			fail(w, invalid("values: %v", err))
			return
		}
		s.commitGitOps(w, r, []gitops.Change{change}, "model.recipe", "aigw-ui: change the recipe of "+m.Name)
		return
	}
	fail(w, store.ErrNotFound)
}

// readGitOps reads the repository, or answers why it cannot.
func (s *Server) readGitOps(w http.ResponseWriter, r *http.Request) (gitops.Snapshot, bool) {
	if s.gitops == nil {
		writeError(w, http.StatusNotFound, "deployments are off: the hub has no git repository (gitops.repo)")
		return gitops.Snapshot{}, false
	}
	snap, err := s.gitops.Read(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "read the deployments repository: "+err.Error())
		return gitops.Snapshot{}, false
	}
	return snap, true
}

// placeOf finds a cluster's directory and a model in the repository.
func placeOf(snap gitops.Snapshot, cluster, model string) (gitops.Cluster, gitops.Model, error) {
	var c *gitops.Cluster
	for i := range snap.Clusters {
		if snap.Clusters[i].Name == cluster {
			c = &snap.Clusters[i]
		}
	}
	if c == nil {
		return gitops.Cluster{}, gitops.Model{}, invalid("no directory for cluster %q under sites/: add the cluster to the platform first", cluster)
	}
	for _, m := range snap.Models {
		if m.Name == model {
			return *c, m, nil
		}
	}
	return gitops.Cluster{}, gitops.Model{}, invalid("no model %q under operators/", model)
}

// checkModelValues refuses values the chart would refuse, and a namespace
// other than the app's: the chart's objects would go elsewhere than Argo CD
// looks for them.
func checkModelValues(values map[string]any, namespace string) error {
	if values == nil {
		return invalid("values are required")
	}
	if err := gitops.CheckValues(values); err != nil {
		return invalid("%v", err)
	}
	if ns, _ := values["namespace"].(string); ns != namespace {
		return invalid("namespace is %q, but the model's Argo CD app deploys to %q (projectNamespace)", ns, namespace)
	}
	return nil
}

// commitGitOps commits changes and records the commit in the task log, or
// answers why it was not made.
func (s *Server) commitGitOps(w http.ResponseWriter, r *http.Request, changes []gitops.Change, action, message string) {
	commit, err := s.gitops.Commit(r.Context(), changes, message)
	switch {
	case errors.Is(err, gitops.ErrConflict):
		writeError(w, http.StatusConflict, "the files changed in git since they were opened (or already exist): open them again")
	case err != nil:
		s.log(r, action, message, false, err.Error())
		writeError(w, http.StatusBadGateway, "commit to the deployments repository: "+err.Error())
	default:
		s.log(r, action, message, true, "commit "+commit+"; Argo CD applies it at its next refresh")
		writeJSON(w, http.StatusOK, map[string]string{"commit": commit})
	}
}
