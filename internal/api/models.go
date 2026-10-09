package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
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

var (
	costToken = regexp.MustCompile(`^(?:[a-z_]+|[0-9]+(?:\.[0-9]+)?u?|[-+*/()]|\s+)`)
	costWord  = regexp.MustCompile(`^[a-z_]+$`)
	costNames = map[string]bool{
		"input_tokens": true, "output_tokens": true, "total_tokens": true, "cached_input_tokens": true,
		"cache_creation_input_tokens": true, "reasoning_tokens": true, "uint": true, "double": true,
	}
)

// validCostExpression accepts arithmetic over the token counts the gateway
// exposes. It is a guard against typos, not a CEL parser: the gateway has the
// final say when the policy is applied.
func validCostExpression(expr string) error {
	if len(expr) > 200 {
		return invalid("cost_expression must be at most 200 characters")
	}
	depth := 0
	for rest := expr; rest != ""; {
		tok := costToken.FindString(rest)
		switch {
		case tok == "":
			return invalid("cost_expression has an unsupported character at %q", rest)
		case costWord.MatchString(tok) && !costNames[tok]:
			return invalid("cost_expression uses %q; the known names are input_tokens, output_tokens, total_tokens, cached_input_tokens, cache_creation_input_tokens, reasoning_tokens, uint and double", tok)
		case tok == "(":
			depth++
		case tok == ")":
			depth--
		}
		if depth < 0 {
			return invalid("cost_expression has an unmatched )")
		}
		rest = rest[len(tok):]
	}
	if depth != 0 {
		return invalid("cost_expression has an unmatched (")
	}
	return nil
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
		out[i].Warnings = append(recipeWarnings(out[i]), ownRouteWarnings(out[i], fleet)...)
		if n := len(bestEffort[out[i].ID]) - render.MaxOverageTenants; n > 0 {
			out[i].Warnings = append(out[i].Warnings, fmt.Sprintf("%d tenants should be served as best-effort and are not: the best-effort route lists at most %d tenants, the first by name. The others are refused once their budget is spent.", n, render.MaxOverageTenants))
		}
		if out[i].Fleet && fleetSites(out[i], peers) > 1 {
			out[i].Warnings = append(out[i].Warnings, "Quotas are not enforced on this model's entry route: it has more than one site, and Envoy AI Gateway up to 1.2.0 attaches no quota to a route with site weights. Every tenant is answered without a limit until the gateways run a version with the fix.")
		}
		if out[i].SpentMode == store.SpentBestEffort && s.usage == nil {
			out[i].Warnings = append(out[i].Warnings, "The server has no Redis configured, so it cannot see that a budget is spent. A tenant past its budget is refused.")
		}
		if out[i].Fleet && !s.st.FleetConfigured() {
			out[i].Warnings = append(out[i].Warnings, "The server has no FLEET_DOMAIN or FLEET_PEER_SNI set, so the entry route is left as it is on the clusters and gets no weight changes.")
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// fleetSites counts the sites the model's entry route is rendered with.
func fleetSites(m store.Model, peers map[string]bool) int {
	n := 0
	for _, z := range m.SiteWeights {
		if peers[z.Zone] {
			n++
		}
	}
	return n
}

// fleetBody is the body of turning a model's entry route on or off.
type fleetBody struct {
	Enabled bool `json:"enabled"`
}

// setModelFleet turns the entry route of a model on or off. When on, every
// fleet cluster gets a route for the model that sends each conversation to
// one of the sites that serve it.
func (s *Server) setModelFleet(w http.ResponseWriter, r *http.Request) {
	var b fleetBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	if b.Enabled && !s.st.FleetConfigured() {
		writeError(w, http.StatusConflict, "The server has no FLEET_DOMAIN or FLEET_PEER_SNI set, so it cannot render an entry route.")
		return
	}
	name, err := s.st.SetModelFleet(r.Context(), r.PathValue("id"), b.Enabled)
	switch {
	case errors.Is(err, store.ErrFleetNoSite):
		writeError(w, http.StatusConflict, "No fleet cluster serves "+name+" yet. Mark the clusters as part of the fleet and wait for a poll.")
		return
	case errors.Is(err, store.ErrFleetManual):
		writeError(w, http.StatusConflict, name+" has endpoints entered by hand. Remove them first: the entry route replaces them.")
		return
	case errors.Is(err, store.ErrBestEffortOn):
		writeError(w, http.StatusConflict, name+" serves a spent budget as best-effort, which is built on the entry route. Set it back to refuse first.")
		return
	case err != nil:
		fail(w, err)
		return
	}
	if b.Enabled {
		s.changed(r, "model.fleet-on", "Turned the entry route on for "+name+". Requests through it are counted on the entry route, so the model's quota counters restart once.")
	} else {
		s.changed(r, "model.fleet-off", "Turned the entry route off for "+name+". Its quota counters restart once.")
	}
	w.WriteHeader(http.StatusNoContent)
}

// spentBody is the body of setting what happens when a budget is spent.
type spentBody struct {
	// Mode is "refuse" or "best-effort".
	Mode string `json:"mode"`
	// Unlimited also serves the tenants without a quota of their own on
	// the model as best-effort. It only applies in best-effort mode.
	Unlimited bool `json:"best_effort_unlimited"`
}

// setModelSpentMode sets what a model does with a tenant whose budget is
// spent: refuse it, or serve it as best-effort on a second entry route.
func (s *Server) setModelSpentMode(w http.ResponseWriter, r *http.Request) {
	var b spentBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	if b.Mode != store.SpentRefuse && b.Mode != store.SpentBestEffort {
		fail(w, invalid("mode must be refuse or best-effort"))
		return
	}
	name, err := s.st.SetModelSpentMode(r.Context(), r.PathValue("id"), b.Mode, b.Unlimited)
	switch {
	case errors.Is(err, store.ErrNoEntryRoute):
		writeError(w, http.StatusConflict, name+" has no entry route. Best-effort is a second entry route, so turn the entry route on first.")
		return
	case err != nil:
		fail(w, err)
		return
	}
	switch {
	case b.Mode == store.SpentRefuse:
		s.changed(r, "model.spent-refuse", "A tenant past its budget on "+name+" is refused again.")
	case b.Unlimited:
		s.changed(r, "model.spent-best-effort", "A tenant past its budget on "+name+" is served as best-effort, and so is a tenant without a quota on it.")
	default:
		s.changed(r, "model.spent-best-effort", "A tenant past its budget on "+name+" is served as best-effort.")
	}
	w.WriteHeader(http.StatusNoContent)
}

// listOverage returns the periods in which a tenant was served as
// best-effort, newest first, running ones included. ?limit= defaults to 100,
// at most 500.
func (s *Server) listOverage(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			fail(w, invalid("limit must be a number from 1 to 500"))
			return
		}
		limit = n
	}
	out, err := s.st.ListOverage(r.Context(), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// drainBody is the body of a drain or undrain.
type drainBody struct {
	Drained bool `json:"drained"`
}

// drainSite starts or ends an operator drain of one model on one cluster. A
// drained site's weight steps down to 0, one instance per poll, and comes
// back the same way.
func (s *Server) drainSite(w http.ResponseWriter, r *http.Request) {
	var b drainBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	model, cluster, err := s.st.SetDrained(r.Context(), r.PathValue("id"), r.PathValue("cluster_id"), b.Drained)
	if errors.Is(err, store.ErrLastSite) {
		writeError(w, http.StatusConflict, "No other site has capacity for "+model+", so draining "+cluster+" would leave the model unreachable.")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	action, summary := "site.drain", "Started draining "+model+" on cluster "+cluster
	if !b.Drained {
		action, summary = "site.undrain", "Ended the drain of "+model+" on cluster "+cluster
	}
	// Ending a drain lists the site again at once, at the lowest weight.
	// The weight then moves on the next polls; each step is its own task.
	s.changed(r, action, summary)
	w.WriteHeader(http.StatusNoContent)
}

// recipeWarnings reports differences between the sites that serve a model
// which break sharing its traffic.
func recipeWarnings(m store.Model) []string {
	revisions, lengths := map[string][]string{}, map[string][]string{}
	for _, e := range m.Endpoints {
		if !e.Capacity.Serving {
			continue
		}
		if v := e.Capacity.Revision; v != "" {
			revisions[v] = append(revisions[v], e.ClusterName)
		}
		if v := e.Capacity.MaxModelLen; v != "" {
			lengths[v] = append(lengths[v], e.ClusterName)
		}
	}
	out := []string{}
	if len(revisions) > 1 {
		out = append(out, "The sites serve different revisions ("+valueList(revisions)+"), so the same model name gives different answers.")
	}
	if len(lengths) > 1 {
		out = append(out, "The sites take different request lengths ("+valueList(lengths)+"), so a long request fails at the smaller site.")
	}
	return out
}

// ownRouteWarnings names the fleet clusters whose own route for a model with
// an entry route is still reachable by clients. fleet says which clusters
// are in the fleet, by ID.
func ownRouteWarnings(m store.Model, fleet map[string]bool) []string {
	if !m.Fleet {
		return nil
	}
	var clusters []string
	for _, e := range m.Endpoints {
		if !fleet[e.ClusterID] {
			continue
		}
		for _, b := range e.Backends {
			if !b.PeerOnly {
				clusters = append(clusters, e.ClusterName)
				break
			}
		}
	}
	if len(clusters) == 0 {
		return nil
	}
	return []string{"On " + strings.Join(clusters, ", ") + " the cluster's own route for the model is still attached to the client listener, to the whole Gateway, or by port and not by listener name. " +
		"It is older than the entry route and wins, so clients there do not use the entry route. Its backends keep the quota. Attach that route to the peer listener alone."}
}

// valueList formats "value on site, site; value on site".
func valueList(sites map[string][]string) string {
	values := make([]string, 0, len(sites))
	for v := range sites {
		values = append(values, v)
	}
	sort.Strings(values)
	for i, v := range values {
		values[i] = v + " on " + strings.Join(sites[v], ", ")
	}
	return strings.Join(values, "; ")
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
