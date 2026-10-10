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
