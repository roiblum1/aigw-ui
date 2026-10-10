package api

import (
	"errors"
	"net/http"

	"aigw-ui/internal/store"
)

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
	listLimited(w, r, 100, s.st.ListOverage)
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
