// Package costcel checks and evaluates a cost expression the way the gateway
// does: the same CEL environment and the same rules for the result. The
// gateway only logs a quota expression it cannot use and then charges
// nothing, so the hub has to refuse a bad one itself.
package costcel

import (
	"fmt"

	"github.com/google/cel-go/cel"
)

// Usage is the token counts of one answered request.
type Usage struct {
	Input, CachedInput, CacheCreationInput, Output, Total, Reasoning uint32
}

var env = newEnv()

func newEnv() *cel.Env {
	options := []cel.EnvOption{
		cel.Variable("model", cel.StringType),
		cel.Variable("backend", cel.StringType),
		cel.Variable("route_name", cel.StringType),
	}
	for _, name := range []string{
		"input_tokens", "cached_input_tokens", "cache_creation_input_tokens",
		"output_tokens", "total_tokens", "reasoning_tokens",
	} {
		options = append(options, cel.Variable(name, cel.UintType))
	}
	e, err := cel.NewEnv(options...)
	if err != nil {
		panic(fmt.Sprintf("cost expression environment: %v", err))
	}
	return e
}

// Check reports why the gateway would not use expr. Like the gateway it
// evaluates the expression once with every count at 0, so a division by a
// count is refused.
func Check(expr string) error {
	_, err := Eval(expr, Usage{})
	return err
}

// Eval returns what a request with these counts is charged.
func Eval(expr string, u Usage) (uint64, error) {
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return 0, issues.Err()
	}
	prog, err := env.Program(ast)
	if err != nil {
		return 0, err
	}
	out, _, err := prog.Eval(map[string]any{
		"model": "dummy", "backend": "dummy", "route_name": "dummy",
		"input_tokens": u.Input, "cached_input_tokens": u.CachedInput,
		"cache_creation_input_tokens": u.CacheCreationInput,
		"output_tokens":               u.Output, "total_tokens": u.Total, "reasoning_tokens": u.Reasoning,
	})
	if err != nil {
		return 0, err
	}
	switch out.Type() {
	case cel.UintType:
		return out.Value().(uint64), nil
	case cel.IntType:
		n := out.Value().(int64)
		if n < 0 {
			return 0, fmt.Errorf("the result is negative (%d)", n)
		}
		return uint64(n), nil
	default:
		return 0, fmt.Errorf("the result is %v, not a whole number", out.Type())
	}
}
