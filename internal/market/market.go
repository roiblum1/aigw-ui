// Package market holds what models cost on the open market: a snapshot of
// the public OpenRouter catalog, embedded in the binary so that a hub with no
// network has it. Nothing here fetches anything; hack/fetch-market.sh writes
// the snapshot on a machine that has internet access.
//
// The market gives a model its default price, so that a dollar of a budget
// means what the same traffic would cost on OpenRouter. Nothing in this
// package changes a price: it only says what the market's is.
package market

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

//go:embed openrouter.json
var embedded []byte

// Snapshot is the market on one day.
type Snapshot struct {
	Source string `json:"source"`
	URL    string `json:"url"`
	// Date is the day the catalog was read, as YYYY-MM-DD.
	Date   string  `json:"snapshot_date"`
	Models []Entry `json:"models"`
}

// Entry is one model of the catalog. Prices are dollars for a million
// tokens; CacheRead is nil when the market sells no cached prompt token.
type Entry struct {
	ID string `json:"id"`
	// HFID is the model's Hugging Face id, "" when the catalog gives none.
	HFID       string   `json:"hf_id"`
	Name       string   `json:"name"`
	Context    int64    `json:"context"`
	Prompt     float64  `json:"prompt"`
	Completion float64  `json:"completion"`
	CacheRead  *float64 `json:"cache_read"`
}

var current = mustParse(embedded)

// Current is the snapshot embedded in this binary.
func Current() *Snapshot { return current }

// Parse reads a snapshot as hack/fetch-market.sh writes it.
func Parse(data []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.DateOnly, s.Date); err != nil {
		return nil, fmt.Errorf("snapshot_date: %w", err)
	}
	return &s, nil
}

func mustParse(data []byte) *Snapshot {
	s, err := Parse(data)
	if err != nil {
		panic("market snapshot: " + err.Error())
	}
	return s
}

// Age is how many whole days old the snapshot is at now.
func (s *Snapshot) Age(now time.Time) int {
	day, _ := time.Parse(time.DateOnly, s.Date)
	return int(now.UTC().Sub(day).Hours() / 24)
}

// Find returns the entry with an OpenRouter id.
func (s *Snapshot) Find(id string) (Entry, bool) {
	for _, e := range s.Models {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Match returns the entry whose Hugging Face id is one of the names a model
// is served under. OpenRouter lists one model several times, such as
// "z-ai/glm-5.3" and "z-ai/glm-5.3:batch" with the same Hugging Face id: the
// entry without a ":" variant is taken. More than one such entry is no
// match, since nothing says which one is meant.
func (s *Snapshot) Match(names ...string) (Entry, bool) {
	var base, all []Entry
	for _, e := range s.Models {
		if e.HFID == "" || !matches(e.HFID, names) {
			continue
		}
		all = append(all, e)
		if !strings.Contains(e.ID, ":") {
			base = append(base, e)
		}
	}
	switch {
	case len(base) == 1:
		return base[0], true
	case len(base) == 0 && len(all) == 1:
		return all[0], true
	}
	return Entry{}, false
}

func matches(hfID string, names []string) bool {
	for _, n := range names {
		if n != "" && strings.EqualFold(n, hfID) {
			return true
		}
	}
	return false
}

// Search returns up to limit entries whose id, Hugging Face id or name holds
// q, ignoring case: entries without a ":" variant first, then by id.
func (s *Snapshot) Search(q string, limit int) []Entry {
	q = strings.ToLower(strings.TrimSpace(q))
	out := []Entry{}
	for _, e := range s.Models {
		if q == "" || strings.Contains(strings.ToLower(e.ID), q) || strings.Contains(strings.ToLower(e.HFID), q) ||
			strings.Contains(strings.ToLower(e.Name), q) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		vi, vj := strings.Contains(out[i].ID, ":"), strings.Contains(out[j].ID, ":")
		if vi != vj {
			return !vi
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Prices are a model's prices in dollars for a million tokens.
type Prices struct {
	Input, Cached, Output float64
}

// Prices returns the prices a model takes from the market, and false when
// the market's cannot be used: a free prompt is no price. The cached price
// is the market's, or a tenth of the input price when it sells none, as the
// Prices dialog does; never more than the input price.
func (e Entry) Prices() (Prices, bool) {
	if !(e.Prompt > 0) || e.Completion < 0 {
		return Prices{}, false
	}
	p := Prices{Input: e.Prompt, Cached: round(e.Prompt / 10), Output: e.Completion}
	if e.CacheRead != nil && *e.CacheRead >= 0 {
		p.Cached = *e.CacheRead
	}
	p.Cached = math.Min(p.Cached, p.Input)
	return p, true
}

// round drops what a division leaves past a hundred-thousandth of a dollar,
// a credit.
func round(v float64) float64 { return math.Round(v*1e5) / 1e5 }
