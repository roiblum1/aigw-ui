package gitops

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"aigw-ui/internal/vllmflags"
)

// The vLLM command of a model's pods, as KServe and the model-deployer
// chart build it from the values, and the hub's check of it before a save.
// KServe passes the arguments to vLLM as they are: a flag vLLM does not
// have stops the pod when it starts, minutes after Argo CD synced, and a
// flag set twice is taken from the last place without a word.

// Launch is the command of a model's pods, word by word, as vLLM gets it:
// KServe's launcher (the kserve-config-llm-template preset; on multi-node
// kserve-config-llm-worker-data-parallel), then the chart's arguments.
type Launch struct {
	Leader []vllmflags.Arg
	// Worker is the workers' command on multi-node (llmisvc.multiNode).
	Worker []vllmflags.Arg
}

// LaunchOf builds the command from a model's merged values, for a vLLM
// version (KServe adds some flags only from a version on). KServe's TLS
// flags (its global EnableTLS) are left out: the hub does not know them.
func LaunchOf(v map[string]any, version string) Launch {
	kserve := func(why string, texts ...string) []vllmflags.Arg {
		var out []vllmflags.Arg
		for _, t := range texts {
			out = append(out, vllmflags.Arg{Text: t, Source: why, Fixed: true})
		}
		return out
	}
	soft := func(args []vllmflags.Arg) []vllmflags.Arg {
		for i := range args {
			args[i].Soft = true
		}
		return args
	}
	name, ns := str(v, "model", "name"), str(v, "namespace")
	head := kserve("KServe", "/mnt/models", "--served-model-name", name, "publishers/"+ns+"/models/"+name, "--port", "8000")

	user := append(argsOf(v, "extraArgs"), argsOf(v, "args")...)
	var tail []vllmflags.Arg // after the parallel flags, before $@
	if vllmflags.AtLeast(version, "0.16.0") {
		tail = append(tail, soft(kserve("KServe", "--disable-access-log-for-endpoints", "/health,/metrics,/ping"))...)
	} else {
		tail = append(tail, soft(kserve("KServe", "--disable-uvicorn-access-log"))...)
	}
	if vllmflags.AtLeast(version, "0.18.0") {
		tail = append(tail, soft(kserve("KServe", "--shutdown-timeout", "280"))...)
	}
	if off := mapAt(v, "llmisvc", "kvCacheOffloading"); len(off) > 0 && !on(v, "lmcache", "enabled") && vllmflags.AtLeast(version, "0.22.0") && !hasKVTransfer(user) {
		policy := str(v, "llmisvc", "kvCacheOffloading", "evictionPolicy")
		if policy == "" {
			policy = "lru"
		}
		cfg := map[string]any{"kv_connector": "OffloadingConnector", "kv_role": "kv_both", "kv_connector_extra_config": map[string]any{
			"cpu_bytes_to_use": quantity(off["cpu"]), "eviction_policy": policy, "spec_name": "TieringOffloadingSpec"}}
		tail = append(tail, kserve("KServe (llmisvc.kvCacheOffloading)", "--kv-transfer-config", jsonText(cfg))...)
	}

	par := mapAt(v, "llmisvc", "parallelism")
	tensor, _ := number(par["tensor"])
	var l Launch
	if on(v, "llmisvc", "multiNode") {
		data, _ := number(par["data"])
		dataLocal, _ := number(par["dataLocal"])
		rpc, _ := number(par["dataRPCPort"])
		data, dataLocal, rpc = orOne(data), orOne(dataLocal), orDefault(rpc, 5555)
		servers := str(v, "llmisvc", "env", "VLLM_API_SERVER_COUNT")
		if servers == "" {
			servers = "8"
		}
		dpArgs := func(start int64) []vllmflags.Arg {
			var out []vllmflags.Arg
			if truthy(par["expert"]) {
				out = append(out, kserve("KServe (llmisvc.parallelism.expert)", "--enable-expert-parallel")...)
			}
			if tensor > 0 {
				out = append(out, kserve("KServe (llmisvc.parallelism.tensor)", "--tensor-parallel-size", itoa(tensor))...)
			}
			return append(out, kserve("KServe (llmisvc.parallelism)",
				"--data-parallel-size", itoa(data), "--data-parallel-size-local", itoa(dataLocal),
				"--data-parallel-address", "${DP_ADDRESS}", "--data-parallel-rpc-port", itoa(rpc),
				"--data-parallel-start-rank", itoa(start))...)
		}
		l.Leader = append(append(append(cloneArgs(head), kserve("KServe", "--api-server-count", servers)...), dpArgs(0)...), tail...)
		l.Worker = append(append(append(cloneArgs(head), dpArgs(dataLocal)...), kserve("KServe", "--headless")...), tail...)
	} else {
		l.Leader = append(cloneArgs(head), tail...)
		if tensor > 0 {
			l.Leader = append(l.Leader, kserve("KServe (llmisvc.parallelism.tensor)", "--tensor-parallel-size", itoa(tensor))...)
		}
	}

	chart := chartArgs(v, user)
	l.Leader = append(l.Leader, chart...)
	if l.Worker != nil {
		l.Worker = append(l.Worker, chart...)
	}
	return l
}

