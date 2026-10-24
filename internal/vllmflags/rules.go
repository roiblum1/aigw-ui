package vllmflags

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Check reads a command and returns everything wrong with it: what vLLM
// would refuse (a flag it does not have, a bad value, a bad combination)
// and what the hub refuses on its own (a flag set twice).
func (c *Catalog) Check(args []Arg) (*Command, []Problem) {
	cmd, probs := c.Parse(args)
	probs = append(probs, cmd.duplicates()...)
	probs = append(probs, cmd.startupConfigs()...)
	probs = append(probs, cmd.rules()...)
	if cmd.has("config") {
		s, _ := cmd.last("config")
		probs = append(probs, hubProblem("warning", s.Arg, "--config", "the flags in the --config file are read in the pod; the hub does not check them"))
	}
	return cmd, probs
}

func (cmd *Command) last(dest string) (Setting, bool) {
	s := cmd.Set[dest]
	if len(s) == 0 {
		return Setting{}, false
	}
	return s[len(s)-1], true
}

// has says whether dest is set to something other than None.
func (cmd *Command) has(dest string) bool {
	s, ok := cmd.last(dest)
	return ok && s.Value != nil
}

// Int is dest's integer when the command sets it.
func (cmd *Command) Int(dest string) (int64, bool) {
	s, ok := cmd.last(dest)
	if !ok {
		return 0, false
	}
	n, ok := s.Value.(int64)
	return n, ok
}

// IntOr is dest's integer: set, else the flag's default, else def.
func (cmd *Command) IntOr(dest string, def int64) int64 {
	if n, ok := cmd.Int(dest); ok {
		return n
	}
	if f := cmd.Catalog.FlagOf(dest); f != nil && len(f.Default) > 0 {
		var n int64
		if json.Unmarshal(f.Default, &n) == nil {
			return n
		}
	}
	return def
}

// On says whether a switch or boolean is on: set, else its default.
func (cmd *Command) On(dest string) bool {
	if s, ok := cmd.last(dest); ok {
		b, _ := s.Value.(bool)
		return b
	}
	if f := cmd.Catalog.FlagOf(dest); f != nil {
		return string(f.Default) == "true"
	}
	return false
}

// Str is dest's text when the command sets it to a non-empty one.
func (cmd *Command) Str(dest string) (string, bool) {
	s, ok := cmd.last(dest)
	if !ok {
		return "", false
	}
	t, ok := s.Value.(string)
	return t, ok && t != ""
}

// Shown is a setting as written: --flag=value, or --flag for a switch.
func (s Setting) Shown() string {
	if s.Text == "" {
		return s.Option
	}
	return s.Option + "=" + s.Text
}

// duplicates finds a flag set more than once. vLLM takes the last one and
// says nothing, so a cluster's flag in llmisvc.extraArgs that its recipe's
// llmisvc.args sets too (they come after) does nothing, and a flag KServe
// sets is changed under it.
func (cmd *Command) duplicates() []Problem {
	var out []Problem
	dests := make([]string, 0, len(cmd.Set))
	for d := range cmd.Set {
		dests = append(dests, d)
	}
	sort.Strings(dests)
	for _, d := range dests {
		s := cmd.Set[d]
		if len(s) < 2 || s[0].Flag.Kind == "append" || s[0].Flag.Kind == "count" {
			continue
		}
		last := s[len(s)-1]
		name := last.Flag.Name()
		for _, prev := range s[:len(s)-1] {
			switch {
			case prev.Arg.Fixed && last.Arg.Fixed:
			case prev.Arg.Fixed && prev.Arg.Soft:
				out = append(out, hubProblem("warning", last.Arg, name, "%s sets %s; %s changes it to %s", prev.Arg.Source, prev.Shown(), last.Arg.Source, last.Shown()))
			case prev.Arg.Fixed:
				out = append(out, hubProblem("error", last.Arg, name, "%s sets %s already: %s sets it again and wins (%s); remove it from %s", prev.Arg.Source, prev.Shown(), last.Arg.Source, last.Shown(), last.Arg.Source))
			case last.Arg.Fixed:
				out = append(out, hubProblem("error", prev.Arg, name, "%s sets %s, and %s sets it after it (%s), which wins: set it in one place", prev.Arg.Source, prev.Shown(), last.Arg.Source, last.Shown()))
			default:
				out = append(out, hubProblem("error", last.Arg, name, "%s is set twice: %s in %s, %s in %s; vLLM takes the last, without a word", name, prev.Shown(), prev.Arg.Source, last.Shown(), last.Arg.Source))
			}
		}
	}
	return out
}

