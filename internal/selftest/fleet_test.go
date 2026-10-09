package selftest

import "testing"

func TestStickiness(t *testing.T) {
	for name, tc := range map[string]struct {
		servedBy []string
		want     string
	}{
		"one site":           {[]string{"site1-a", "site1-a", "site1-a"}, Passed},
		"two sites":          {[]string{"site1-a", "site2-a", "site1-a"}, Failed},
		"no header":          {[]string{"", "", ""}, Warning},
		"header on some":     {[]string{"site1-a", "", "site1-a"}, Warning},
		"too few answers":    {[]string{"site1-a", "site1-a"}, Warning},
		"no answers":         {nil, Warning},
		"two sites, too few": {[]string{"site1-a", "site2-a"}, Warning},
	} {
		if got, detail := stickiness(tc.servedBy); got != tc.want {
			t.Errorf("%s: %s (%s), want %s", name, got, detail, tc.want)
		}
	}
}

func TestChargedTwice(t *testing.T) {
	for name, tc := range map[string]struct {
		counted, request int64
		cost             string
		want             bool
	}{
		"once":                    {12, 12, "", false},
		"twice":                   {24, 12, "", true},
		"a little more":           {13, 12, "", false},
		"tokens unknown":          {24, 0, "", false},
		"with a cost expression":  {48, 12, "output_tokens * 4u", false},
		"three times, both sites": {36, 12, "", true},
	} {
		if got, _ := chargedTwice(tc.counted, tc.request, tc.cost); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
