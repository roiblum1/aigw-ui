package selftest

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The stickiness step sends this many requests with one session ID, and then
// one each for this many other IDs to see how conversations spread.
const (
	stickyRepeats = 3
	spreadIDs     = 10
	// stickyLimit replaces the temporary tenant's quota of one token for the
	// step: its requests have to be answered, not refused.
	stickyLimit = 1_000_000
)

// chargedTwice reports whether the counter holds more than one request can
// explain. With the entry route a request passes two gateways, the entry and
// the serving site, and only the entry may charge it.
//
// It cannot tell with a cost expression: the counter then holds a computed
// amount, not total_tokens.
func chargedTwice(counted, request int64, costExpression string) (bool, string) {
	if request <= 0 || costExpression != "" || counted < 2*request {
		return false, ""
	}
	return true, fmt.Sprintf("%d tokens were counted for one request that used %d. The request is charged more than once: "+
		"check that only the entry gateway has a QuotaPolicy for the model, and none on the serving site's route.", counted, request)
}

// stickiness judges where the requests of one conversation were served.
// servedBy holds the site each answered request named, "" when it named none.
func stickiness(servedBy []string) (string, string) {
	sites := map[string]bool{}
	for _, s := range servedBy {
		sites[s] = true
	}
	switch {
	case len(servedBy) < stickyRepeats:
		return Warning, fmt.Sprintf("Only %d of %d requests were answered, so nothing can be said.", len(servedBy), stickyRepeats)
	case sites[""]:
		return Warning, "The answers do not carry the x-llm-served-by header, so the site that served them cannot be seen. The serving sites' peer listener sets it."
	case len(sites) > 1:
		return Failed, "Requests with one session ID were served by " + strings.Join(sortedKeys(sites), " and ") +
			". A conversation that changes site loses its cache. Check that every fleet cluster is on the same fleet revision."
	}
	return Passed, fmt.Sprintf("%d requests with one session ID were all served by %s.", len(servedBy), servedBy[0])
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // never fails
	return hex.EncodeToString(b)
}

// sticky checks that requests with one session ID are served by one site,
// and records where other session IDs land.
func (t *test) sticky() (string, string) {
	switch {
	case !t.cluster.FleetEnabled || !t.model.Fleet:
		return Skipped, "The model has no entry route on this cluster, so requests do not choose between sites."
	case t.tenant == nil:
		return Skipped, "There is no temporary tenant to send requests as."
	}
	// The tenant's quota of one token is used up. Raise it for this step.
	if err := t.r.st.UpsertQuota(t.ctx, t.tenant.ID, t.model.ID, stickyLimit, testWindow, nil); err != nil {
		return Failed, err.Error()
	}
	if _, err := t.sync("Self-test of " + t.cluster.Name + ": raised the temporary quota for the stickiness check"); err != nil {
		return Failed, "The cluster refused the raised quota: " + err.Error()
	}

	header := t.r.st.SessionHeader()
	send := func(id string) (string, bool) {
		res, err := t.chatWith(map[string]string{header: id})
		return res.ServedBy, err == nil && res.Status == http.StatusOK
	}
	// Wait until the raised quota lets a request through.
	conversation := sessionID()
	var servedBy []string
	for start := time.Now(); time.Since(start) < limitWait; {
		if site, ok := send(conversation); ok {
			servedBy = append(servedBy, site)
			break
		}
		if !t.wait() {
			break
		}
	}
	if len(servedBy) == 0 {
		return Warning, fmt.Sprintf("No request was answered within %d seconds of raising the quota, so stickiness could not be checked.", int(limitWait.Seconds()))
	}
	for len(servedBy) < stickyRepeats {
		site, ok := send(conversation)
		if !ok {
			break
		}
		servedBy = append(servedBy, site)
	}
	status, detail := stickiness(servedBy)
	if status != Passed {
		return status, detail
	}

	spread := map[string]int{}
	for range spreadIDs {
		if site, ok := send(sessionID()); ok && site != "" {
			spread[site]++
		}
	}
	var parts []string
	for _, site := range sortedCounts(spread) {
		parts = append(parts, fmt.Sprintf("%s %d", site, spread[site]))
	}
	return Passed, fmt.Sprintf("%s %d other session IDs landed on: %s.", detail, spreadIDs, strings.Join(parts, ", "))
}

func sortedCounts(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fleetRevision checks that this cluster's entry routes are the fleet's
// current ones. A cluster that is behind can send a conversation to another
// site than the other clusters do.
func (t *test) fleetRevision() (string, string) {
	if !t.cluster.FleetEnabled {
		return Skipped, "The cluster is not part of the fleet."
	}
	current, err := t.r.st.CurrentFleetRevision(t.ctx)
	if err != nil {
		return Failed, "The fleet cannot be rendered right now: " + err.Error()
	}
	c, err := t.r.st.GetCluster(t.ctx, t.cluster.ID)
	if err != nil {
		return Failed, err.Error()
	}
	switch {
	case current == "":
		return Skipped, "No model has an entry route."
	case c.FleetRevision != current:
		return Failed, fmt.Sprintf("The cluster is on fleet revision %q and the fleet on %q. Sync the cluster.", c.FleetRevision, current)
	}
	return Passed, "The cluster is on the fleet's revision " + current + "."
}
