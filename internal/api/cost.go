package api

import (
	"regexp"
)

var (
	costToken = regexp.MustCompile(`^(?:[a-z_]+|[0-9]+(?:\.[0-9]+)?u?|[-+*/()]|\s+)`)
	costWord  = regexp.MustCompile(`^[a-z_]+$`)
	costNames = map[string]bool{
		"input_tokens": true, "output_tokens": true, "total_tokens": true, "cached_input_tokens": true,
		"cache_creation_input_tokens": true, "reasoning_tokens": true, "uint": true, "double": true,
	}
)

// validCostExpression accepts arithmetic over the token counts the gateway
// exposes. It is a guard against typos, not a CEL parser: the gateway has the
// final say when the policy is applied.
func validCostExpression(expr string) error {
	if len(expr) > 200 {
		return invalid("cost_expression must be at most 200 characters")
	}
	depth := 0
	for rest := expr; rest != ""; {
		tok := costToken.FindString(rest)
		switch {
		case tok == "":
			return invalid("cost_expression has an unsupported character at %q", rest)
		case costWord.MatchString(tok) && !costNames[tok]:
			return invalid("cost_expression uses %q; the known names are input_tokens, output_tokens, total_tokens, cached_input_tokens, cache_creation_input_tokens, reasoning_tokens, uint and double", tok)
		case tok == "(":
			depth++
		case tok == ")":
			depth--
		}
		if depth < 0 {
			return invalid("cost_expression has an unmatched )")
		}
		rest = rest[len(tok):]
	}
	if depth != 0 {
		return invalid("cost_expression has an unmatched (")
	}
	return nil
}
