package gitops

import (
	"reflect"
	"strings"
	"testing"

	"aigw-ui/internal/vllmflags"
)

const vllm032 = "docker.io/vllm/vllm-openai-cpu:v0.32.0"

func texts(args []vllmflags.Arg) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a.Text
	}
	return out
}

func values(image string, llmisvc map[string]any) map[string]any {
	return map[string]any{
		"namespace": "llm-judge",
		"images":    map[string]any{"vllm": image},
		"model":     map[string]any{"name": "judge"},
		"llmisvc":   llmisvc,
	}
}

// The command is the one KServe rendered for the judge on the exported
// site (live-cluster/ns-llm-judge): its preset's words, the tensor parallel
// from spec.parallelism, then the chart's arguments with the quotes bash
// removes.
func TestLaunchIsWhatKServeRenders(t *testing.T) {
	v := values(vllm032, map[string]any{
		"parallelism": map[string]any{"tensor": 2},
		"args":        []any{"--max-model-len=16384", "--reasoning-parser=qwen3", `--default-chat-template-kwargs='{"enable_thinking": false}'`},
	})
	got := texts(LaunchOf(v, "0.32.0").Leader)
	want := []string{"/mnt/models", "--served-model-name", "judge", "publishers/llm-judge/models/judge", "--port", "8000",
		"--disable-access-log-for-endpoints", "/health,/metrics,/ping", "--shutdown-timeout", "280", "--tensor-parallel-size", "2",
		"--max-model-len=16384", "--reasoning-parser=qwen3", `--default-chat-template-kwargs={"enable_thinking": false}`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command:\n got %q\nwant %q", got, want)
	}
}

// Data parallel over ports and LMCache: the chart's words around the
// cluster's and the recipe's flags, LMCache's connector exactly as the
// chart's toJson writes it (glm53 on the exported site).
func TestLaunchHasTheChartsDataParallelAndLMCache(t *testing.T) {
	v := values(vllm032, map[string]any{
		"dataParallelExternalLB": map[string]any{"ranks": 8},
		"extraArgs":              []any{"--enable-expert-parallel"},
		"args":                   []any{"--max-num-seqs=64"},
	})
	v["namespace"] = "llm-glm53"
	v["lmcache"] = map[string]any{"enabled": true, "name": "glm53-cache", "isolatedIPC": true, "server": map[string]any{"port": 5555}}
	got := texts(LaunchOf(v, "0.32.0").Leader)[10:] // after KServe's 10 words
	want := []string{"--data-parallel-size=8", "--data-parallel-size-local=8", "--data-parallel-multi-port-external-lb", "--data-parallel-supervisor-port=8208",
		"--enable-expert-parallel", "--max-num-seqs=64", "--kv-transfer-config",
		`{"kv_connector":"LMCacheMPConnector","kv_connector_extra_config":{"lmcache.mp.host":"tcp://glm53-cache.llm-glm53.svc.cluster.local","lmcache.mp.isolated_ipc":true,"lmcache.mp.port":"5555"},"kv_connector_module_path":"lmcache.integration.vllm.lmcache_mp_connector","kv_role":"kv_both"}`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chart's words:\n got %q\nwant %q", got, want)
	}
}

// Multi-node: the leader and the workers run KServe's data-parallel
// preset, the workers headless from their first rank.
func TestLaunchOnTwoNodes(t *testing.T) {
	v := values(vllm032, map[string]any{
		"multiNode":   true,
		"parallelism": map[string]any{"data": 8, "dataLocal": 4, "tensor": 2, "expert": true},
	})
	l := LaunchOf(v, "0.32.0")
	leader := strings.Join(texts(l.Leader), " ")
	worker := strings.Join(texts(l.Worker), " ")
	for _, s := range []string{"--api-server-count 8 --enable-expert-parallel --tensor-parallel-size 2 --data-parallel-size 8 --data-parallel-size-local 4", "--data-parallel-start-rank 0 --disable-access-log"} {
		if !strings.Contains(leader, s) {
			t.Errorf("leader %q lacks %q", leader, s)
		}
	}
	if !strings.Contains(worker, "--data-parallel-start-rank 4 --headless") || strings.Contains(worker, "--api-server-count") {
		t.Errorf("worker %q", worker)
	}
}

func messages(probs []vllmflags.Problem, level string) []string {
	var out []string
	for _, p := range probs {
		if p.Level == level {
			out = append(out, p.Origin+": "+p.Message)
		}
	}
	return out
}

