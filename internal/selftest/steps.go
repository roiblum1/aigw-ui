package selftest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aigw-ui/internal/gateway"
	"aigw-ui/internal/kube"
	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// The steps of a run, in order.
const (
	stepGateway = "gateway"
	stepApply   = "apply"
	stepKey     = "key"
	stepBadKey  = "bad-key"
	stepCounter = "counter"
	stepLimit   = "limit"
	stepOverage = "overage"
	stepReset   = "reset"
	stepSticky  = "sticky"
	stepFleet   = "fleet-revision"
	stepCleanup = "cleanup"
)

func plan() []Step {
	return []Step{
		{ID: stepGateway, Title: "The gateway lists the model", Status: Pending},
		{ID: stepApply, Title: "A temporary tenant, key and quota are applied", Status: Pending},
		{ID: stepKey, Title: "A request with the new key is answered", Status: Pending},
		{ID: stepBadKey, Title: "A request with an unknown key is refused", Status: Pending},
		{ID: stepCounter, Title: "The tokens are counted where the Usage page reads them", Status: Pending},
		{ID: stepLimit, Title: "A request over the quota is refused", Status: Pending},
		{ID: stepOverage, Title: "A tenant past its budget is served as best-effort", Status: Pending},
		{ID: stepReset, Title: "A usage reset lets the tenant through again", Status: Pending},
		{ID: stepSticky, Title: "A conversation stays on one site", Status: Pending},
		{ID: stepFleet, Title: "The cluster has the fleet's current entry routes", Status: Pending},
		{ID: stepCleanup, Title: "The temporary tenant is removed", Status: Pending},
	}
}

// How long a step waits for the gateway to catch up, and how often it looks.
// A change travels from the API server through the gateway's controller to
// the proxy and the rate limit service, which takes seconds, not milliseconds.
const (
	keyWait     = 90 * time.Second
	counterWait = 20 * time.Second
	limitWait   = 30 * time.Second
	resetWait   = 15 * time.Second
	pollEvery   = 3 * time.Second
)

// testLimit and testWindow are the temporary tenant's quota: the first
// answered request uses it up. The window is an hour so that it cannot roll
// over between two steps the way a minute would.
const (
	testLimit  = 1
	testWindow = "1h"
)

// test is the state of one run.
type test struct {
	r       *Runner
	run     *Run
	ctx     context.Context
	cluster store.Cluster
	model   store.Model

	tenant *store.Tenant // set once the temporary tenant exists
	key    string        // its API key; empty when the cluster does not enforce keys
	// counted and refused remember what earlier steps found out.
	counted, refused bool
	// firstTokens is what the first answered request used.
	firstTokens int64
}

func (t *test) set(id, status, detail string) { t.r.set(t.run, id, status, detail) }

// steps runs the steps in order and stops at the first one whose failure
// makes the rest meaningless. Cleanup runs whenever a tenant was created.
func (t *test) steps() {
	defer t.cleanup()
	for _, step := range []struct {
		id  string
		run func() (status, detail string)
		// needed stops the run when the step does not pass.
		needed bool
	}{
		{stepGateway, t.gateway, true},
		{stepApply, t.apply, true},
		{stepKey, t.firstRequest, true},
		{stepBadKey, t.unknownKey, false},
		{stepCounter, t.counter, false},
		{stepLimit, t.limit, false},
		{stepOverage, t.overage, false},
		{stepReset, t.reset, false},
		{stepSticky, t.sticky, false},
		{stepFleet, t.fleetRevision, false},
	} {
		t.set(step.id, Running, "")
		status, detail := step.run()
		t.set(step.id, status, detail)
		if step.needed && status != Passed {
			return
		}
	}
}

// wait pauses between two looks. It reports false when the run is over.
func (t *test) wait() bool {
	select {
	case <-t.ctx.Done():
		return false
	case <-time.After(pollEvery):
		return true
	}
}

func (t *test) chat(key string) (gateway.ChatResult, error) {
	return gateway.Chat(t.ctx, t.cluster.GatewayURL, key, t.model.Name)
}

// chatWith sends a request as the temporary tenant with extra headers.
func (t *test) chatWith(headers map[string]string) (gateway.ChatResult, error) {
	return gateway.ChatWith(t.ctx, t.cluster.GatewayURL, t.key, t.model.Name, headers)
}

func (t *test) gateway() (string, string) {
	token, err := t.r.st.DiscoveryToken(t.ctx, t.cluster.ID)
	if err != nil {
		return Failed, err.Error()
	}
	names, err := gateway.ListModels(t.ctx, t.cluster.GatewayURL, token)
	if err != nil {
		return Failed, err.Error() + ". If the gateway wants a key for /v1/models, set the cluster's API key for /v1/models."
	}
	for _, name := range names {
		if name == t.model.Name {
			return Passed, fmt.Sprintf("%s answers and lists %s.", t.cluster.GatewayURL, t.model.Name)
		}
	}
	return Failed, fmt.Sprintf("%s answers but does not list %s.", t.cluster.GatewayURL, t.model.Name)
}

