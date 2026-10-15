package market

import (
	"testing"
	"time"
)

var sample = []byte(`{"source": "OpenRouter", "url": "https://openrouter.ai/api/v1/models", "snapshot_date": "2026-10-12", "models": [
{"id": "z-ai/glm-5.3", "hf_id": "zai-org/GLM-5.3", "name": "Z.ai: GLM 5.3", "context": 1048576, "prompt": 2.8, "completion": 8.8, "cache_read": 0.56},
{"id": "z-ai/glm-5.3:batch", "hf_id": "zai-org/GLM-5.3", "name": "Z.ai: GLM 5.3 (batch)", "context": 1048576, "prompt": 1.4, "completion": 4.4, "cache_read": null},
{"id": "qwen/qwen3-coder", "hf_id": "Qwen/Qwen3-Coder-480B-A35B-Instruct", "name": "Qwen: Qwen3 Coder", "context": 262144, "prompt": 0.3, "completion": 1.0, "cache_read": null},
{"id": "a/one", "hf_id": "org/Twin", "name": "One", "context": 1, "prompt": 1, "completion": 1, "cache_read": null},
{"id": "b/two", "hf_id": "org/Twin", "name": "Two", "context": 1, "prompt": 2, "completion": 2, "cache_read": null},
{"id": "free/model", "hf_id": "org/Free", "name": "Free", "context": 1, "prompt": 0, "completion": 0, "cache_read": null}
]}`)

func TestEmbeddedSnapshot(t *testing.T) {
	s := Current()
	if s.Source != "OpenRouter" || len(s.Models) < 100 {
		t.Fatalf("embedded snapshot: %s, %d models", s.Source, len(s.Models))
	}
	for _, e := range s.Models {
		if e.ID == "" || e.Prompt < 0 || e.Completion < 0 || (e.CacheRead != nil && *e.CacheRead < 0) {
			t.Fatalf("entry %+v: a variable or missing price must be left out", e)
		}
	}
}

func TestMatch(t *testing.T) {
	s, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	// The variant ":batch" shares the Hugging Face id; the base entry wins,
	// whatever the case of the served name.
	if e, ok := s.Match("glm-5.3", "zai-org/glm-5.3"); !ok || e.ID != "z-ai/glm-5.3" {
		t.Fatalf("glm: %+v %v", e, ok)
	}
	// Two base entries with one Hugging Face id: nothing says which.
	if e, ok := s.Match("org/Twin"); ok {
		t.Fatalf("twin matched %s", e.ID)
	}
	if _, ok := s.Match("qwen3-coder", ""); ok {
		t.Fatal("matched by OpenRouter id, not Hugging Face id")
	}
	if got := s.Search("GLM", 10); len(got) != 2 || got[0].ID != "z-ai/glm-5.3" {
		t.Fatalf("search: %+v", got)
	}
	if got := s.Search("", 3); len(got) != 3 {
		t.Fatalf("search limit: %d", len(got))
	}
	if _, ok := s.Find("b/two"); !ok {
		t.Fatal("find")
	}
	if age := s.Age(time.Date(2026, 12, 11, 9, 0, 0, 0, time.UTC)); age != 60 {
		t.Fatalf("age = %d", age)
	}
}

func TestPrices(t *testing.T) {
	s, _ := Parse(sample)
	glm, _ := s.Find("z-ai/glm-5.3")
	if p, ok := glm.Prices(); !ok || p != (Prices{Input: 2.8, Cached: 0.56, Output: 8.8}) {
		t.Fatalf("glm: %+v", p)
	}
	// No cached price on the market: a tenth of the input price.
	qwen, _ := s.Find("qwen/qwen3-coder")
	if p, _ := qwen.Prices(); p != (Prices{Input: 0.3, Cached: 0.03, Output: 1}) {
		t.Fatalf("qwen: %+v", p)
	}
	free, _ := s.Find("free/model")
	if _, ok := free.Prices(); ok {
		t.Fatal("a free prompt is no price")
	}
	// A cached price above the input price is held at the input price.
	odd := Entry{Prompt: 1, Completion: 2, CacheRead: ptr(3)}
	if p, _ := odd.Prices(); p.Cached != 1 {
		t.Fatalf("odd: %+v", p)
	}
}

func ptr(v float64) *float64 { return &v }

func TestParseRefusesNoDate(t *testing.T) {
	if _, err := Parse([]byte(`{"source":"OpenRouter","models":[]}`)); err == nil {
		t.Fatal("a snapshot without a date was taken")
	}
}
