package api

import (
	"fmt"
	"sort"
	"strings"

	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// modelWarnings returns what an operator should know about the model as it
// is set up now. fleet says which clusters are part of the fleet, by ID;
// peers which clusters an entry route can send to, by name.
// bestEffortTenants is how many tenants the model's best-effort route
// should list.
func (s *Server) modelWarnings(m store.Model, fleet, peers map[string]bool, bestEffortTenants int) []string {
	warnings := append(recipeWarnings(m), ownRouteWarnings(m, fleet)...)
	if n := bestEffortTenants - render.MaxOverageTenants; n > 0 {
		warnings = append(warnings, fmt.Sprintf("%d tenants should be served as best-effort and are not: the best-effort route lists at most %d tenants, those past their budget first. The others are refused once their budget is spent.", n, render.MaxOverageTenants))
	}
	if m.Fleet && fleetSites(m, peers) > 1 {
		warnings = append(warnings, "Quotas are not enforced on this model's entry route: it has more than one site, and Envoy AI Gateway up to 1.2.0 attaches no quota to a route with site weights. Every tenant is answered without a limit until the gateways run a version with the fix.")
	}
	if m.SpentMode == store.SpentBestEffort && s.usage == nil {
		warnings = append(warnings, "The server has no Redis configured, so it cannot see that a budget is spent. A tenant past its budget is refused.")
	}
	if m.SpentMode == store.SpentBestEffort && s.usage != nil && !s.sy.Auto() {
		warnings = append(warnings, "Auto sync is off, so a tenant past its budget is only moved to best-effort, and back, when someone presses Sync. Until then it is refused.")
	}
	if m.Fleet && !s.st.FleetConfigured() {
		warnings = append(warnings, "The server has no FLEET_DOMAIN or FLEET_PEER_SNI set, so the entry route is left as it is on the clusters and gets no weight changes.")
	}
	return warnings
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
