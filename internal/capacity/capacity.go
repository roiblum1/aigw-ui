// Package capacity measures what one instance of a model serves at the
// latency an operator promises, and turns that into what a model can be
// given: tokens or dollars per day, the price that covers its cost, and how
// far the budgets handed out exceed it.
//
// The measurement is AIPerf in a Job on the cluster, aimed at one pod of the
// model. It replays made-up coding-agent sessions at a growing number of
// requests in flight. The Job makes up its prompts itself and prints one
// line of numbers per step; nothing of a prompt reaches the server.
package capacity

import (
	"fmt"
	"math"
)

// Settings are what an operator chooses for one measurement.
type Settings struct {
	// Concurrency lists the load steps: requests in flight, ascending.
	Concurrency []int `json:"concurrency"`
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
	// The latency an operator promises, as the 95th percentile of a step:
	// time to the first token, and time between two tokens after it.
	TTFTMillis float64 `json:"ttft_p95_ms"`
	ITLMillis  float64 `json:"itl_p95_ms"`
}

// AIPerf's coding-agent sessions grow to at most this many prompt tokens.
const agenticMaxPrompt = 167000

// Defaults returns the settings for a model whose deployments accept at most
// maxModelLen tokens, or an unknown length when it is 0.
func Defaults(maxModelLen int) Settings {
	s := Settings{
		Concurrency:     []int{1, 2, 4, 8, 16, 32, 64},
		MinRequests:     60,
		StepSeconds:     1200,
		Sessions:        200,
		MaxPromptTokens: agenticMaxPrompt,
		MaxOutputTokens: 1500,
		TTFTMillis:      5000,
		ITLMillis:       50,
	}
	if maxModelLen > 0 && maxModelLen-s.MaxOutputTokens < s.MaxPromptTokens {
		s.MaxPromptTokens = max(maxModelLen-s.MaxOutputTokens, 0)
		if s.MaxPromptTokens < 2*s.MaxOutputTokens {
			s.MaxOutputTokens = max(maxModelLen/4, 16)
			s.MaxPromptTokens = maxModelLen - s.MaxOutputTokens
		}
	}
	return s
}

// Validate refuses settings that cannot give a meaningful answer, or that
// would keep a GPU busy for an unreasonable time.
func (s Settings) Validate() error {
	switch {
	case len(s.Concurrency) == 0 || len(s.Concurrency) > 12:
		return fmt.Errorf("concurrency must list from 1 to 12 steps")
	case !ascending(s.Concurrency):
		return fmt.Errorf("concurrency must go up from one step to the next")
	case s.Concurrency[0] < 1 || s.Concurrency[len(s.Concurrency)-1] > 1024:
		return fmt.Errorf("concurrency must be from 1 to 1024")
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

func ascending(steps []int) bool {
	for i := 1; i < len(steps); i++ {
		if steps[i] <= steps[i-1] {
			return false
		}
	}
	return true
}

// Requests is how many requests a step at a load sends: at least
// MinRequests, and four per request in flight.
func (s Settings) Requests(concurrency int) int {
	return max(s.MinRequests, 4*concurrency)
}

// Duration is the longest the measurement may take, in seconds: every step,
// the time to let its last requests finish, and the time to make up the
// sessions and start.
func (s Settings) Duration() int {
	return len(s.Concurrency)*(s.StepSeconds+stepGrace+60) + 900
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

// maxErrorShare is the part of a step's requests that may fail for the step
// to count.
const maxErrorShare = 0.01

// Check says whether a step meets the settings, and if not, why.
func (s Step) Check(set Settings) (bool, string) {
	total := s.Requests + s.Errors
	switch {
	case s.Error != "":
		return false, s.Error
	case s.Requests == 0:
		return false, "no request was answered"
	case float64(s.Errors) > maxErrorShare*float64(total):
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
}

// Analyze finds the highest load the instance serves at the promised
// latency. It stops at the first step that misses: a step above it that
// passes again is noise, not capacity.
func Analyze(set Settings, steps []Step) Result {
	r := Result{Steps: make([]StepView, 0, len(steps))}
	missed := false
	for i := range steps {
		s := &steps[i]
		ok, why := s.Check(set)
		r.Steps = append(r.Steps, StepView{Concurrency: s.Concurrency, Met: ok, Reason: why,
			FewSamples:     s.Error == "" && s.Requests < set.MinRequests,
			InputPerSecond: s.InputPerSecond(), OutputPerSecond: s.OutputPerSecond(), CachedShare: s.CachedShare()})
		switch {
		case missed:
		case !ok:
			r.Limit = fmt.Sprintf("at %d in flight: %s", s.Concurrency, why)
			missed = true
		default:
			r.Best = s
		}
	}
	if missed {
		return r
	}
	if r.Best != nil {
		r.Limit = fmt.Sprintf("every step met the targets; the instance may serve more than %d in flight", r.Best.Concurrency)
	}
	return r
}
