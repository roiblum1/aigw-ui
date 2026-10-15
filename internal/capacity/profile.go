package capacity

import (
	"fmt"
	"time"
)

// Profile is the one benchmark every LLMInferenceService is measured with,
// so that their results compare. Editing it makes a new version; exactly
// one version is active, and a result counts only with the version it was
// measured with.
type Profile struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	Workload  Workload  `json:"workload"`
	SLO       SLO       `json:"slo"`
	Sweep     Sweep     `json:"sweep"`
	Note      string    `json:"note"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
}

// Workload is the traffic a measurement replays: AIPerf's coding-agent
// sessions.
type Workload struct {
	// MaxPromptTokens is the longest prompt a session grows to; at AIPerf's
	// 167,000 a prompt is about 100,000 tokens on average. A deployment that
	// takes less gets every part of a session scaled down by one factor.
	MaxPromptTokens int `json:"max_prompt_tokens"`
	MaxOutputTokens int `json:"max_output_tokens"`
	// Sessions is how many sessions are made up. Every step replays them.
	Sessions int `json:"sessions"`
	// SharedPrefixTokens is the system prompt every session shares.
	SharedPrefixTokens int `json:"shared_prefix_tokens"`
	// PauseMillis is the longest pause between two turns of a session; 0
	// replays them back to back.
	PauseMillis int `json:"pause_ms"`
}

// SLO is the latency promised to the tenants, as the 95th percentile of a
// step, and the share of requests that may fail.
type SLO struct {
	TTFTMillis    float64 `json:"ttft_p95_ms"`
	ITLMillis     float64 `json:"itl_p95_ms"`
	MaxErrorShare float64 `json:"max_error_share"`
}

// Sweep is how the load is raised: doubling from FirstConcurrency until a
// step misses the SLO, then BisectSteps halvings of the gap between the last
// step that met it and the first that did not.
type Sweep struct {
	FirstConcurrency int `json:"first_concurrency"`
	BisectSteps      int `json:"bisect_steps"`
	// StepSeconds is the longest a step sends requests, MinRequests how
	// many it sends at least.
	StepSeconds int `json:"step_seconds"`
	MinRequests int `json:"min_requests"`
	// MaxConcurrency stops the doubling: a model that meets the SLO there
	// is reported as serving at least that much.
	MaxConcurrency int `json:"max_concurrency"`
}

// DefaultProfile is the profile a hub starts with: the 0.13 defaults, with
// steps of at most five minutes.
func DefaultProfile() Profile {
	return Profile{
		Name:     "agentic-code",
		Version:  1,
		Workload: Workload{MaxPromptTokens: agenticMaxPrompt, MaxOutputTokens: 1500, Sessions: 200, SharedPrefixTokens: 32000},
		SLO:      SLO{TTFTMillis: 5000, ITLMillis: 50, MaxErrorShare: 0.01},
		Sweep:    Sweep{FirstConcurrency: 1, BisectSteps: 2, StepSeconds: 300, MinRequests: 60, MaxConcurrency: 1024},
		Active:   true,
	}
}

// Validate refuses a profile that cannot give a meaningful answer, or that
// would keep a GPU busy for an unreasonable time.
func (p Profile) Validate() error {
	w, slo, sw := p.Workload, p.SLO, p.Sweep
	switch {
	case p.Name == "" || len(p.Name) > 100:
		return fmt.Errorf("name must be from 1 to 100 characters")
	case w.MaxPromptTokens < 1000 || w.MaxPromptTokens > 1_000_000:
		return fmt.Errorf("max_prompt_tokens must be from 1,000 to 1,000,000")
	case w.MaxOutputTokens < 16 || w.MaxOutputTokens > 32768:
		return fmt.Errorf("max_output_tokens must be from 16 to 32,768")
	case w.Sessions < 10 || w.Sessions > 5000:
		return fmt.Errorf("sessions must be from 10 to 5000")
	case w.SharedPrefixTokens < 0 || w.SharedPrefixTokens > w.MaxPromptTokens/2:
		return fmt.Errorf("shared_prefix_tokens must be from 0 to half of max_prompt_tokens")
	case w.PauseMillis < 0 || w.PauseMillis > 600_000:
		return fmt.Errorf("pause_ms must be from 0 to 600,000")
	case !(slo.TTFTMillis > 0) || slo.TTFTMillis > 600_000:
		return fmt.Errorf("ttft_p95_ms must be more than 0 and at most 600,000")
	case !(slo.ITLMillis > 0) || slo.ITLMillis > 10_000:
		return fmt.Errorf("itl_p95_ms must be more than 0 and at most 10,000")
	case slo.MaxErrorShare < 0 || slo.MaxErrorShare > 0.5:
		return fmt.Errorf("max_error_share must be from 0 to 0.5")
	case sw.FirstConcurrency < 1 || sw.FirstConcurrency > 1024:
		return fmt.Errorf("first_concurrency must be from 1 to 1024")
	case sw.MaxConcurrency < sw.FirstConcurrency || sw.MaxConcurrency > 1024:
		return fmt.Errorf("max_concurrency must be from first_concurrency to 1024")
	case sw.BisectSteps < 0 || sw.BisectSteps > 5:
		return fmt.Errorf("bisect_steps must be from 0 to 5")
	case sw.StepSeconds < 60 || sw.StepSeconds > 3600:
		return fmt.Errorf("step_seconds must be from 60 to 3600")
	case sw.MinRequests < 10 || sw.MinRequests > 2000:
		return fmt.Errorf("min_requests must be from 10 to 2,000")
	case len(p.Note) > 500:
		return fmt.Errorf("note must be at most 500 characters")
	}
	return nil
}

// Settings are what one run of the profile executes against a deployment
// that takes at most maxModelLen tokens (0 when it does not say), in a mode.
// A deployment that takes less than the profile's longest prompt gets every
// part of a session scaled down by the same factor, as 0.13 did: one profile
// cannot hand a 100,000-token prompt to a model that takes 8,192.
func (p Profile) Settings(maxModelLen int, mode string) Settings {
	s := Settings{
		FirstConcurrency:   p.Sweep.FirstConcurrency,
		BisectSteps:        p.Sweep.BisectSteps,
		MaxConcurrency:     p.Sweep.MaxConcurrency,
		MinRequests:        p.Sweep.MinRequests,
		StepSeconds:        p.Sweep.StepSeconds,
		Sessions:           p.Workload.Sessions,
		MaxPromptTokens:    p.Workload.MaxPromptTokens,
		MaxOutputTokens:    p.Workload.MaxOutputTokens,
		SharedPrefixTokens: p.Workload.SharedPrefixTokens,
		PauseMillis:        p.Workload.PauseMillis,
		TTFTMillis:         p.SLO.TTFTMillis,
		ITLMillis:          p.SLO.ITLMillis,
		MaxErrorShare:      p.SLO.MaxErrorShare,
		Mode:               mode,
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

// Measuring modes.
const (
	// ModeDrained measures an instance with no user traffic: its site is
	// drained first. A step with other traffic fails.
	ModeDrained = "drained"
	// ModeLive measures an instance that serves users: the throughput is
	// its own counters, which include theirs.
	ModeLive = "live"
)

// MaxForeignShare is the most of a drained step's prompt tokens that may come
// from somewhere else than the measurement.
const MaxForeignShare = 0.05

// MaxResultAge is how long a result counts as fresh.
const MaxResultAge = 30 * 24 * time.Hour

// Why a result is stale.
const (
	StaleSpec    = "spec"    // measured before its spec hash changed
	StaleProfile = "profile" // measured with another version of the profile
	StaleAge     = "age"     // older than MaxResultAge
)

// Staleness says why a result measured with profileVersion at measuredAt is
// stale at now, or "" when it is fresh.
func Staleness(profileVersion, activeVersion int, measuredAt, now time.Time) string {
	switch {
	case profileVersion != activeVersion:
		return StaleProfile
	case now.Sub(measuredAt) > MaxResultAge:
		return StaleAge
	}
	return ""
}

// Defaults are the default profile's settings for a deployment that takes at
// most maxModelLen tokens, drained.
func Defaults(maxModelLen int) Settings {
	return DefaultProfile().Settings(maxModelLen, ModeDrained)
}
