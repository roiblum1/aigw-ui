package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// SpecHash identifies what an LLMInferenceService runs, whatever its scale:
// SHA-256 of its spec with every "replicas" field removed, so that scaling
// it keeps its measurement and changing anything else, such as an argument,
// does not. metadata.generation would change on a scale too.
//
// A prefill/decode service adds the ratio of prefill to decode replicas,
// reduced: what one decode replica serves depends on how many prefill
// replicas feed it.
func SpecHash(svc *unstructured.Unstructured) string {
	spec, _, _ := unstructured.NestedMap(svc.Object, "spec")
	// encoding/json writes map keys in order, so equal specs give equal
	// bytes.
	data, _ := json.Marshal(withoutReplicas(spec))
	h := sha256.New()
	h.Write(data)
	if _, split, _ := unstructured.NestedMap(svc.Object, "spec", "prefill"); split {
		p, d := replicas(svc, "spec", "prefill", "replicas"), replicas(svc, "spec", "replicas")
		if g := gcd(p, d); g > 0 {
			p, d = p/g, d/g
		}
		fmt.Fprintf(h, "|prefill:decode=%d:%d", p, d)
	}
	return hex.EncodeToString(h.Sum(nil))
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
