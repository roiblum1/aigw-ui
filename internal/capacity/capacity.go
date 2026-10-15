// Package capacity measures what one instance of a model serves at the
// latency an operator promises, and turns that into what a model can be
// given: tokens or dollars per day, the price that covers its cost, and how
// far the budgets handed out exceed it.
//
// The measurement is AIPerf in a Job on the cluster, aimed at one instance of
// one LLMInferenceService. It replays made-up coding-agent sessions, doubling
// the requests in flight until a step misses the promised latency, then
// halving the gap. The Job makes up its prompts itself and prints one line of
// numbers per step; nothing of a prompt reaches the server.
package capacity

import (
	"fmt"
	"math"
)

// Settings are what one measurement executes: the active benchmark profile
// as it applies to one deployment (Profile.Settings).
type Settings struct {
	// Concurrency lists the load steps of a 0.13–0.14 measurement, which
	// went up a fixed list. A sweep leaves it empty.
	Concurrency []int `json:"concurrency,omitempty"`
	// The sweep: doubling from FirstConcurrency up to MaxConcurrency until a
	// step misses, then BisectSteps halvings of the gap.
	FirstConcurrency int `json:"first_concurrency,omitempty"`
	BisectSteps      int `json:"bisect_steps,omitempty"`
	MaxConcurrency   int `json:"max_concurrency,omitempty"`
	// MinRequests is how many requests a step sends at least, so that its
	// 95th percentile rests on enough of them; more at a higher load.
	MinRequests int `json:"min_requests"`
	// StepSeconds is the longest a step sends requests. A step that reaches
	// it before MinRequests are answered is marked as having few samples.
	StepSeconds int `json:"step_seconds"`
	// Sessions is how many coding-agent sessions are made up. Every step
	// replays them from the start.
	Sessions int `json:"sessions"`
	// MaxPromptTokens is the longest prompt a session grows to. AIPerf's
	// coding-agent sessions stop at 167,000 tokens; a smaller value scales
	// every part of a session down by the same factor.
	MaxPromptTokens int `json:"max_prompt_tokens"`
	// MaxOutputTokens is the longest answer.
	MaxOutputTokens int `json:"max_output_tokens"`
	// SharedPrefixTokens is the system prompt every session shares, before
	// scaling; 0 is AIPerf's 32,000. PauseMillis is the longest pause between
	// turns; 0 is none.
	SharedPrefixTokens int `json:"shared_prefix_tokens,omitempty"`
	PauseMillis        int `json:"pause_ms,omitempty"`
	// The latency an operator promises, as the 95th percentile of a step:
	// time to the first token, and time between two tokens after it.
	TTFTMillis float64 `json:"ttft_p95_ms"`
	ITLMillis  float64 `json:"itl_p95_ms"`
	// MaxErrorShare is the part of a step's requests that may fail; 0 is
	// 1%.
	MaxErrorShare float64 `json:"max_error_share,omitempty"`
	// Mode is ModeDrained or ModeLive; "" for a 0.13–0.14 measurement, which
	// was neither.
	Mode string `json:"mode,omitempty"`
}

// AIPerf's coding-agent sessions grow to at most this many prompt tokens.
const agenticMaxPrompt = 167000

// Validate refuses settings that cannot give a meaningful answer, or that
// would keep a GPU busy for an unreasonable time.
func (s Settings) Validate() error {
	switch {
	case s.Mode != ModeDrained && s.Mode != ModeLive:
		return fmt.Errorf("mode must be %q or %q", ModeDrained, ModeLive)
	case len(s.Concurrency) > 0:
		return fmt.Errorf("a measurement sweeps the load; a list of steps is no longer taken")
	case s.FirstConcurrency < 1 || s.MaxConcurrency < s.FirstConcurrency || s.MaxConcurrency > 1024:
		return fmt.Errorf("the load must sweep from 1 to 1024 requests in flight")
	case s.BisectSteps < 0 || s.BisectSteps > 5:
		return fmt.Errorf("bisect_steps must be from 0 to 5")
	case s.MaxErrorShare < 0 || s.MaxErrorShare > 0.5:
		return fmt.Errorf("max_error_share must be from 0 to 0.5")
	case s.MinRequests < 10 || s.MinRequests > 2000:
		return fmt.Errorf("min_requests must be from 10 to 2,000")
	case s.StepSeconds < 60 || s.StepSeconds > 3600:
		return fmt.Errorf("step_seconds must be from 60 to 3600")
	case s.Sessions < 10 || s.Sessions > 5000:
		return fmt.Errorf("sessions must be from 10 to 5000")
	case s.MaxPromptTokens < 1000 || s.MaxPromptTokens > 1_000_000:
		return fmt.Errorf("max_prompt_tokens must be from 1,000 to 1,000,000")
	case s.MaxOutputTokens < 16 || s.MaxOutputTokens > 32768:
		return fmt.Errorf("max_output_tokens must be from 16 to 32,768")
	case !(s.TTFTMillis > 0) || s.TTFTMillis > 600_000:
		return fmt.Errorf("ttft_p95_ms must be more than 0 and at most 600,000")
	case !(s.ITLMillis > 0) || s.ITLMillis > 10_000:
		return fmt.Errorf("itl_p95_ms must be more than 0 and at most 10,000")
	}
	return nil
}

