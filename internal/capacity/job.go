package capacity

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

//go:embed runner.py
var runner string

// Labels on the Job and its pod.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	RunLabel       = "aigw-ui.io/capacity-run"
)

// JobSpec is one measurement as the cluster runs it.
type JobSpec struct {
	RunID     string
	Name      string
	Namespace string
	Image     string
	// URLs are the instance, for example http://10.0.0.5:8000, with one
	// address per data-parallel rank of a pod that serves on several ports;
	// the load is sent to them in turn. Model is the name it serves the
	// model under. A prefill/decode deployment has Peer instead: it is
	// measured through its scheduler on the gateway's peer listener, and the
	// Job runs in the gateway's namespace.
	URLs     []string
	Model    string
	Peer     *Peer
	Settings Settings
	// MetricsURLs are the servers whose vLLM counters the measurement adds
	// up: the pod measured, each of its ranks, or every decode pod. Empty
	// is URLs.
	MetricsURLs []string
}

// Peer is where a Job reaches the gateway's peer listener: the address of
// the site's own gateway, the server name the listener answers to, and the
// secret and ConfigMap with the site's client certificate and the CA.
type Peer struct {
	Address      string
	SNI          string
	ClientSecret string
	CAConfigMap  string
}

// Job returns the Job that runs a measurement. It never restarts: a second
// try would measure a warm cache and a half-done step as if they were one.
func Job(spec JobSpec) (*unstructured.Unstructured, error) {
	cfg := map[string]any{
		"urls":              spec.URLs,
		"model":             spec.Model,
		"settings":          spec.Settings,
		"grace_seconds":     stepGrace,
		"agentic":           Agentic(spec.Settings),
		"metrics_urls":      spec.MetricsURLs,
		"wait_idle_seconds": idleWait,
		"settle_seconds":    settleWait,
		"quiet_seconds":     quietSpell,
	}
	mounts := []any{map[string]any{"name": "work", "mountPath": "/tmp"}}
	volumes := []any{map[string]any{"name": "work", "emptyDir": map[string]any{}}}
	if p := spec.Peer; p != nil {
		cfg["peer"] = map[string]string{"address": p.Address, "sni": p.SNI,
			"cert": "/peer/tls.crt", "key": "/peer/tls.key", "ca": "/peer-ca/ca.crt"}
		mounts = append(mounts,
			map[string]any{"name": "peer", "mountPath": "/peer", "readOnly": true},
			map[string]any{"name": "peer-ca", "mountPath": "/peer-ca", "readOnly": true})
		volumes = append(volumes,
			map[string]any{"name": "peer", "secret": map[string]any{"secretName": p.ClientSecret}},
			map[string]any{"name": "peer-ca", "configMap": map[string]any{"name": p.CAConfigMap}})
	}
	config, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	labels := map[string]any{ManagedByLabel: "aigw-ui", RunLabel: spec.RunID}
	// Through the peer listener the Job also runs the proxy, which passes
	// every token on: with AIPerf on two CPUs it adds to the time between
	// tokens at a few hundred streams, on four it does not.
	cpus := "2"
	if spec.Peer != nil {
		cpus = "4"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      spec.Name,
			"namespace": spec.Namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"backoffLimit":            int64(0),
			"activeDeadlineSeconds":   int64(spec.Settings.Duration()),
			"ttlSecondsAfterFinished": int64(7 * 24 * 3600),
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"restartPolicy":                "Never",
					"automountServiceAccountToken": false,
					"securityContext": map[string]any{
						"runAsNonRoot":   true,
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"containers": []any{map[string]any{
						"name":    "aiperf",
						"image":   spec.Image,
						"command": []any{"python3", "-c", runner},
						"env": []any{
							map[string]any{"name": "AIGW_CAPACITY", "value": string(config)},
							// Nothing is downloaded: the built-in tokenizer
							// ships with the image.
							map[string]any{"name": "HF_HUB_OFFLINE", "value": "1"},
							map[string]any{"name": "HOME", "value": "/tmp"},
						},
						"workingDir": "/tmp",
						"resources": map[string]any{
							"requests": map[string]any{"cpu": cpus, "memory": "4Gi"},
							"limits":   map[string]any{"memory": "8Gi"},
						},
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false,
							"capabilities":             map[string]any{"drop": []any{"ALL"}},
						},
						"volumeMounts": mounts,
					}},
					"volumes": volumes,
				},
			},
		},
	}}, nil
}

