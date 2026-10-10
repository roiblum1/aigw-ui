package api

import (
	"aigw-ui/internal/costcel"
)

// maxCostExpression is far above anything a person writes and far below what
// an annotation or an object may hold.
const maxCostExpression = 1000

// validCostExpression accepts what the gateway would use: the expression is
// compiled and run once the way the gateway does it. Empty is valid and
// leaves the gateway's default.
func validCostExpression(expr string) error {
	if expr == "" {
		return nil
	}
	if len(expr) > maxCostExpression {
		return invalid("cost_expression must be at most %d characters", maxCostExpression)
	}
	if err := costcel.Check(expr); err != nil {
		return invalid("cost_expression is not one the gateway accepts: %v", err)
	}
	return nil
}
