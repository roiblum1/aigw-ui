package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// The longest request a model server takes, prompt and answer together, as
// the server itself says on /v1/models (vLLM's max_model_len). vLLM picks
// it from the model and the GPU's memory unless it is told one, so only the
// running server knows it; the annotation aigw-ui.io/max-model-len is what
// an operator declares, and wins where it is set. The hub asks through the
// Kubernetes API's pod proxy, the way it reaches everything else on a
// cluster, once per pod: a running server keeps its length, and a new spec
// makes new pods.

// lengthRetry is how long a pod that did not say its length waits before
// it is asked again: not allowed (pods/proxy), not reachable, or no vLLM.
const lengthRetry = 10 * time.Minute

// lengthTimeout bounds one question to a pod.
const lengthTimeout = 5 * time.Second

// servedLength is what one pod said, or when it was asked without an answer
// and whether the API server refused the hub the question.
type servedLength struct {
	length int
	asked  time.Time
	denied bool
}

// servedLength returns the length the ready pods of an LLMInferenceService
// take: what the first of them that answers says. It asks at most one pod
// on each look; 0 while none has said. denied is set when the hub may not
// ask (no get on pods/proxy).
func (c *Client) servedLength(ctx context.Context, g gpuShapes, svc *unstructured.Unstructured, now time.Time) (length int, denied bool) {
	asked := false
	for i := range g.pods {
		p := &g.pods[i]
		if p.GetNamespace() != svc.GetNamespace() || p.GetLabels()["app.kubernetes.io/name"] != svc.GetName() ||
			p.GetLabels()["llm-d.ai/role"] == "prefill" || !isReady(p) {
			continue
		}
		c.lengthMu.Lock()
		s, known := c.lengths[p.GetUID()]
		c.lengthMu.Unlock()
		if s.length > 0 {
			return s.length, false
		}
		if asked || (known && now.Sub(s.asked) < lengthRetry) {
			denied = denied || s.denied
			continue
		}
		asked = true
		n, err := c.askLength(ctx, p)
		s = servedLength{length: n, asked: now, denied: apierrors.IsForbidden(err)}
		c.lengthMu.Lock()
		if c.lengths == nil {
			c.lengths = map[types.UID]servedLength{}
		}
		c.lengths[p.GetUID()] = s
		c.lengthMu.Unlock()
		if n > 0 {
			return n, false
		}
		denied = denied || s.denied
	}
	return 0, denied
}

// forgetLengths drops what pods that are gone said. Only a full list of
// the pods tells which are gone.
func (c *Client) forgetLengths(g gpuShapes) {
	if !g.listed {
		return
	}
	live := map[types.UID]bool{}
	for i := range g.pods {
		live[g.pods[i].GetUID()] = true
	}
	c.lengthMu.Lock()
	defer c.lengthMu.Unlock()
	for uid := range c.lengths {
		if !live[uid] {
			delete(c.lengths, uid)
		}
	}
}

// askLength asks a pod's model server for its max_model_len on /v1/models,
// through the API server's pod proxy, at the scheme and port of the
// server's readiness check.
func (c *Client) askLength(ctx context.Context, pod *unstructured.Unstructured) (int, error) {
	if c.disc == nil {
		return 0, errors.New("no client for the API server's proxy")
	}
	scheme, port := serverPort(*pod)
	ctx, cancel := context.WithTimeout(ctx, lengthTimeout)
	defer cancel()
	raw, err := c.disc.RESTClient().Get().
		AbsPath("/api/v1/namespaces", pod.GetNamespace(), "pods", scheme+":"+pod.GetName()+":"+strconv.FormatInt(port, 10), "proxy", "v1", "models").
		DoRaw(ctx)
	if err != nil {
		return 0, err
	}
	var body struct {
		Data []struct {
			MaxModelLen int `json:"max_model_len"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, err
	}
	for _, m := range body.Data {
		if m.MaxModelLen > 0 {
			return m.MaxModelLen, nil
		}
	}
	return 0, errors.New("the server's /v1/models gives no max_model_len")
}