// Requests is how many requests a step at a load sends: at least
// MinRequests, and four per request in flight.
func (s Settings) Requests(concurrency int) int {
	return max(s.MinRequests, 4*concurrency)
}

// Duration is the longest the measurement may take, in seconds: every step,
// the time to let its last requests finish, the wait for an instance with
// no requests in flight, and the time to make up the sessions and start.
func (s Settings) Duration() int {
	return s.Steps()*(s.StepSeconds+stepGrace+60) + idleWait + 900
}

// Steps is the most steps the measurement runs: the doublings from
// FirstConcurrency to MaxConcurrency, then the bisections.
func (s Settings) Steps() int {
	if len(s.Concurrency) > 0 {
		return len(s.Concurrency)
	}
	n := 1
	for c := max(s.FirstConcurrency, 1); c < s.MaxConcurrency; c *= 2 {
		n++
	}
	return n + s.BisectSteps
}

// idleWait is how long a drained measurement waits for its instance to have
// no request in flight before it gives up: other traffic still reaches it.
const idleWait = 600

// errorShare is the part of a step's requests that may fail.
func (s Settings) errorShare() float64 {
	if s.MaxErrorShare > 0 {
		return s.MaxErrorShare
	}
	return maxErrorShare
}

// stepGrace is how long a step waits for the requests still in flight when
// it stops sending. A long prompt with a long answer needs minutes.
const stepGrace = 300

// Step is what one load step measured. The Job prints it as one line.
type Step struct {
	Concurrency int `json:"concurrency"`
	// Requests answered and requests that failed.
	Requests int `json:"requests"`
	Errors   int `json:"errors"`
	// DurationSeconds is how long the step sent requests.
	DurationSeconds float64 `json:"duration_s"`
	// Tokens of the answered requests, as the model server reported them.
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	InputMean    float64 `json:"isl_mean"`
	OutputMean   float64 `json:"osl_mean"`
	TTFTP50      float64 `json:"ttft_p50_ms"`
	TTFTP95      float64 `json:"ttft_p95_ms"`
	ITLP50       float64 `json:"itl_p50_ms"`
	ITLP95       float64 `json:"itl_p95_ms"`
	// What the instance's own counters grew by during the step: prompt
	// tokens, and the part of them it took from its cache. Nil when the
	// instance does not publish them. They include any other traffic the
	// instance served at the same time.
	ServerPromptTokens *float64 `json:"server_prompt_tokens"`
	ServerCachedTokens *float64 `json:"server_cached_tokens"`
	// ServerOutputTokens is what its counter of generated tokens grew by,
	// and ServerSeconds the time between the two readings of the counters.
	ServerOutputTokens *float64 `json:"server_output_tokens,omitempty"`
	ServerSeconds      float64  `json:"server_seconds,omitempty"`
	// Error is set when the step could not run at all.
	Error string `json:"error,omitempty"`
}

// InputPerSecond and OutputPerSecond are the tokens the step served per
// second.
func (s Step) InputPerSecond() float64  { return perSecond(s.InputTokens, s.DurationSeconds) }
func (s Step) OutputPerSecond() float64 { return perSecond(s.OutputTokens, s.DurationSeconds) }

func perSecond(n int64, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return float64(n) / seconds
}

// CachedShare is the part of the prompt tokens the instance took from its
// cache, or nil when it does not say.
func (s Step) CachedShare() *float64 {
	if s.ServerPromptTokens == nil || s.ServerCachedTokens == nil || *s.ServerPromptTokens <= 0 {
		return nil
	}
	share := math.Min(math.Max(*s.ServerCachedTokens / *s.ServerPromptTokens, 0), 1)
	return &share
}