// startupConfigs checks the JSON flags vLLM turns into a config only when
// the engine starts (SpeculativeConfig(**value)): a key the config does not
// have, or a value its field does not take, stops vLLM there.
func (cmd *Command) startupConfigs() []Problem {
	var out []Problem
	for _, d := range sortedDests(cmd) {
		s, _ := cmd.last(d)
		f := s.Flag
		obj, ok := s.Value.(map[string]any)
		if f.ConfigAt != "startup" || !ok {
			continue
		}
		cfg := cmd.Catalog.Configs[f.Config]
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			field, known := cfg.Fields[k]
			var err error
			if !known {
				err = fmt.Errorf("%s is not a key of %s%s", k, f.Config, closest(k, cfg.Fields))
			} else {
				err = cmd.Catalog.checkField(f.Config, k, field, obj[k])
			}
			if err != nil {
				out = append(out, vllmError(s.Arg, f.Name(), "%s: %v (vLLM stops when the engine starts)", f.Name(), err))
			}
		}
	}
	return out
}

func sortedDests(cmd *Command) []string {
	out := make([]string, 0, len(cmd.Set))
	for d := range cmd.Set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// rules are the checks vLLM runs on its arguments before it loads a model:
// validate_parsed_serve_args (the quick serve checks and, for the chart's
// data parallel over ports, validate_multi_port_external_lb_args),
// create_engine_config's data-parallel launch checks, ParallelConfig's and
// SchedulerConfig's validators. A value vLLM picks per GPU when it is left
// out is not compared.
func (cmd *Command) rules() []Problem {
	var out []Problem
	refuse := func(dest, format string, args ...any) {
		s, _ := cmd.last(dest)
		name := dest
		if s.Flag != nil {
			name = s.Flag.Name()
		} else if f := cmd.Catalog.FlagOf(dest); f != nil {
			name = f.Name()
		}
		out = append(out, vllmError(s.Arg, name, format, args...))
	}
	if cmd.Model != "" && cmd.moved {
		refuse("model", "--model names the model a second time: KServe passes it as %s, and vLLM refuses the extra word", cmd.Model)
	}

	// The quick serve checks.
	if cmd.On("enable_auto_tool_choice") && !cmd.has("tool_call_parser") {
		refuse("enable_auto_tool_choice", "--enable-auto-tool-choice needs --tool-call-parser")
	}
	if cmd.On("enable_log_outputs") && !cmd.On("enable_log_requests") {
		refuse("enable_log_outputs", "--enable-log-outputs needs --enable-log-requests")
	}
	if n, ok := cmd.Int("sse_keep_alive_interval"); ok && n < 0 {
		refuse("sse_keep_alive_interval", "--sse-keep-alive-interval must be 0 (off) or more")
	}
	if cmd.On("enable_per_request_metrics") && cmd.On("disable_log_stats") {
		refuse("enable_per_request_metrics", "--enable-per-request-metrics needs the engine's statistics: remove --disable-log-stats")
	}
	if cmd.On("grpc") && cmd.has("grpc_port") {
		refuse("grpc_port", "--grpc and --grpc-port cannot be used together")
	}

	dp := cmd.IntOr("data_parallel_size", 1)
	local, hasLocal := cmd.Int("data_parallel_size_local")
	hasRank := cmd.has("data_parallel_rank")
	externalLB, hybridLB := cmd.On("data_parallel_external_lb"), cmd.On("data_parallel_hybrid_lb")

	// Data parallel over ports, one rank per port (the chart's
	// dataParallelExternalLB).
	if cmd.On("data_parallel_multi_port_external_lb") {
		const mp = "data_parallel_multi_port_external_lb"
		if cmd.On("grpc") {
			refuse(mp, "--data-parallel-multi-port-external-lb does not support --grpc")
		}
		if cmd.has("uds") {
			refuse(mp, "--data-parallel-multi-port-external-lb does not support --uds")
		}
		if cmd.has("ssl_keyfile") != cmd.has("ssl_certfile") {
			refuse(mp, "--ssl-keyfile and --ssl-certfile go together")
		}
		if n, ok := cmd.Int("api_server_count"); ok && n != 1 {
			refuse("api_server_count", "--data-parallel-multi-port-external-lb needs --api-server-count=1 (it is %d)", n)
		}
		if hasRank {
			refuse("data_parallel_rank", "--data-parallel-multi-port-external-lb sets each rank itself: remove --data-parallel-rank")
		}
		if externalLB || hybridLB {
			refuse(mp, "--data-parallel-multi-port-external-lb cannot go with --data-parallel-external-lb or --data-parallel-hybrid-lb")
		}
		switch {
		case dp < 2:
			refuse(mp, "--data-parallel-multi-port-external-lb needs --data-parallel-size above 1")
		case !hasLocal || local < 2:
			refuse(mp, "--data-parallel-multi-port-external-lb needs --data-parallel-size-local of 2 or more")
		case local > dp:
			refuse("data_parallel_size_local", "--data-parallel-size-local (%d) cannot be above --data-parallel-size (%d)", local, dp)
		case dp%local != 0:
			refuse("data_parallel_size", "--data-parallel-size (%d) must be a multiple of --data-parallel-size-local (%d)", dp, local)
		default:
			start, ok := cmd.Int("data_parallel_start_rank")
			if !ok {
				start = cmd.IntOr("node_rank", 0) * local
			}
			if start+local > dp {
				refuse("data_parallel_start_rank", "the ranks on this node (%d from %d) would pass --data-parallel-size (%d)", local, start, dp)
			}
			port := cmd.IntOr("port", 8000)
			if sup, ok := cmd.Int("data_parallel_supervisor_port"); ok && sup >= port && sup <= port+local-1 {
				refuse("data_parallel_supervisor_port", "--data-parallel-supervisor-port %d is one of the ranks' ports (%d-%d)", sup, port, port+local-1)
			}
		}
	}

	// create_engine_config's data-parallel launch checks.
	if hybridLB && externalLB {
		refuse("data_parallel_hybrid_lb", "--data-parallel-hybrid-lb and --data-parallel-external-lb cannot be on together")
	}
	external := externalLB || hasRank
	if external && cmd.IntOr("nnodes", 1) <= 1 {
		if !hasRank {
			refuse("data_parallel_external_lb", "--data-parallel-external-lb needs --data-parallel-rank")
		} else if hasLocal && local != 1 {
			refuse("data_parallel_size_local", "an external data-parallel rank needs --data-parallel-size-local 1 (it is %d)", local)
		}
	}

	// ParallelConfig.
	if hasLocal && local > dp {
		refuse("data_parallel_size_local", "--data-parallel-size-local (%d) must be at most --data-parallel-size (%d)", local, dp)
	}
	if dp <= 1 && external {
		refuse("data_parallel_external_lb", "external data-parallel load balancing needs --data-parallel-size above 1")
	}
	if !cmd.On("numa_bind") && (cmd.has("numa_bind_nodes") || cmd.has("numa_bind_cpus")) {
		refuse("numa_bind_nodes", "--numa-bind-nodes and --numa-bind-cpus need --numa-bind")
	}
	tp, pp := cmd.IntOr("tensor_parallel_size", 1), cmd.IntOr("pipeline_parallel_size", 1)
	pcp, dcp := cmd.IntOr("prefill_context_parallel_size", 1), cmd.IntOr("decode_context_parallel_size", 1)
	eplb := cmd.On("enable_eplb")
	if eplb {
		if !cmd.On("enable_expert_parallel") {
			refuse("enable_eplb", "--enable-eplb needs --enable-expert-parallel")
		} else if tp*pcp*dp <= 1 {
			refuse("enable_eplb", "--enable-eplb needs tensor, prefill-context or data parallelism (TP=%d, PCP=%d, DP=%d)", tp, pcp, dp)
		}
	} else if s, ok := cmd.last("eplb_config"); ok {
		if m, ok := s.Value.(map[string]any); ok {
			if n, ok := normalNumber(m["num_redundant_experts"]).(int64); ok && n != 0 {
				refuse("eplb_config", "--eplb-config num_redundant_experts is %d, but EPLB is off (--enable-eplb)", n)
			}
		}
	}
	if pcp == 1 && dcp > 0 && tp%dcp != 0 {
		refuse("decode_context_parallel_size", "--tensor-parallel-size (%d) must be a multiple of --decode-context-parallel-size (%d)", tp, dcp)
	} else if pcp > 1 && dcp != 1 && dcp != pcp && dcp != tp*pcp {
		refuse("decode_context_parallel_size", "with prefill context parallel, --decode-context-parallel-size must be 1, %d or %d (it is %d)", pcp, tp*pcp, dcp)
	}
	if cmd.On("enable_elastic_ep") {
		switch {
		case !eplb:
			refuse("enable_elastic_ep", "--enable-elastic-ep needs --enable-eplb")
		case pp > 1:
			refuse("enable_elastic_ep", "--enable-elastic-ep does not work with pipeline parallelism (%d)", pp)
		case external || hybridLB:
			refuse("enable_elastic_ep", "--enable-elastic-ep does not work with external or hybrid data-parallel load balancing")
		}
	}

	// SchedulerConfig.
	length, hasLength := cmd.Int("max_model_len")
	hasLength = hasLength && length > 0
	batched, hasBatched := cmd.Int("max_num_batched_tokens")
	seqs, hasSeqs := cmd.Int("max_num_seqs")
	if hasBatched && hasLength && batched < length {
		if s, ok := cmd.last("enable_chunked_prefill"); ok && s.Value == false {
			refuse("max_num_batched_tokens", "--max-num-batched-tokens (%d) is below --max-model-len (%d) with chunked prefill off: raise it or turn chunked prefill on", batched, length)
		}
	}
	if hasBatched && hasSeqs && batched < seqs {
		refuse("max_num_batched_tokens", "--max-num-batched-tokens (%d) must be at least --max-num-seqs (%d)", batched, seqs)
	}
	if active, ok := cmd.Int("max_num_active_seqs"); ok && hasSeqs && active > seqs {
		refuse("max_num_active_seqs", "--max-num-active-seqs (%d) cannot be above --max-num-seqs (%d)", active, seqs)
	}
	if lp, ok := cmd.Int("long_prefill_token_threshold"); ok && lp > 0 && hasLength && lp > length {
		refuse("long_prefill_token_threshold", "--long-prefill-token-threshold (%d) cannot be above --max-model-len (%d)", lp, length)
	}
	return out
}

// Errors are the problems that refuse a save.
func Errors(probs []Problem) []Problem {
	var out []Problem
	for _, p := range probs {
		if p.Level == "error" {
			out = append(out, p)
		}
	}
	return out
}

// Summary is the problems in one line, for an error message.
func Summary(probs []Problem) string {
	var parts []string
	for _, p := range probs {
		parts = append(parts, p.Message)
	}
	return strings.Join(parts, "; ")
}
