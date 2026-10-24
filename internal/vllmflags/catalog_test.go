package vllmflags

import "testing"

// An image's own version decides whether the check may refuse a save. The
// tag of an LMCache build is LMCache's version, and a digest or a version
// without a list says nothing: those are checked against the newest list,
// as warnings.
func TestForImageTrustsOnlyAVLLMBuildsTag(t *testing.T) {
	cases := []struct {
		image string
		exact bool
	}{
		{"docker.io/vllm/vllm-openai-cpu:v0.32.0", true},
		{"registry-internal/vllm/vllm-openai:v0.32.0", true},
		{"docker.io/vllm/vllm-openai-cpu:v0.32.0-x86_64", true},
		{"registry-internal:5000/mirror/vllm-cuda:v0.32.0", true},
		{"registry-internal/vllm/vllm-openai:v0.30.0", false},
		{"registry-internal/lmcache/lmcache/vllm-openai:v0.32.0", false},
		{"registry-internal/vllm/vllm-openai@sha256:0123", false},
		{"registry-internal/vllm/vllm-openai:latest", false},
		{"registry.redhat.io/rhaiis/vllm-cuda-rhel9:3.2.1", false},
		{"", false},
	}
	for _, tc := range cases {
		c, exact, why := ForImage(tc.image)
		if c == nil || exact != tc.exact || (why == "") != exact {
			t.Errorf("%q: list %v, exact %v (want %v), why %q", tc.image, c != nil, exact, tc.exact, why)
		}
	}
}

// Every list loads, and each knows the flags the hub's own command builder
// writes (a list without them would make every check fail).
func TestCatalogsKnowKServesFlags(t *testing.T) {
	all, err := Catalogs()
	if err != nil || len(all) == 0 {
		t.Fatalf("lists: %v, %v", all, err)
	}
	for _, c := range all {
		for _, o := range []string{"--served-model-name", "--port", "--tensor-parallel-size", "--data-parallel-size-local", "--max-model-len", "--kv-transfer-config"} {
			if c.Flag(o) == nil {
				t.Errorf("vLLM %s: no %s", c.Version, o)
			}
		}
	}
}

// A flag set by KServe and set again by a model's values is refused; a
// soft one (KServe's shutdown timeout) is only noted; the cluster's flag
// that its recipe's flag overrides is refused with both places named.
func TestDuplicatesNameWhoSetWhat(t *testing.T) {
	all, _ := Catalogs()
	c := all[len(all)-1]
	args := []Arg{
		{Text: "/mnt/models", Source: "KServe", Fixed: true},
		{Text: "--port", Source: "KServe", Fixed: true}, {Text: "8000", Source: "KServe", Fixed: true},
		{Text: "--shutdown-timeout", Source: "KServe", Fixed: true, Soft: true}, {Text: "280", Source: "KServe", Fixed: true, Soft: true},
		{Text: "--max-num-seqs=32", Source: "llmisvc.extraArgs"},
		{Text: "--max_num_seqs", Source: "llmisvc.args"}, {Text: "64", Source: "llmisvc.args"},
		{Text: "--port=9000", Source: "llmisvc.args"},
		{Text: "--shutdown-timeout=10", Source: "llmisvc.args"},
	}
	_, probs := c.Check(args)
	want := map[string]string{
		"KServe sets --port=8000 already: llmisvc.args sets it again and wins (--port=9000); remove it from llmisvc.args":                             "error",
		"KServe sets --shutdown-timeout=280; llmisvc.args changes it to --shutdown-timeout=10":                                                        "warning",
		"--max-num-seqs is set twice: --max-num-seqs=32 in llmisvc.extraArgs, --max-num-seqs=64 in llmisvc.args; vLLM takes the last, without a word": "error",
	}
	got := map[string]string{}
	for _, p := range probs {
		got[p.Message] = p.Level
	}
	for m, level := range want {
		if got[m] != level {
			t.Errorf("no %s %q in %v", level, m, got)
		}
	}
}