// ForeignShare is the part of the prompt tokens the instance counted that the
// measurement did not send: other traffic, or nil when the instance does not
// publish its counter.
func (s Step) ForeignShare() *float64 {
	if s.ServerPromptTokens == nil || *s.ServerPromptTokens <= 0 {
		return nil
	}
	share := math.Min(math.Max(1 - float64(s.InputTokens) / *s.ServerPromptTokens, 0), 1)
	return &share
}

// maxErrorShare is the part of a step's requests that may fail for the step
// to count, unless the settings say otherwise.
const maxErrorShare = 0.01

// Check says whether a step meets the settings, and if not, why. A drained
// step with more than MaxForeignShare of other traffic does not count: what
// it measured was not the instance alone.
func (s Step) Check(set Settings) (bool, string) {
	total := s.Requests + s.Errors
	switch {
	case s.Error != "":
		return false, s.Error
	case s.Requests == 0:
		return false, "no request was answered"
	case set.Mode == ModeDrained && s.ForeignShare() != nil && *s.ForeignShare() > MaxForeignShare:
		return false, fmt.Sprintf("contaminated: %.0f%% of the prompt tokens the instance counted were not sent by the measurement; something still sends to it, such as a client of the site's own route",
			*s.ForeignShare()*100)
	case float64(s.Errors) > set.errorShare()*float64(total):
		return false, fmt.Sprintf("%d of %d requests failed", s.Errors, total)
	case s.TTFTP95 > set.TTFTMillis:
		return false, fmt.Sprintf("time to first token p95 %.0f ms is over %.0f ms", s.TTFTP95, set.TTFTMillis)
	case s.ITLP95 > set.ITLMillis:
		return false, fmt.Sprintf("time between tokens p95 %.1f ms is over %.1f ms", s.ITLP95, set.ITLMillis)
	}
	return true, ""
}

// Result is what a measurement found.
type Result struct {
	// Best is the highest step that met the settings, with every step below
	// it meeting them too. Nil when the first step already missed them.
	Best *Step `json:"best"`
	// Limit says why the next step did not count, or that the last step
	// still met the settings and the instance may serve more.
	Limit string `json:"limit"`
	// Steps are the steps as the page shows them.
	Steps []StepView `json:"steps"`
}

// StepView is a step with what follows from it.
type StepView struct {
	Concurrency int `json:"concurrency"`
	// Met says whether the step met the targets, Reason why not.
	Met    bool   `json:"met"`
	Reason string `json:"reason,omitempty"`
	// FewSamples is set when the step reached its longest time before
	// enough requests were answered: its percentiles rest on few of them.
	FewSamples      bool     `json:"few_samples"`
	InputPerSecond  float64  `json:"input_per_s"`
	OutputPerSecond float64  `json:"output_per_s"`
	CachedShare     *float64 `json:"cached_share"`
	// ForeignShare is the part of the instance's prompt tokens that other
	// traffic sent: a failure when drained, shown when live.
	ForeignShare *float64 `json:"foreign_share"`
}

// Analyze finds the highest load the instance serves at the promised
// latency: the highest step that met the targets below the lowest step that
// missed them. Steps come in the order they ran; with bisection that is not
// ascending. A step that passes above one that missed is noise, not
// capacity.
func Analyze(set Settings, steps []Step) Result {
	r := Result{Steps: make([]StepView, 0, len(steps))}
	lowestMiss, why := 0, ""
	met := make([]bool, len(steps))
	for i := range steps {
		s := &steps[i]
		ok, reason := s.Check(set)
		met[i] = ok
		r.Steps = append(r.Steps, StepView{Concurrency: s.Concurrency, Met: ok, Reason: reason,
			FewSamples:     s.Error == "" && s.Requests < set.MinRequests,
			InputPerSecond: s.InputPerSecond(), OutputPerSecond: s.OutputPerSecond(), CachedShare: s.CachedShare(),
			ForeignShare: s.ForeignShare()})
		if !ok && (lowestMiss == 0 || s.Concurrency < lowestMiss) {
			lowestMiss, why = s.Concurrency, reason
		}
	}
	for i := range steps {
		if met[i] && (lowestMiss == 0 || steps[i].Concurrency < lowestMiss) && (r.Best == nil || steps[i].Concurrency > r.Best.Concurrency) {
			r.Best = &steps[i]
		}
	}
	switch {
	case lowestMiss > 0:
		r.Limit = fmt.Sprintf("at %d in flight: %s", lowestMiss, why)
	case r.Best != nil:
		r.Limit = fmt.Sprintf("every step met the targets; the instance may serve more than %d in flight", r.Best.Concurrency)
	}
	return r
}
