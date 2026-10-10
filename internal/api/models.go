package api

import (
	"net/http"
	"strings"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// modelBody is the body of a model create or update. On an update every field
// that is left out keeps its stored value; "endpoints": [] removes the manual
// endpoints, leaving the field out keeps them.
type modelBody struct {
	Name           string           `json:"name"`
	DefaultLimit   *int64           `json:"default_limit"`
	DefaultWindow  *string          `json:"default_window"`
	CostExpression *string          `json:"cost_expression"`
	Endpoints      []store.Endpoint `json:"endpoints"`
}

// input validates the body. current is the stored model on an update and nil
// on a create.
func (b *modelBody) input(name string, current *store.Model) (store.ModelInput, error) {
	in := store.ModelInput{Name: name, DefaultLimit: 1, DefaultWindow: "1d"}
	if current != nil {
		in.DefaultLimit, in.DefaultWindow, in.CostExpression = current.DefaultLimit, current.DefaultWindow, current.CostExpression
		in.KeepEndpoints = b.Endpoints == nil
	}
	if b.DefaultLimit != nil {
		in.DefaultLimit = *b.DefaultLimit
	}
	if b.DefaultWindow != nil {
		in.DefaultWindow = *b.DefaultWindow
	}
	if b.CostExpression != nil {
		in.CostExpression = strings.TrimSpace(*b.CostExpression)
	}
	switch {
	case !modelName.MatchString(name):
		return store.ModelInput{}, invalid("name must start with a letter or digit and use only letters, digits and . _ : / -")
	case render.Slug(name) == "":
		return store.ModelInput{}, invalid("name must contain a letter or digit")
	case in.DefaultLimit < 1:
		return store.ModelInput{}, invalid("default_limit must be at least 1")
	case !validWindow(in.DefaultWindow):
		return store.ModelInput{}, invalid("default_window must be 1m, 1h or 1d")
	}
	if err := validCostExpression(in.CostExpression); err != nil {
		return store.ModelInput{}, err
	}
	// Discovered endpoints come from the clusters and cannot be set here.
	manual := b.Endpoints[:0:0]
	for _, e := range b.Endpoints {
		if e.Source != store.SourceDiscovered {
			manual = append(manual, e)
		}
	}
	b.Endpoints = manual

	seen := map[string]bool{}
	for _, e := range b.Endpoints {
		switch {
		case e.ClusterID == "":
			return store.ModelInput{}, invalid("each endpoint needs a cluster_id")
		case seen[e.ClusterID]:
			return store.ModelInput{}, invalid("a model can have one endpoint per cluster")
		case !hostname.MatchString(e.Host):
			return store.ModelInput{}, invalid("endpoint host %q is not a valid hostname or IP", e.Host)
		case e.Port < 1 || e.Port > 65535:
			return store.ModelInput{}, invalid("endpoint port must be between 1 and 65535")
		case e.UpstreamModel != "" && !modelName.MatchString(e.UpstreamModel):
			return store.ModelInput{}, invalid("upstream_model %q is not a valid model name", e.UpstreamModel)
		}
		seen[e.ClusterID] = true
	}
	in.Endpoints = b.Endpoints
	return in, nil
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	out, err := s.st.ListModels(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	clusters, err := s.st.ListClusters(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	fleet := map[string]bool{}
	// peers holds the names of the clusters an entry route can send to.
	peers := map[string]bool{}
	for _, c := range clusters {
		fleet[c.ID] = c.FleetEnabled
		peers[c.Name] = c.FleetEnabled && c.PeerHost != ""
	}
	bestEffort, err := s.st.BestEffortTenants(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	for i := range out {
		out[i].Warnings = s.modelWarnings(out[i], fleet, peers, len(bestEffort[out[i].ID]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createModel(w http.ResponseWriter, r *http.Request) {
	var b modelBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	in, err := b.input(b.Name, nil)
	if err != nil {
		fail(w, err)
		return
	}
	id, err := s.st.CreateModel(r.Context(), in)
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "model.add", "Added model "+in.Name)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// updateModel ignores the name in the body: a model cannot be renamed.
func (s *Server) updateModel(w http.ResponseWriter, r *http.Request) {
	var b modelBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	id := r.PathValue("id")
	current, err := s.st.GetModel(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	in, err := b.input(current.Name, &current)
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.st.UpdateModel(r.Context(), id, in); err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "model.update", "Changed model "+in.Name)
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	// The name is read first: after the delete it is gone.
	m, err := s.st.GetModel(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.st.DeleteModel(r.Context(), m.ID); err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "model.delete", "Deleted model "+m.Name+" and its quotas")
	w.WriteHeader(http.StatusNoContent)
}
