package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// SpecHash identifies what an LLMInferenceService runs, whatever its scale:
// SHA-256 of its spec with every "replicas" field removed, so that scaling
// it keeps its measurement and changing anything else, such as an argument,
// does not. metadata.generation would change on a scale too.
//
// What it runs is also set outside its spec, so the hash adds:
//
//   - the specs of the LLMInferenceServiceConfigs it is built on, in the
//     order KServe applied them: KServe's own presets (the template, the
//     scheduler) and those its baseRefs name. A shared config can change
//     vLLM's arguments or image, or the scheduler, for every service built
//     on it;
//   - the image digests its ready model servers run, by role: a vLLM
//     upgrade in KServe's own config, or an image pushed again under the
//     same tag, changes only these.
//
// A prefill/decode service adds the ratio of prefill to decode replicas,
// reduced: what one decode replica serves depends on how many prefill
// replicas feed it.
func SpecHash(svc *unstructured.Unstructured, bases []Base, images []string) string {
	spec, _, _ := unstructured.NestedMap(svc.Object, "spec")
	// encoding/json writes map keys in order, so equal specs give equal
	// bytes.
	data, _ := json.Marshal(withoutReplicas(spec))
	h := sha256.New()
	h.Write(data)
	for _, b := range bases {
		fmt.Fprintf(h, "|base %s=", b.Name)
		if b.Spec == nil {
			h.Write([]byte("?"))
			continue
		}
		data, _ := json.Marshal(withoutReplicas(b.Spec))
		h.Write(data)
	}
	if len(images) > 0 {
		fmt.Fprintf(h, "|images=%s", strings.Join(images, ","))
	}
	if _, split, _ := unstructured.NestedMap(svc.Object, "spec", "prefill"); split {
		p, d := replicas(svc, "spec", "prefill", "replicas"), replicas(svc, "spec", "replicas")
		if g := gcd(p, d); g > 0 {
			p, d = p/g, d/g
		}
		fmt.Fprintf(h, "|prefill:decode=%d:%d", p, d)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Base is an LLMInferenceServiceConfig a service is built on, with its spec;
// nil when the hub may not read it or it is not there.
type Base struct {
	Name string
	Spec map[string]any
}

// llmConfigGVRs are the versions of LLMInferenceServiceConfig, newest first.
var llmConfigGVRs = []schema.GroupVersionResource{
	{Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceserviceconfigs"},
	{Group: "serving.kserve.io", Version: "v1alpha1", Resource: "llminferenceserviceconfigs"},
}

// baseConfigs are a cluster's LLMInferenceServiceConfigs. denied is set when
// the hub may not list them, failed when listing them failed otherwise.
type baseConfigs struct {
	items          []unstructured.Unstructured
	denied, failed bool
}

func (c *Client) baseConfigs(ctx context.Context) baseConfigs {
	for i, gvr := range llmConfigGVRs {
		list, err := c.dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
		switch {
		case missingKind(err) && i < len(llmConfigGVRs)-1:
			continue
		case missingKind(err):
			return baseConfigs{}
		case apierrors.IsForbidden(err):
			return baseConfigs{denied: true}
		case err != nil:
			return baseConfigs{failed: true}
		}
		return baseConfigs{items: list.Items}
	}
	return baseConfigs{}
}

// of returns the configs a service is built on. KServe lists those it
// applied in status.appliedConfigs, its presets among them, with their
// namespaces; they are taken in that order. A service without that list, from
// an older KServe, has the configs its spec.baseRefs name: KServe looks for
// each in the service's namespace, then in its own, which the hub does not
// know, so the first other namespace that has one by that name is taken. ok
// is false when the configs could not be listed this time: the hash is then
// not worked out.
func (b baseConfigs) of(svc *unstructured.Unstructured) ([]Base, bool) {
	applied, _, _ := unstructured.NestedSlice(svc.Object, "status", "appliedConfigs")
	refs, _, _ := unstructured.NestedSlice(svc.Object, "spec", "baseRefs")
	if len(applied) == 0 && len(refs) == 0 {
		return nil, true
	}
	if b.failed {
		return nil, false
	}
	if len(applied) > 0 {
		out := make([]Base, 0, len(applied))
		for _, a := range applied {
			m, _ := a.(map[string]any)
			name, _ := m["name"].(string)
			ns, _ := m["namespace"].(string)
			if ns == "" {
				ns = svc.GetNamespace()
			}
			base := Base{Name: ns + "/" + name}
			for i := range b.items {
				if c := &b.items[i]; c.GetName() == name && c.GetNamespace() == ns {
					base.Spec, _, _ = unstructured.NestedMap(c.Object, "spec")
					break
				}
			}
			out = append(out, base)
		}
		return out, true
	}
	out := make([]Base, 0, len(refs))
	for _, r := range refs {
		m, _ := r.(map[string]any)
		name, _ := m["name"].(string)
		base := Base{Name: name}
		var other *unstructured.Unstructured
		for i := range b.items {
			c := &b.items[i]
			if c.GetName() != name {
				continue
			}
			if c.GetNamespace() == svc.GetNamespace() {
				other = c
				break
			}
			if other == nil || c.GetNamespace() < other.GetNamespace() {
				other = c
			}
		}
		if other != nil {
			base.Spec, _, _ = unstructured.NestedMap(other.Object, "spec")
		}
		out = append(out, base)
	}
	return out, true
}

// mainContainer is the name KServe gives the container that runs the model
// server.
const mainContainer = "main"

// images returns the image digests a service's ready pods run in their model
// server, as "role=digest", sorted. The role is the pod's llm-d.ai/role
// (prefill, decode or none), and the workers of a multi-node group apart
// from its leader. ok is false while it cannot be told: the pods could not
// be listed, none is ready, or the ready pods of one role run different
// images, as during a rollout. When the hub may not list pods, the images
// are not known and the hash goes without them.
func (g gpuShapes) images(svc *unstructured.Unstructured) ([]string, bool) {
	if slices.Contains(g.denied, "pods") {
		return nil, true
	}
	if !g.listed {
		return nil, false
	}
	byRole := map[string]string{}
	for i := range g.pods {
		p := &g.pods[i]
		if p.GetNamespace() != svc.GetNamespace() || p.GetLabels()["app.kubernetes.io/name"] != svc.GetName() || !isReady(p) {
			continue
		}
		role := p.GetLabels()["llm-d.ai/role"]
		if index, ok := p.GetLabels()["leaderworkerset.sigs.k8s.io/worker-index"]; ok && index != "0" {
			role += "/worker"
		}
		digest := imageDigest(p)
		if digest == "" {
			return nil, false
		}
		if seen, ok := byRole[role]; ok && seen != digest {
			return nil, false
		}
		byRole[role] = digest
	}
	if len(byRole) == 0 {
		return nil, false
	}
	out := make([]string, 0, len(byRole))
	for role, digest := range byRole {
		out = append(out, role+"="+digest)
	}
	sort.Strings(out)
	return out, true
}

// imageDigest is the digest of the image a pod's model server runs, as the
// kubelet reports it ("" before it does). The registry is left out: the
// same image from a mirror is the same image.
func imageDigest(p *unstructured.Unstructured) string {
	statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
	for _, s := range statuses {
		m, _ := s.(map[string]any)
		if m["name"] != mainContainer {
			continue
		}
		id, _ := m["imageID"].(string)
		if i := strings.LastIndex(id, "@"); i >= 0 {
			id = id[i+1:]
		}
		return id
	}
	return ""
}

func withoutReplicas(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if k != "replicas" {
				out[k] = withoutReplicas(x)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = withoutReplicas(x)
		}
		return out
	}
	return v
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// quantizationArg finds vLLM's --quantization in an argument list.
var quantizationArg = regexp.MustCompile(`--quantization(?:=|\s+)(\S+)`)

// Serving reads how a service runs its model, for the page: vLLM's
// --quantization ("" when not set: the checkpoint's own), and whether it
// uses LMCache. Both are read from the containers' command, args and
// environment, where KServe passes extra arguments in VLLM_ADDITIONAL_ARGS.
func Serving(svc *unstructured.Unstructured) (quantization string, lmcache bool) {
	var texts []string
	for _, path := range [][]string{{"spec", "template"}, {"spec", "worker"}, {"spec", "prefill", "template"}} {
		containers, _, _ := unstructured.NestedSlice(svc.Object, append(path, "containers")...)
		for _, c := range containers {
			cm, _ := c.(map[string]any)
			for _, key := range []string{"command", "args"} {
				list, _ := cm[key].([]any)
				for _, a := range list {
					if s, ok := a.(string); ok {
						texts = append(texts, s)
					}
				}
			}
			env, _ := cm["env"].([]any)
			for _, e := range env {
				em, _ := e.(map[string]any)
				name, _ := em["name"].(string)
				value, _ := em["value"].(string)
				if strings.HasPrefix(name, "LMCACHE_") {
					lmcache = true
				}
				texts = append(texts, value)
			}
		}
	}
	all := strings.Join(texts, " ")
	if m := quantizationArg.FindStringSubmatch(all); m != nil {
		quantization = strings.Trim(m[1], `"'`)
	}
	return quantization, lmcache || strings.Contains(strings.ToLower(all), "lmcache")
}