// Agentic returns AIPerf's coding-agent session settings for a measurement.
// They are AIPerf's own defaults, in which a prompt grows to 167,000 tokens
// and is about 100,000 on average, with the profile's shared system prompt
// and pauses (none by default). A smaller longest prompt scales every part of
// a session by the same factor, so the share of a prompt that other sessions
// share stays the same.
func Agentic(s Settings) map[string]any {
	f := math.Min(float64(s.MaxPromptTokens)/agenticMaxPrompt, 1)
	tokens := func(n float64) float64 { return math.Max(math.Round(n*f), 1) }
	answer := math.Min(1000, math.Round(float64(s.MaxOutputTokens)*2/3))
	block := math.Min(512, math.Max(64, math.Round(512*f/16)*16))
	shared := 32000.0
	if s.SharedPrefixTokens > 0 {
		shared = float64(s.SharedPrefixTokens)
	}
	return map[string]any{
		"new_tokens_per_turn": map[string]any{"mean": tokens(6000), "median": tokens(6000), "max": tokens(10000), "bias": 1.1},
		"generation_length":   map[string]any{"mean": answer, "median": answer, "max": float64(s.MaxOutputTokens)},
		// No pause between turns: AIPerf waits it out with the request slot
		// held, so "1 in flight" would be one session idle most of the time.
		// What an instance serves at a load does not depend on the pauses.
		"inter_turn_delay": map[string]any{
			"agentic_fraction": 0.7,
			"agentic_delay":    map[string]any{"mean": 2500, "median": 1800, "max": 10000},
			"human_delay":      map[string]any{"mean": 40000, "median": 25000, "max": 120000},
			"max":              s.PauseMillis,
		},
		"reset":             map[string]any{"base_probability": 0.01, "context_scaling": 1.5},
		"max_prompt_tokens": s.MaxPromptTokens,
		"block_size":        int(block),
		"cache": map[string]any{
			"layer1_tokens":   int(tokens(shared)),
			"layer1_5_tokens": 0,
			"layer2":          map[string]any{"mean": tokens(15000), "median": tokens(5000), "max": tokens(30000)},
			"layer1_5_groups": map[string]any{"num_groups": 1, "zipf_alpha": 0.6},
		},
	}
}

// Dataset is what the made-up sessions hold.
type Dataset struct {
	Sessions   int     `json:"sessions"`
	Requests   int     `json:"requests"`
	PromptMean float64 `json:"prompt_mean"`
	PromptMax  int64   `json:"prompt_max"`
}

// Fit is what a Job prints when the instance takes shorter requests than
// its settings ask for: the instance's length, and the settings and sessions
// it fitted to it by the rule of Profile.Settings.
type Fit struct {
	MaxModelLen int            `json:"max_model_len"`
	Settings    Settings       `json:"settings"`
	Agentic     map[string]any `json:"agentic"`
}

// Log is what the Job has printed so far.
type Log struct {
	// Fit is set when the Job fitted the settings to the instance: the run
	// is measured with Fit.Settings, not with the settings it was given.
	Fit     *Fit
	Dataset *Dataset
	Steps   []Step
	// Done is set when the Job printed its last line. Error when it gave
	// up before the first step.
	Done  bool
	Error string
}

// ParseLog reads the Job's lines. Lines that do not start with "AIGW-" are
// AIPerf's and are left alone; a line that cannot be read is skipped.
func ParseLog(text string) Log {
	var l Log
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		kind, body, ok := strings.Cut(sc.Text(), " ")
		if !ok || !strings.HasPrefix(kind, "AIGW-") {
			continue
		}
		switch kind {
		case "AIGW-FIT":
			var f Fit
			if json.Unmarshal([]byte(body), &f) == nil {
				l.Fit = &f
			}
		case "AIGW-DATASET":
			var d Dataset
			if json.Unmarshal([]byte(body), &d) == nil {
				l.Dataset = &d
			}
		case "AIGW-STEP":
			var s Step
			if json.Unmarshal([]byte(body), &s) == nil {
				l.Steps = append(l.Steps, s)
			}
		case "AIGW-ERROR":
			var e struct {
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(body), &e) == nil {
				l.Error = e.Error
			}
		case "AIGW-DONE":
			l.Done = true
		}
	}
	return l
}

// JobName returns the name of a run's Job.
func JobName(runID string) string {
	return fmt.Sprintf("aigw-capacity-%s", strings.ReplaceAll(runID, "-", "")[:12])
}
