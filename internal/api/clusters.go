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
	// FleetEnabled makes the cluster one of the sites that share traffic.
	FleetEnabled *bool `json:"fleet_enabled"`
	// ClientListener is the Gateway listener the API-key policy attaches to;
	// an empty string attaches it to the whole Gateway.
	ClientListener *string `json:"client_listener"`
	// PeerHost and PeerPort are where the other sites reach this site's
	// gateway. A fleet cluster needs the host; the port defaults to 8443.
	PeerHost *string `json:"peer_host"`
	PeerPort *int    `json:"peer_port"`
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
	if b.FleetEnabled != nil {
		c.FleetEnabled = *b.FleetEnabled
	}
	if b.ClientListener != nil {
		c.ClientListener = strings.TrimSpace(*b.ClientListener)
	}
	if b.PeerHost != nil {
		c.PeerHost = strings.ToLower(strings.TrimSpace(*b.PeerHost))
	}
	if b.PeerPort != nil {
		c.PeerPort = *b.PeerPort
	}
	if c.PeerPort == 0 {
		c.PeerPort = 8443
	}
	b.DiscoveryToken = strings.TrimSpace(b.DiscoveryToken)
	switch {
	case c.PeerHost != "" && !hostname.MatchString(c.PeerHost):
		return c, invalid("peer_host must be a DNS name such as llm.site1-a.example.com")
	case c.PeerPort < 1 || c.PeerPort > 65535:
		return c, invalid("peer_port must be between 1 and 65535")
	case c.FleetEnabled && c.PeerHost == "":
		return c, invalid("a fleet cluster needs peer_host, the name the other sites reach its gateway under")
	case c.ClientListener != "" && !dnsLabel.MatchString(c.ClientListener):
		return c, invalid("client_listener is not a valid listener name")
	case c.FleetEnabled && !c.AuthEnabled:
		// Without key enforcement a client can send the client ID header
		// itself and spend another tenant's quota at any site.
		return c, invalid("a fleet cluster must enforce API keys: turn auth_enabled on, or fleet_enabled off")
	case c.FleetEnabled && c.ClientListener == "":
		// Without a listener the key policy also covers the listener other
		// sites forward to. The entry gateway removes the key before it
		// forwards, so every cross-site request would be refused.
		return c, invalid("a fleet cluster needs client_listener, the name of the Gateway listener clients come in on")
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

// checkFleet refuses a fleet cluster whose gateway namespace differs from the
// other fleet clusters'. The namespace is part of the name of every quota
// counter, so a site with another one would count its tenants' tokens apart
// from the rest of the fleet, without any error.
func checkFleet(c store.Cluster, all []store.Cluster) error {
	if !c.FleetEnabled {
		return nil
	}
	for _, o := range all {
		if o.FleetEnabled && o.ID != c.ID && o.Namespace != c.Namespace {
			return invalid("fleet clusters must use the same gateway namespace: %s uses %q, this cluster %q", o.Name, o.Namespace, c.Namespace)
		}
	}
	return nil
}

func (s *Server) checkFleet(r *http.Request, c store.Cluster) error {
	all, err := s.st.ListClusters(r.Context())
	if err != nil {
		return err
	}
	return checkFleet(c, all)
}

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