// chartArgs are the container's arguments as the chart writes them
// (templates/_podspec.tpl): data parallel over ports, the cluster's flags,
// the recipe's, the length, LMCache's connector.
func chartArgs(v map[string]any, user []vllmflags.Arg) []vllmflags.Arg {
	chart := func(why string, texts ...string) []vllmflags.Arg {
		var out []vllmflags.Arg
		for _, t := range texts {
			out = append(out, vllmflags.Arg{Text: t, Source: "chart (" + why + ")", Fixed: true})
		}
		return out
	}
	var out []vllmflags.Arg
	dp := mapAt(v, "llmisvc", "dataParallelExternalLB")
	if ranks, _ := number(dp["ranks"]); ranks > 1 {
		sup, _ := number(dp["supervisorPort"])
		out = append(out, chart("llmisvc.dataParallelExternalLB",
			"--data-parallel-size="+itoa(ranks), "--data-parallel-size-local="+itoa(ranks),
			"--data-parallel-multi-port-external-lb", "--data-parallel-supervisor-port="+itoa(orDefault(sup, 8208)))...)
	}
	out = append(out, user...)
	if n := scalar(mapAt(v, "model")["maxModelLen"]); n != "" {
		out = append(out, chart("model.maxModelLen", "--max-model-len="+n)...)
	}
	if on(v, "lmcache", "enabled") {
		extra := map[string]any{
			"lmcache.mp.host": fmt.Sprintf("tcp://%s.%s.svc.cluster.local", str(v, "lmcache", "name"), str(v, "namespace")),
			"lmcache.mp.port": scalar(mapAt(v, "lmcache", "server")["port"]),
		}
		if on(v, "lmcache", "isolatedIPC") {
			extra["lmcache.mp.isolated_ipc"] = true
		}
		cfg := map[string]any{"kv_connector": "LMCacheMPConnector", "kv_connector_module_path": "lmcache.integration.vllm.lmcache_mp_connector",
			"kv_role": "kv_both", "kv_connector_extra_config": extra}
		out = append(out, chart("lmcache.enabled", "--kv-transfer-config", jsonText(cfg))...)
	}
	return out
}

// argsOf is llmisvc.args or llmisvc.extraArgs as vLLM gets them: KServe's
// bash eval removes the single quotes of --option='value'.
func argsOf(v map[string]any, key string) []vllmflags.Arg {
	list, _ := mapAt(v, "llmisvc")[key].([]any)
	out := make([]vllmflags.Arg, 0, len(list))
	for _, a := range list {
		t := scalar(a)
		if quotedArg.MatchString(t) {
			k, val, _ := strings.Cut(t, "=")
			t = k + "=" + strings.Trim(val, "'")
		}
		out = append(out, vllmflags.Arg{Text: t, Source: "llmisvc." + key})
	}
	return out
}

func hasKVTransfer(args []vllmflags.Arg) bool {
	for _, a := range args {
		if kvTransferFlag.MatchString(a.Text) {
			return true
		}
	}
	return false
}

