package api

import (
	"net/http"
	"strings"

	"aigw-ui/internal/gateway"
	"aigw-ui/internal/kube"
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
	case current != nil && current.FleetEnabled && c.FleetEnabled && c.Name != current.Name:
		// The name is the site's zone on every gateway. A new name is a new
		// zone: the conversations of every model would be dealt out again.
		return c, invalid("a fleet cluster cannot be renamed: its name is its zone on every gateway. Take it out of the fleet first")
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

// fleetProbeError refuses a cluster that joins the fleet without the
// EnvoyPatchPolicy kind. Every entry route needs one: without it a site that
// answers 503 keeps part of its conversations.
func fleetProbeError(p kube.Probe, err error) error {
	switch {
	case err != nil:
		return invalid("the cluster could not be asked whether it has what a fleet cluster needs: %v", err)
	case !p.Kinds["EnvoyPatchPolicy"]:
		return invalid("the cluster has no EnvoyPatchPolicy kind, which every entry route needs. Install the Envoy Gateway CRDs and set extensionApis.enableEnvoyPatchPolicy: true")
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
