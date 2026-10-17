package render

import (
	"sort"
	"strconv"
)

// QuotaClaim is the claim of a token that names, by model slug, the amount
// of each own quota its tenant has (QuotaAmountName). The gateway copies
// each model's to that model's QuotaHeader, which the model's rule for the
// amount matches. The JWT filter takes a client's own value of the header
// out before it writes the token's, and writes nothing when the token names
// none, so a tenant cannot pick an amount.
const QuotaClaim = "quotas"

// QuotaHeader is the header that carries a token's amount for the model.
func QuotaHeader(modelSlug string) string { return "x-aigw-quota-" + modelSlug }

// QuotaAmountName is how a token and a rule name an amount: the limit, the
// window and, for a quota that is only counted, "-shadow".
func QuotaAmountName(limit int64, window string, shadow bool) string {
	name := strconv.FormatInt(limit, 10) + "-" + window
	if shadow {
		name += "-shadow"
	}
	return name
}

// QuotaAmount is one amount of the tenants' own quotas on a model, with the
// fixed position of its rule in the model's QuotaPolicy. The position is part of the names of the rule's counters in
// Redis, so it never moves while the amount is in use.
type QuotaAmount struct {
	Slot   int
	Limit  int64
	Window string
	Shadow bool
}

func (a QuotaAmount) name() string { return QuotaAmountName(a.Limit, a.Window, a.Shadow) }

// amountRules returns the amounts of a model in order: the amount at index
// i holds Slot i. A position no amount holds is returned with Limit 0 and
// is rendered as a placeholder, as a tenant rule's is.
func amountRules(m Model) []QuotaAmount {
	var rules []QuotaAmount
	for _, a := range m.Amounts {
		if a.Slot < 0 || !validQuota(a.Limit, a.Window) {
			continue
		}
		for len(rules) <= a.Slot {
			rules = append(rules, QuotaAmount{})
		}
		rules[a.Slot] = a
	}
	return rules
}

// amountRule is the rule of one amount: it matches the requests whose token
// names that amount for the model, and counts each tenant in a bucket of its
// own (Distinct on the tenant header).
func amountRule(m Model, a QuotaAmount, shadow bool) map[string]any {
	rule := map[string]any{
		"clientSelectors": []any{
			map[string]any{"headers": []any{
				map[string]any{"name": QuotaHeader(m.Slug), "type": "Exact", "value": a.name()},
				map[string]any{"name": TenantHeader, "type": "Distinct"},
			}},
		},
		"quota": map[string]any{"limit": a.Limit, "duration": a.Window},
	}
	if a.Shadow || shadow {
		rule["shadowMode"] = true
	}
	return rule
}

// amountStem is the Redis key of a tenant's counter under the rule at
// position rule, without the key prefix, the tenant and the window start.
// The gateway sorts a rule's headers by name, names each match after its
// position, the header and the value it matches, and sends a Distinct
// header's own value.
func amountStem(prefix string, rule int, m Model, a QuotaAmount) string {
	return tenantStem(prefix, rule, QuotaHeader(m.Slug)+"|"+a.name())
}

// tenantStem is the Redis key of a tenant's counter under a rule that
// matches one header (match, "<header>|<value or pattern>") and counts each
// tenant by the tenant header, without the key prefix, the tenant and the
// window start.
func tenantStem(prefix string, rule int, match string) string {
	r := "rule-" + strconv.Itoa(rule) + "-"
	first := r + match + "-match-0"
	return prefix + first + "_" + first + "_" + r + TenantHeader + "-match-1_"
}

// QuotaClaimHeaders returns the header of every model whose rules are by
// amount and that has an amount, sorted by header, each with the claim the
// gateway fills it from.
func QuotaClaimHeaders(models []Model) []map[string]any {
	seen := map[string]bool{}
	var slugs []string
	for _, m := range models {
		if m.ByAmount && len(amountRules(m)) > 0 && !seen[m.Slug] {
			seen[m.Slug] = true
			slugs = append(slugs, m.Slug)
		}
	}
	sort.Strings(slugs)
	out := make([]map[string]any, 0, len(slugs))
	for _, slug := range slugs {
		out = append(out, map[string]any{"header": QuotaHeader(slug), "claim": QuotaClaim + "." + slug})
	}
	return out
}