// FlagCheck is the hub's check of a model's vLLM command on a cluster.
type FlagCheck struct {
	// Image is llmisvc's image (images.vllm), "" for KServe's default.
	Image string `json:"image"`
	// Version is the vLLM flag list checked against. Exact says it is the
	// image's own; otherwise Note says why not, and every finding is only a
	// warning.
	Version string `json:"vllm"`
	Exact   bool   `json:"exact"`
	Note    string `json:"note,omitempty"`
	// Command is the leader's command (the only one off multi-node),
	// Worker the workers'.
	Command  []vllmflags.Arg     `json:"command"`
	Worker   []vllmflags.Arg     `json:"worker,omitempty"`
	Problems []vllmflags.Problem `json:"problems"`
}

// Errors are the findings that refuse a save.
func (f FlagCheck) Errors() []vllmflags.Problem { return vllmflags.Errors(f.Problems) }

// CheckFlags checks the vLLM command a model's merged values make: what vLLM
// refuses (vllmflags) and the hub's own rules (the GPUs a pod asks for, the
// flags the hub's prices need).
func CheckFlags(v map[string]any) FlagCheck {
	image := str(v, "images", "vllm")
	cat, exact, why := vllmflags.ForImage(image)
	fc := FlagCheck{Image: image, Exact: exact, Note: why, Problems: []vllmflags.Problem{}}
	if cat == nil {
		return fc
	}
	fc.Version = cat.Version
	l := LaunchOf(v, cat.Version)
	fc.Command, fc.Worker = l.Leader, l.Worker
	cmd, probs := cat.Check(l.Leader)
	if l.Worker != nil {
		_, more := cat.Check(l.Worker)
		probs = append(probs, more...)
	}
	probs = append(probs, shapeProblems(v, cmd)...)
	probs = append(probs, billingProblems(cmd)...)
	seen := map[vllmflags.Problem]bool{}
	for _, p := range probs {
		if !exact {
			p.Level = "warning"
		}
		if !seen[p] {
			seen[p] = true
			fc.Problems = append(fc.Problems, p)
		}
	}
	return fc
}

// shapeProblems compares the GPUs a pod asks for with the GPUs vLLM starts
// a process on: tensor × pipeline × prefill-context parallel, times the
// data-parallel ranks the pod runs.
func shapeProblems(v map[string]any, cmd *vllmflags.Command) []vllmflags.Problem {
	var out []vllmflags.Problem
	hub := func(level, format string, args ...any) {
		out = append(out, vllmflags.Problem{Level: level, Origin: "hub", Message: fmt.Sprintf(format, args...)})
	}
	if on(v, "llmisvc", "multiNode") {
		par := mapAt(v, "llmisvc", "parallelism")
		data, _ := number(par["data"])
		local, _ := number(par["dataLocal"])
		if data > 0 && local > 0 && data%local != 0 {
			hub("error", "llmisvc.parallelism.data (%d) must be a multiple of dataLocal (%d): KServe runs data/dataLocal pods per replica", data, local)
		}
	}
	res := mapAt(v, "llmisvc", "resources")
	gpus, kind := int64(0), ""
	for _, k := range []string{"nvidia.com/gpu", "amd.com/gpu"} {
		for _, part := range []string{"limits", "requests"} {
			if n, ok := number(mapAt(res, part)[k]); ok && gpus == 0 {
				gpus, kind = n, k
			}
		}
	}
	if gpus == 0 {
		return out
	}
	tp, pp := cmd.IntOr("tensor_parallel_size", 1), cmd.IntOr("pipeline_parallel_size", 1)
	pcp, dp := cmd.IntOr("prefill_context_parallel_size", 1), cmd.IntOr("data_parallel_size", 1)
	local := dp
	if n, ok := cmd.Int("data_parallel_size_local"); ok {
		local = n
	}
	if cmd.On("data_parallel_external_lb") || cmd.IntOr("data_parallel_rank", -1) >= 0 {
		local = 1
	}
	need := tp * pp * pcp * local
	shape := fmt.Sprintf("tensor %d × pipeline %d × prefill context %d × data-parallel ranks in the pod %d", tp, pp, pcp, local)
	switch {
	case need > gpus:
		hub("error", "vLLM needs %d GPUs per pod (%s), but the pod asks for %d %s", need, shape, gpus, kind)
	case need < gpus:
		hub("warning", "the pod asks for %d %s, and vLLM uses %d (%s)", gpus, kind, need, shape)
	}
	return out
}

