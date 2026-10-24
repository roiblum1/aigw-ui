package vllmflags

import (
	"encoding/json"
	"os"
	"testing"
)

type verdicts struct {
	Version string `json:"vllm"`
	Cases   []struct {
		Name  string   `json:"name"`
		Args  []string `json:"args"`
		OK    bool     `json:"ok"`
		Stage string   `json:"stage"`
		Error string   `json:"error"`
	} `json:"cases"`
}

// The hub refuses what vLLM refuses and nothing it takes. vLLM's verdicts
// on testdata/cases.json were written inside the vLLM image by
// hack/vllm-flags.sh (its parser, its serve checks, its parallel and
// scheduler config checks, its startup configs). The hub's own rules (a
// flag set twice) are left out: vLLM says nothing of those.
func TestVerdictsMatchVLLM(t *testing.T) {
	all, err := Catalogs()
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name string `json:"name"`
	}
	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range all {
		raw, err := os.ReadFile("testdata/verdicts-" + c.Version + ".json")
		if err != nil {
			t.Errorf("vLLM %s: no verdicts (hack/vllm-flags.sh writes them): %v", c.Version, err)
			continue
		}
		var v verdicts
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		if len(v.Cases) != len(cases) {
			t.Errorf("vLLM %s: %d verdicts for %d cases: run hack/vllm-flags.sh again", c.Version, len(v.Cases), len(cases))
		}
		for _, tc := range v.Cases {
			args := make([]Arg, len(tc.Args))
			for i, a := range tc.Args {
				args[i] = Arg{Text: a, Source: "test"}
			}
			_, probs := c.Check(args)
			var refused []string
			for _, p := range probs {
				if p.Origin == "vllm" && p.Level == "error" {
					refused = append(refused, p.Message)
				}
			}
			switch {
			case tc.OK && len(refused) > 0:
				t.Errorf("vLLM %s, %s: vLLM takes it, the hub refuses it: %q", c.Version, tc.Name, refused)
			case !tc.OK && len(refused) == 0:
				t.Errorf("vLLM %s, %s: vLLM refuses it (%s: %s), the hub does not", c.Version, tc.Name, tc.Stage, tc.Error)
			}
		}
	}
}
