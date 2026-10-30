package vllmflags

import (
	"reflect"
	"strings"
	"testing"
)

func newest(t *testing.T) *Catalog {
	t.Helper()
	all, err := Catalogs()
	if err != nil || len(all) == 0 {
		t.Fatalf("lists: %v, %v", all, err)
	}
	return all[len(all)-1]
}

// The list is vLLM's help as printed, so each shape of flag line must read
// right: a pair, a short spelling, an optional value, several values, a
// fixed set of choices, and a list of names that is not a fixed set.
func TestHelpGivesEachFlagsShape(t *testing.T) {
	c := newest(t)
	if c.Version != "0.32.0" || c.Image != "docker.io/vllm/vllm-openai-cpu:v0.32.0" || len(c.Flags) != 316 {
		t.Fatalf("vLLM %s from %q: %d flags (the help lists 316 and -h)", c.Version, c.Image, len(c.Flags))
	}
	cases := []struct {
		option, dest, value string
		options             []string
		choices             bool
	}{
		{"--tensor-parallel-size", "tensor_parallel_size", OneValue, []string{"--tensor-parallel-size", "-tp"}, false},
		{"--no-enable-prefix-caching", "enable_prefix_caching", NoValue, []string{"--enable-prefix-caching", "--no-enable-prefix-caching"}, false},
		{"--disable-log-stats", "disable_log_stats", NoValue, []string{"--disable-log-stats"}, false},
		{"--hf-token", "hf_token", Optional, []string{"--hf-token"}, false},
		{"--served-model-name", "served_model_name", OneOrMore, []string{"--served-model-name"}, false},
		{"--dtype", "dtype", OneValue, []string{"--dtype"}, true},
		{"--tool-call-parser", "tool_call_parser", OneValue, []string{"--tool-call-parser"}, false},
		{"--tokenizer-mode", "tokenizer_mode", OneValue, []string{"--tokenizer-mode"}, false},
		{"-q", "quantization", OneValue, []string{"--quantization", "-q"}, false},
	}
	for _, tc := range cases {
		f := c.Flag(tc.option)
		if f == nil {
			t.Errorf("%s: not in the list", tc.option)
			continue
		}
		if f.Dest != tc.dest || f.Value != tc.value || !reflect.DeepEqual(f.Options, tc.options) || (len(f.Choices) > 0) != tc.choices {
			t.Errorf("%s: dest %q value %q options %q choices %q", tc.option, f.Dest, f.Value, f.Options, f.Choices)
		}
	}
	if c.Flag("--help") != nil || c.Flag("-h") != nil {
		t.Error("--help is listed: it would print the help and stop the pod")
	}
}

func refusals(c *Catalog, words ...string) (errs, warns []string) {
	args := make([]Arg, len(words))
	for i, w := range words {
		args[i] = Arg{Text: w, Source: "test"}
	}
	_, probs := c.Check(args)
	for _, p := range probs {
		if p.Level == "error" {
			errs = append(errs, p.Message)
		} else {
			warns = append(warns, p.Message)
		}
	}
	return errs, warns
}

// What vLLM takes the check takes, including the spellings vLLM rewrites
// before it reads its flags (underscores, -O3, dotted keys) and argparse's
// abbreviations. A refusal blocks a save, so it must never be wrong.
func TestCheckTakesWhatVLLMTakes(t *testing.T) {
	c := newest(t)
	for _, words := range [][]string{
		{"/mnt/models", "--served-model-name", "judge", "publishers/ns/models/judge", "--port", "8000"},
		{"/mnt/models", "--max_model_len", "4096", "--enable-prefix-caching", "--no-enable-log-requests"},
		{"/mnt/models", "-O3", "-tp", "2", "-qfp8"},
		{"/mnt/models", "-O", "2"},
		{"/mnt/models", "-O=1"},
		{"/mnt/models", "--speculative-config.method", "mtp", "--speculative_config.num_speculative_tokens=2", "-cc.mode", "3"},
		{"/mnt/models", `--default-chat-template-kwargs={"enable_thinking": false}`, "--kv-transfer-config", `{"kv_connector":"X","kv_role":"kv_both"}`},
		{"/mnt/models", "--dtype", "half", "--moe-backend", "deep-gemm", "--tool-call-parser", "my_plugin_parser"},
		{"/mnt/models", "--hf-token", "--max-num-seqs", "-1"},
		{"/mnt/models", "--compilation-config.cudagraph_capture_sizes+", "1,2,4"},
	} {
		errs, _ := refusals(c, words...)
		if len(errs) > 0 {
			t.Errorf("%q: refused %q", words, errs)
		}
	}
	if _, warns := refusals(c, "/mnt/models", "--max-num-seq", "8"); len(warns) != 1 || !strings.Contains(warns[0], "--max-num-seq is short for --max-num-seqs") {
		t.Errorf("an abbreviation: warnings %q", warns)
	}
}

// What vLLM refuses as it reads its flags, and the help shows, is refused
// with the flag named.
func TestCheckRefusesWhatTheHelpRulesOut(t *testing.T) {
	c := newest(t)
	cases := []struct {
		words []string
		want  string
	}{
		{[]string{"/mnt/models", "--enable-prefix-cache"}, "--enable-prefix-cache is not a flag of vLLM 0.32.0 (did you mean --enable-prefix-caching?)"},
		{[]string{"/mnt/models", "--max-num", "8"}, "--max-num is short for more than one flag"},
		{[]string{"/mnt/models", "--max-model-len"}, "--max-model-len needs a value"},
		{[]string{"/mnt/models", "--served-model-name"}, "--served-model-name needs at least one value"},
		{[]string{"/mnt/models", "--enforce-eager=true"}, `--enforce-eager takes no value ("true"): write --enforce-eager or --no-enforce-eager`},
		{[]string{"/mnt/models", "--dtype", "halfx"}, `--dtype: "halfx" is not one of its choices (auto, bfloat16, float, float16, float32, half)`},
		{[]string{"/mnt/models", "--enforce-eager", "extra"}, `"extra" is not a value of any flag: vLLM refuses the extra word`},
		{[]string{"/mnt/models", "--speculative-confg.method", "mtp"}, "--speculative-confg is not a flag of vLLM 0.32.0 (did you mean --speculative-config?)"},
	}
	for _, tc := range cases {
		errs, _ := refusals(c, tc.words...)
		if len(errs) != 1 || !strings.Contains(errs[0], tc.want) {
			t.Errorf("%q: errors %q, want %q", tc.words, errs, tc.want)
		}
	}
}