func (t *test) apply() (string, string) {
	// A tenant left behind by a run that was cut short, for example by a restart.
	if _, err := t.r.st.DeleteTenantsWithPrefix(t.ctx, TenantPrefix); err != nil {
		return Failed, err.Error()
	}
	slug, err := newSlug()
	if err != nil {
		return Failed, err.Error()
	}
	tenant, err := t.r.st.CreateTenant(t.ctx, slug, "Self-test (temporary)")
	if err != nil {
		return Failed, err.Error()
	}
	t.tenant = &tenant
	_, key, err := t.r.st.CreateKey(t.ctx, tenant.ID, "self-test")
	if err != nil {
		return Failed, err.Error()
	}
	if t.cluster.AuthEnabled {
		t.key = key
	}
	if err := t.r.st.UpsertQuota(t.ctx, tenant.ID, t.model.ID, testLimit, testWindow, nil); err != nil {
		return Failed, err.Error()
	}
	res, err := t.sync("Self-test of " + t.cluster.Name + ": added temporary tenant " + slug)
	if err != nil {
		return Failed, "The cluster refused the objects: " + err.Error()
	}
	if len(res.Rejected) > 0 {
		return Failed, "The gateway's controller does not accept " + describe(res.Rejected) + "."
	}
	if len(res.Changes) == 0 {
		return Failed, "The sync changed nothing on the cluster, so the tenant's quota was not applied. Is the model still served there?"
	}
	return Passed, "Tenant " + slug + " with a quota of 1 token per hour. On the cluster: " + describe(res.Changes) + "."
}

// sync applies the desired state to the cluster under test only, and logs it
// as a task.
func (t *test) sync(summary string) (kube.SyncResult, error) {
	t.r.sy.Record(t.ctx, "selftest", summary, t.cluster.ID)
	return t.r.sy.SyncCluster(t.ctx, t.cluster.ID)
}

// describe lists objects as "QuotaPolicy ns/name updated (gateway: Accepted)".
func describe(changes []kube.Change) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		part := c.Kind + " " + c.Namespace + "/" + c.Name
		if c.Action != "" {
			part += " " + c.Action
		}
		switch {
		case c.Gateway != "" && c.GatewayMessage != "" && c.Gateway != "Accepted":
			part += " (gateway: " + c.Gateway + ": " + c.GatewayMessage + ")"
		case c.Gateway != "":
			part += " (gateway: " + c.Gateway + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// answer describes a response for an error message.
func answer(res gateway.ChatResult) string {
	if res.Body == "" {
		return fmt.Sprintf("HTTP %d", res.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", res.Status, res.Body)
}

// firstRequest waits until the gateway knows the new key and answers.
func (t *test) firstRequest() (string, string) {
	start := time.Now()
	var last string
	var lastStatus int
	for time.Since(start) < keyWait {
		res, err := t.chat(t.key)
		switch {
		case err != nil:
			last, lastStatus = err.Error(), 0
		case res.Status == http.StatusOK:
			t.firstTokens = res.Tokens
			detail := fmt.Sprintf("Answered %d seconds after the sync, %d tokens used.", int(time.Since(start).Seconds()), res.Tokens)
			if t.key == "" {
				detail += " The cluster does not enforce API keys, so the request was sent without one."
			}
			return Passed, detail
		default:
			last, lastStatus = answer(res), res.Status
		}
		if !t.wait() {
			break
		}
	}
	hint := ""
	switch lastStatus {
	case http.StatusUnauthorized, http.StatusForbidden:
		hint = " The gateway does not know the key: check that the SecurityPolicy " + render.AuthPolicyName + " is accepted and attached to the gateway."
	case http.StatusTooManyRequests:
		hint = " The key is accepted but the request is refused by the quota. The tenant's own rule should have let it through, so the rule does not match: check that the gateway forwards the client ID in the x-aigw-client-id header."
	}
	return Failed, fmt.Sprintf("No answer within %d seconds. Last: %s.%s", int(keyWait.Seconds()), last, hint)
}

func (t *test) unknownKey() (string, string) {
	if t.key == "" {
		return Skipped, "\"Enforce API keys\" is off for this cluster, so the gateway takes requests without a key."
	}
	res, err := t.chat("sk-selftest-not-a-real-key")
	switch {
	case err != nil:
		return Failed, err.Error()
	case res.Status == http.StatusUnauthorized || res.Status == http.StatusForbidden:
		return Passed, fmt.Sprintf("Refused with HTTP %d.", res.Status)
	case res.Status == http.StatusOK:
		return Failed, "The gateway answered a request with a key it was never given: API key auth is not in effect."
	}
	return Warning, "Expected HTTP 401 and got " + answer(res) + "."
}

// cleanup removes the temporary tenant from the hub and from the cluster. It
// runs with its own deadline, so it still works when the run timed out.
func (t *test) cleanup() {
	if t.tenant == nil {
		return
	}
	tenant := t.tenant
	t.tenant = nil
	t.set(stepCleanup, Running, "")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.ctx), time.Minute)
	defer cancel()
	t.ctx = ctx

	if t.r.usage != nil && t.r.usage.CanReset() {
		// Leaves nothing behind in Redis. Without it the counter expires with its window.
		t.r.usage.Reset(ctx, tenant.ID, t.model.ID)
	}
	if err := t.r.st.DeleteTenant(ctx, tenant.ID); err != nil {
		t.set(stepCleanup, Failed, "Tenant "+tenant.Slug+" could not be deleted: "+err.Error()+". Delete it on the Tenants page.")
		return
	}
	res, err := t.sync("Self-test of " + t.cluster.Name + ": removed temporary tenant " + tenant.Slug)
	if err != nil {
		t.set(stepCleanup, Failed, "The tenant is deleted here, but the cluster could not be synced: "+err.Error()+". The next sync removes its key and quota from the cluster.")
		return
	}
	detail := "Tenant " + tenant.Slug + " is gone."
	if len(res.Changes) > 0 {
		detail += " On the cluster: " + describe(res.Changes) + "."
	}
	t.set(stepCleanup, Passed, detail)
}