func has(list []string, part string) bool {
	for _, s := range list {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

// The check finds what vLLM would refuse and the hub's own rules, and says
// where each flag came from.
func TestCheckFlagsFindsWhatWouldStopThePod(t *testing.T) {
	good := []any{"--enable-prompt-tokens-details", "--enable-force-include-usage"}
	cases := []struct {
		name    string
		llmisvc map[string]any
		errors  []string
		warns   []string
	}{
		{"fine", map[string]any{"args": good}, nil, nil},
		{"a misspelt flag in the cluster's flags", map[string]any{"args": good, "extraArgs": []any{"--enable-prefix-cache"}},
			[]string{"vllm: --enable-prefix-cache is not a flag of vLLM 0.32.0 (did you mean --enable-prefix-caching?)"}, nil},
		{"a cluster's flag the recipe sets after it", map[string]any{"args": append([]any{"--gpu-memory-utilization=0.85"}, good...), "extraArgs": []any{"--gpu-memory-utilization=0.9"}},
			[]string{"hub: --gpu-memory-utilization is set twice: --gpu-memory-utilization=0.9 in llmisvc.extraArgs, --gpu-memory-utilization=0.85 in llmisvc.args"}, nil},
		{"KServe's port", map[string]any{"args": append([]any{"--port=9000"}, good...)},
			[]string{"hub: KServe sets --port=8000 already: llmisvc.args sets it again and wins (--port=9000)"}, nil},
		{"KServe's shutdown timeout changed", map[string]any{"args": append([]any{"--shutdown-timeout=60"}, good...)},
			nil, []string{"hub: KServe sets --shutdown-timeout=280; llmisvc.args changes it to --shutdown-timeout=60"}},
		{"fewer GPUs than ranks", map[string]any{"args": good, "dataParallelExternalLB": map[string]any{"ranks": 8},
			"resources": map[string]any{"limits": map[string]any{"nvidia.com/gpu": "4"}}},
			[]string{"hub: vLLM needs 8 GPUs per pod (tensor 1 × pipeline 1 × prefill context 1 × data-parallel ranks in the pod 8), but the pod asks for 4 nvidia.com/gpu"}, nil},
		{"more GPUs than used", map[string]any{"args": good, "parallelism": map[string]any{"tensor": 4},
			"resources": map[string]any{"limits": map[string]any{"nvidia.com/gpu": 8}}},
			nil, []string{"hub: the pod asks for 8 nvidia.com/gpu, and vLLM uses 4"}},
		{"two nodes that do not divide", map[string]any{"args": good, "multiNode": true, "parallelism": map[string]any{"data": 8, "dataLocal": 3}},
			[]string{"hub: llmisvc.parallelism.data (8) must be a multiple of dataLocal (3)"}, nil},
		{"the billing flags off", map[string]any{}, nil,
			[]string{"hub: --enable-prompt-tokens-details is off", "hub: --enable-force-include-usage is off"}},
	}
	for _, tc := range cases {
		fc := CheckFlags(values(vllm032, tc.llmisvc))
		if !fc.Exact || fc.Version != "0.32.0" {
			t.Fatalf("%s: checked against %q (exact %v)", tc.name, fc.Version, fc.Exact)
		}
		errs, warns := messages(fc.Problems, "error"), messages(fc.Problems, "warning")
		if len(errs) != len(tc.errors) {
			t.Errorf("%s: errors %q, want %q", tc.name, errs, tc.errors)
		}
		for _, e := range tc.errors {
			if !has(errs, e) {
				t.Errorf("%s: no error %q in %q", tc.name, e, errs)
			}
		}
		for _, w := range tc.warns {
			if !has(warns, w) {
				t.Errorf("%s: no warning %q in %q", tc.name, w, warns)
			}
		}
	}
}

// An image whose vLLM version the hub cannot tell (an LMCache build, a
// version without a list) is checked against the newest list, and nothing
// it finds refuses a save.
func TestCheckFlagsOfAnUnknownVersionOnlyWarns(t *testing.T) {
	for _, image := range []string{"registry-internal/lmcache/lmcache/vllm-openai:v0.5.5", "registry-internal/vllm/vllm-openai:v0.30.0", ""} {
		fc := CheckFlags(values(image, map[string]any{"args": []any{"--enable-prefix-cache"}}))
		if fc.Exact || fc.Note == "" || len(fc.Errors()) != 0 || len(fc.Problems) == 0 {
			t.Errorf("%q: exact %v, note %q, errors %v, problems %v", image, fc.Exact, fc.Note, fc.Errors(), fc.Problems)
		}
	}
}
