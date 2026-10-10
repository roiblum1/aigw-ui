package api

import (
	"errors"
	"net/http"
	"strings"

	"sigs.k8s.io/yaml"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	out, err := s.st.ListClusters(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	// A fleet cluster on another revision than the fleet's sends some
	// conversations to another site than the rest. A fleet that cannot be
	// rendered right now has no revision to compare with.
	if current, err := s.st.CurrentFleetRevision(r.Context()); err == nil {
		for i := range out {
			out[i].FleetOutdated = out[i].FleetEnabled && out[i].FleetRevision != current
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request) {
	var b clusterBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	in, err := b.cluster(nil)
	if err == nil {
		err = s.checkFleet(r, in)
	}
	if err == nil && in.FleetEnabled {
		err = fleetProbeError(s.sy.ProbeWith(r.Context(), []byte(b.Kubeconfig), in.Namespace, in.GatewayName))
	}
	if err != nil {
		fail(w, err)
		return
	}
	c, err := s.st.CreateCluster(r.Context(), in, []byte(b.Kubeconfig), b.DiscoveryToken)
	if err != nil {
		fail(w, err)
		return
	}
	// Pull the cluster's models right away instead of waiting for the next poll.
	// A cluster that cannot be reached is still saved; the failure shows in its row.
	if _, err := s.sy.DiscoverCluster(r.Context(), c.ID); err == nil {
		if fresh, err := s.st.GetCluster(r.Context(), c.ID); err == nil {
			c = fresh
		}
	}
	s.changed(r, "cluster.add", "Added cluster "+c.Name)
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) updateCluster(w http.ResponseWriter, r *http.Request) {
	var b clusterBody
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	current, err := s.st.GetCluster(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	in, err := b.cluster(&current)
	if err == nil {
		err = s.checkFleet(r, in)
	}
	if err == nil && in.FleetEnabled && !current.FleetEnabled {
		err = fleetProbeError(s.sy.Probe(r.Context(), current.ID))
	}
	if err != nil {
		fail(w, err)
		return
	}
	c, err := s.st.UpdateCluster(r.Context(), in, []byte(strings.TrimSpace(b.Kubeconfig)), b.DiscoveryToken)
	if err != nil {
		fail(w, err)
		return
	}
	s.changed(r, "cluster.update", "Changed cluster "+c.Name)
	writeJSON(w, http.StatusOK, c)
}

// deleteCluster only forgets the cluster. Objects already applied to it stay
// there, because once the kubeconfig is gone they can no longer be removed.
func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	c, err := s.st.GetCluster(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if c.FleetEnabled {
		// Its gateway would keep the entry routes with the sites and weights
		// of today, and its keys, with nobody to update or revoke them.
		writeError(w, http.StatusConflict, c.Name+" is part of the fleet. Take it out of the fleet and wait for its sync first, so its entry routes are removed.")
		return
	}
	if err := s.st.DeleteCluster(r.Context(), c.ID); err != nil {
		fail(w, err)
		return
	}
	s.log(r, "cluster.delete", "Removed cluster "+c.Name, true,
		"The cluster is no longer managed. Objects already applied to it were left in place.")
	w.WriteHeader(http.StatusNoContent)
}

// clusterName returns the name of the cluster in the request path, or its ID
// when there is no such cluster, for the audit log.
func (s *Server) clusterName(r *http.Request) string {
	c, err := s.st.GetCluster(r.Context(), r.PathValue("id"))
	if err != nil {
		return r.PathValue("id")
	}
	return c.Name
}

func (s *Server) probeCluster(w http.ResponseWriter, r *http.Request) {
	note(r, "cluster.test", "Tested the connection to cluster "+s.clusterName(r))
	p, err := s.sy.Probe(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"reachable": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reachable": true, "probe": p})
}

func (s *Server) syncCluster(w http.ResponseWriter, r *http.Request) {
	if c, err := s.st.GetCluster(r.Context(), r.PathValue("id")); err == nil {
		s.record(r, "sync", "Manual sync of "+c.Name, c.ID)
	}
	res, err := s.sy.SyncCluster(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Server) syncAll(w http.ResponseWriter, r *http.Request) {
	s.record(r, "sync", "Manual sync of all clusters", "")
	s.sy.SyncAll(r.Context())
	s.listClusters(w, r)
}

func (s *Server) discoverCluster(w http.ResponseWriter, r *http.Request) {
	note(r, "discovery", "Asked cluster "+s.clusterName(r)+" for its models")
	n, err := s.sy.DiscoverCluster(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": n})
}

func (s *Server) discoverAll(w http.ResponseWriter, r *http.Request) {
	note(r, "discovery", "Asked all clusters for their models")
	s.sy.DiscoverAll(r.Context())
	s.listClusters(w, r)
}

// clusterManifests shows what a sync would apply, with key values masked.
func (s *Server) clusterManifests(w http.ResponseWriter, r *http.Request) {
	state, err := s.st.RenderState(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	var sb strings.Builder
	for i, obj := range render.Redacted(render.Objects(state)) {
		doc, err := yaml.Marshal(obj.Object)
		if err != nil {
			fail(w, err)
			return
		}
		if i > 0 {
			sb.WriteString("---\n")
		}
		sb.Write(doc)
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": sb.String()})
}
