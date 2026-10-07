package api

import (
	"errors"
	"net/http"
	"strings"

	"sigs.k8s.io/yaml"

	"aigw-ui/internal/gateway"
	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// clusterBody is the body of a cluster create or update. On an update every
// field that is left out keeps its stored value, so a caller that only knows
// some of the fields cannot wipe the others.
type clusterBody struct {
	Name        string  `json:"name"`
	Site        *string `json:"site"`
	Namespace   string  `json:"namespace"`
	GatewayName string  `json:"gateway_name"`
	AuthEnabled *bool   `json:"auth_enabled"`
	Kubeconfig  string  `json:"kubeconfig"`
	// GatewayURL is optional; an empty string removes it. DiscoveryToken is
	// write-only; empty keeps the stored one.
	GatewayURL     *string `json:"gateway_url"`
	DiscoveryToken string  `json:"discovery_token"`
}

// cluster validates the body and returns the cluster to store. current is the
// stored cluster on an update and nil on a create.
func (b *clusterBody) cluster(current *store.Cluster) (store.Cluster, error) {
	var c store.Cluster
	if current != nil {
		c = *current
	}
	if b.Name != "" {
		c.Name = b.Name
	}
	if b.Namespace != "" {
		c.Namespace = b.Namespace
	}
	if b.GatewayName != "" {
		c.GatewayName = b.GatewayName
	}
	if b.Site != nil {
		c.Site = *b.Site
	}
	if b.AuthEnabled != nil {
		c.AuthEnabled = *b.AuthEnabled
	}
	if b.GatewayURL != nil {
		c.GatewayURL = strings.TrimSpace(*b.GatewayURL)
		if c.GatewayURL != "" {
			normalized, err := gateway.NormalizeURL(c.GatewayURL)
			if err != nil {
				return c, invalid("gateway_url %v", err)
			}
			c.GatewayURL = normalized
		}
	}
	b.DiscoveryToken = strings.TrimSpace(b.DiscoveryToken)
	switch {
	case b.DiscoveryToken != "" && c.GatewayURL == "":
		return c, invalid("discovery_token needs a gateway_url")
	case !dnsLabel.MatchString(c.Name):
		return c, invalid("name must be lowercase letters, digits and dashes")
	case !dnsLabel.MatchString(c.Namespace):
		return c, invalid("namespace is not a valid Kubernetes namespace name")
	case !dnsLabel.MatchString(c.GatewayName):
		return c, invalid("gateway_name is not a valid Kubernetes object name")
	case current == nil && strings.TrimSpace(b.Kubeconfig) == "":
		return c, invalid("kubeconfig is required")
	}
	return c, nil
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	out, err := s.st.ListClusters(r.Context())
	if err != nil {
		fail(w, err)
		return
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
	s.sy.Changed(r.Context(), "cluster.add", "Added cluster "+c.Name)
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
	if err != nil {
		fail(w, err)
		return
	}
	c, err := s.st.UpdateCluster(r.Context(), in, []byte(strings.TrimSpace(b.Kubeconfig)), b.DiscoveryToken)
	if err != nil {
		fail(w, err)
		return
	}
	s.sy.Changed(r.Context(), "cluster.update", "Changed cluster "+c.Name)
	writeJSON(w, http.StatusOK, c)
}

// deleteCluster only forgets the cluster. Objects already applied to it stay
// there, because once the kubeconfig is gone they can no longer be removed.
func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteCluster(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) probeCluster(w http.ResponseWriter, r *http.Request) {
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
		s.sy.Record(r.Context(), "sync", "Manual sync of "+c.Name, c.ID)
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
	s.sy.Record(r.Context(), "sync", "Manual sync of all clusters", "")
	s.sy.SyncAll(r.Context())
	s.listClusters(w, r)
}

func (s *Server) discoverCluster(w http.ResponseWriter, r *http.Request) {
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