// billingProblems warns of flags off that the hub's prices count on.
func billingProblems(cmd *vllmflags.Command) []vllmflags.Problem {
	var out []vllmflags.Problem
	if cmd.Catalog.FlagOf("enable_prompt_tokens_details") != nil && !cmd.On("enable_prompt_tokens_details") {
		out = append(out, vllmflags.Problem{Level: "warning", Origin: "hub", Flag: "--enable-prompt-tokens-details",
			Message: "--enable-prompt-tokens-details is off: answers carry no cached-token count, so cached input is priced as input"})
	}
	if cmd.Catalog.FlagOf("enable_force_include_usage") != nil && !cmd.On("enable_force_include_usage") {
		out = append(out, vllmflags.Problem{Level: "warning", Origin: "hub", Flag: "--enable-force-include-usage",
			Message: "--enable-force-include-usage is off: a stream that does not ask for usage ends without it, and its tokens are not counted"})
	}
	return out
}

func mapAt(v map[string]any, path ...string) map[string]any {
	for _, k := range path {
		next, _ := v[k].(map[string]any)
		v = next
	}
	return v
}

func on(v map[string]any, path ...string) bool {
	last := path[len(path)-1]
	return truthy(mapAt(v, path[:len(path)-1]...)[last])
}

func truthy(x any) bool {
	switch t := x.(type) {
	case bool:
		return t
	case string:
		return t == "true"
	}
	return false
}

// number is a YAML or JSON number, or a string holding one.
func number(x any) (int64, bool) {
	switch t := x.(type) {
	case int:
		return int64(t), true
	case int64:
		return t, true
	case uint64:
		return int64(t), true
	case float64:
		return int64(t), t == float64(int64(t))
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// scalar is a value as Helm's toString writes it.
func scalar(x any) string {
	switch t := x.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return fmt.Sprint(x)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func orOne(n int64) int64 { return orDefault(n, 1) }

func orDefault(n, d int64) int64 {
	if n <= 0 {
		return d
	}
	return n
}

func cloneArgs(a []vllmflags.Arg) []vllmflags.Arg { return append([]vllmflags.Arg(nil), a...) }

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var quantityPattern = regexp.MustCompile(`^(\d+)(Ki|Mi|Gi|Ti|K|M|G|T)?$`)

// quantity is a Kubernetes quantity (64Gi) in bytes, as KServe's
// kvTransferConfig writes cpu_bytes_to_use.
func quantity(x any) int64 {
	m := quantityPattern.FindStringSubmatch(scalar(x))
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	mult := map[string]int64{"": 1, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40}
	return n * mult[m[2]]
}

// ClusterCheck is a model's flag check on one cluster; Cluster is "" for a
// recipe that no cluster runs yet.
type ClusterCheck struct {
	Cluster string `json:"cluster"`
	Model   string `json:"model"`
	FlagCheck
}

// RecipeChecks checks a recipe (at path) on every cluster that runs the
// model and on the clusters in also, with the recipe changed to recipe. A
// recipe no cluster runs is checked alone: a recipe that leaves the shape
// to each cluster is never run alone.
func (s Snapshot) RecipeChecks(model, path string, recipe map[string]any, also ...Cluster) []ClusterCheck {
	with := s.With(path, recipe)
	var out []ClusterCheck
	for _, d := range s.Deployments {
		if d.Model == model {
			out = append(out, ClusterCheck{Cluster: d.Cluster, Model: model, FlagCheck: CheckFlags(with.merged(Layers(s.clusterNamed(d.Cluster), model)))})
		}
	}
	for _, c := range also {
		out = append(out, ClusterCheck{Cluster: c.Name, Model: model, FlagCheck: CheckFlags(with.merged(Layers(c, model)))})
	}
	if len(out) == 0 {
		out = append(out, ClusterCheck{Model: model, FlagCheck: CheckFlags(recipe)})
	}
	return out
}

func (s Snapshot) clusterNamed(name string) Cluster {
	for _, c := range s.Clusters {
		if c.Name == name {
			return c
		}
	}
	return Cluster{Name: name}
}
