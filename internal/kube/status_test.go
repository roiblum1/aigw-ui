package kube

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPatchVerdict(t *testing.T) {
	policy := func(conditions ...map[string]any) *unstructured.Unstructured {
		list := make([]any, 0, len(conditions))
		for _, c := range conditions {
			list = append(list, c)
		}
		return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"ancestors": []any{map[string]any{"conditions": list}}}}}
	}
	cond := func(kind, status, message string) map[string]any {
		return map[string]any{"type": kind, "status": status, "message": message}
	}
	for name, tc := range map[string]struct {
		policy  *unstructured.Unstructured
		verdict string
	}{
		"in effect":           {policy(cond("Accepted", "True", ""), cond("Programmed", "True", "")), "Programmed"},
		"patches not enabled": {policy(cond("Accepted", "False", "EnvoyPatchPolicy is disabled")), "NotAccepted"},
		"nothing to patch":    {policy(cond("Accepted", "True", ""), cond("Programmed", "False", "no match")), "NotProgrammed"},
		"nothing reported":    {&unstructured.Unstructured{Object: map[string]any{}}, ""},
		"accepted only":       {policy(cond("Accepted", "True", "")), ""},
	} {
		if got, _ := patchVerdict(tc.policy, "Programmed"); got != tc.verdict {
			t.Errorf("%s: verdict %q, want %q", name, got, tc.verdict)
		}
	}

	// The tenants' budget policy as Envoy Gateway 1.9.2 reported it while a
	// fleet entry route had a policy of its own (lab, 2026-10-16).
	for name, tc := range map[string]struct {
		policy  *unstructured.Unstructured
		verdict string
	}{
		"accepted":   {policy(cond("Accepted", "True", "Policy has been accepted.")), "Accepted"},
		"merged":     {policy(cond("Accepted", "True", ""), cond("Merged", "True", "This policy is being merged by other backendTrafficPolicies")), "Accepted"},
		"overridden": {policy(cond("Accepted", "True", ""), cond("Overridden", "True", "This policy is being overridden by other backendTrafficPolicies for these routes: [ns/fleet-m]")), "Overridden"},
	} {
		got, message := patchVerdict(tc.policy, "Accepted")
		if got != tc.verdict {
			t.Errorf("%s: verdict %q, want %q", name, got, tc.verdict)
		}
		if got == "Overridden" && !strings.Contains(message, "fleet-m") {
			t.Errorf("%s: message %q does not name the route", name, message)
		}
	}
}
