package gitops

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	dnsLabel   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	objectName = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,40}[a-z0-9])?$`)
	modelName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	modelURI   = regexp.MustCompile(`^(hf|s3|pvc|oci|gs|https?)://\S+$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// KServe's launcher runs the arguments through bash eval: one plain
	// token each, no shell character.
	plainArg = regexp.MustCompile("^[^\\s'\"$`\\\\;&|<>(){}*?\\[\\]!#~]+$")
	// A value in single quotes is one word to eval and nothing in it is
	// expanded, as long as it holds no single quote: JSON can go there.
	quotedArg = regexp.MustCompile(`^--[A-Za-z0-9._-]+='[^']*'$`)
	// Flags the chart sets itself from its own values.
	kvTransferFlag = regexp.MustCompile(`^--kv[-_]transfer[-_]config`)
	maxLenFlag     = regexp.MustCompile(`^--max[-_]model[-_]len`)
)

// AppName is the name the platform gives a model's Application on a
// cluster (and Argo CD's tracking label on every object, so 63 characters at
// most); the hub finds its status by it. pattern holds {cluster} and
// {model}.
func AppName(pattern, cluster, model string) string {
	return strings.NewReplacer("{cluster}", cluster, "{model}", model).Replace(pattern)
}

// DefaultAppName is how argocd-day2-platform names an operator's
// Application on a hosted cluster.
const DefaultAppName = "popai-{cluster}-{model}-deploy"

// CheckModelName refuses a new model's operator name or namespace that the
// platform or the cluster would not take, and the namespaces of the
// platform itself.
func CheckModelName(name, namespace string, reserved ...string) error {
	switch {
	case !dnsLabel.MatchString(name):
		return fmt.Errorf("the model's name must be lowercase letters, digits and dashes (a DNS label): it names its directory and Argo CD Application")
	case len(AppName(DefaultAppName, strings.Repeat("x", 20), name)) > 63:
		return fmt.Errorf("the model's name is too long for its Argo CD Application's name (63 characters with the cluster's)")
	case !dnsLabel.MatchString(namespace):
		return fmt.Errorf("namespace must be a DNS label")
	case namespace == "default" || namespace == "kserve" || strings.HasPrefix(namespace, "kube-") || strings.HasPrefix(namespace, "openshift"):
		return fmt.Errorf("namespace %s belongs to the platform", namespace)
	}
	for _, r := range reserved {
		if namespace == r {
			return fmt.Errorf("namespace %s is the gateway's, where aigw-ui's own objects are", namespace)
		}
	}
	return nil
}

// CheckValues refuses values the chart would refuse, so that a mistake is
// shown at once and not as a failed sync in Argo CD minutes later.
func CheckValues(v map[string]any) error {
	if n := str(v, "model", "name"); !modelName.MatchString(n) {
		return fmt.Errorf("model.name is required: letters, digits and . _ : / -")
	}
	if str(v, "namespace") == "" {
		return fmt.Errorf("namespace is required (the app's projectNamespace)")
	}
	if u := str(v, "model", "uri"); str(v, "model", "subPath") == "" && !modelURI.MatchString(u) {
		return fmt.Errorf("set model.subPath (the node-prep PVC) or model.uri (hf://, s3://, pvc://...)")
	}
	if n := ObjectName(v); !objectName.MatchString(n) {
		return fmt.Errorf("the LLMInferenceService's name %q must start with a letter and have at most 42 lowercase letters, digits and dashes: set llmisvc.name", n)
	}
	model, _ := v["model"].(map[string]any)
	hasLength := model["maxModelLen"] != nil && model["maxModelLen"] != ""
	llmisvc, _ := v["llmisvc"].(map[string]any)
	var args []any
	for _, key := range []string{"args", "extraArgs"} {
		list, ok := llmisvc[key].([]any)
		if llmisvc[key] != nil && !ok {
			return fmt.Errorf("llmisvc.%s must be a list", key)
		}
		args = append(args, list...)
	}
	for _, a := range args {
		s, ok := a.(string)
		switch {
		case !ok || !(plainArg.MatchString(s) || quotedArg.MatchString(s)):
			return fmt.Errorf("llmisvc.args: %v is not one plain token or --option='value' (KServe runs the arguments through bash eval)", a)
		case kvTransferFlag.MatchString(s):
			return fmt.Errorf("llmisvc.args: %s is set by the chart when lmcache.enabled", s)
		case hasLength && maxLenFlag.MatchString(s):
			return fmt.Errorf("llmisvc.args: %s and model.maxModelLen: set the length in one place", s)
		}
	}
	env, ok := llmisvc["env"].(map[string]any)
	if llmisvc["env"] != nil && !ok {
		return fmt.Errorf("llmisvc.env must be a map of name: value")
	}
	for name := range env {
		switch {
		case !envName.MatchString(name):
			return fmt.Errorf("llmisvc.env: %q is not a variable name", name)
		case name == "VLLM_ADDITIONAL_ARGS":
			return fmt.Errorf("llmisvc.env: VLLM_ADDITIONAL_ARGS goes through bash eval; use llmisvc.args")
		}
	}
	return nil
}
