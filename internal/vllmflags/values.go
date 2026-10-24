package vllmflags

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// convert is a value as the flag's converter reads it (the type argparse
// runs), then checked against the flag's choices, as argparse does.
func (c *Catalog) convert(f *Flag, raw string) (any, error) {
	if f.Optional && (raw == "" || raw == "None") {
		return nil, nil
	}
	var v any
	var err error
	switch f.Type {
	case "int", "port":
		v, err = pyInt(raw)
	case "float":
		v, err = pyFloat(raw)
	case "hint":
		v, err = humanInt(raw)
	case "hint_auto":
		if s := strings.TrimSpace(raw); s == "-1" || strings.EqualFold(s, "auto") {
			v = int64(-1)
		} else {
			v, err = humanInt(raw)
		}
	case "json":
		v, err = loadJSON(raw)
		if err != nil {
			err = fmt.Errorf("%q is not JSON", raw)
		}
	case "dict_or_str":
		v = raw
		if objectLike.MatchString(raw) {
			if v, err = loadJSON(raw); err != nil {
				err = fmt.Errorf("%q is not JSON", raw)
			}
		}
	case "config":
		v, err = loadJSON(expandHumanNumbers(raw))
		if err != nil {
			return nil, fmt.Errorf("%q is not JSON", raw)
		}
		if err := c.checkConfig(f.Config, v, ""); err != nil {
			return nil, err
		}
	default: // str, any
		v = raw
		if f.Normalize == "lower_underscore" {
			v = strings.ReplaceAll(strings.ToLower(raw), "-", "_")
		}
	}
	if err != nil {
		return nil, err
	}
	if len(f.Choices) > 0 {
		s := choiceText(v)
		for _, ch := range f.Choices {
			if ch == s {
				return v, nil
			}
		}
		return nil, fmt.Errorf("%q is not one of %s", raw, strings.Join(f.Choices, ", "))
	}
	return v, nil
}

var objectLike = regexp.MustCompile(`(?s)^\s*{.*}\s*$`)

func choiceText(v any) string {
	switch x := v.(type) {
	case nil:
		return "<None>" // argparse compares None, which no choice equals
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

var pyIntPattern = regexp.MustCompile(`^[+-]?\d+(_\d+)*$`)

// pyInt is Python's int(text): spaces around, a sign, digits with single
// underscores between them.
func pyInt(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if !pyIntPattern.MatchString(s) {
		return 0, fmt.Errorf("%q is not an integer", raw)
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not an integer", raw)
	}
	return n, nil
}

// pyFloat is Python's float(text).
func pyFloat(raw string) (float64, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch strings.TrimLeft(s, "+-") {
	case "inf", "infinity":
		if strings.HasPrefix(s, "-") {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	case "nan":
		return math.NaN(), nil
	}
	if strings.Contains(s, "__") || strings.HasPrefix(strings.TrimLeft(s, "+-"), "_") || strings.HasSuffix(s, "_") {
		return 0, fmt.Errorf("%q is not a number", raw)
	}
	n, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil || strings.ContainsAny(s, "xp") {
		return 0, fmt.Errorf("%q is not a number", raw)
	}
	return n, nil
}

var humanPattern = regexp.MustCompile(`^(\d+(?:\.\d+)?)([kKmMgGtT])$`)

// humanInt is vLLM's human_readable_int: 25.6k is 25,600 and 1K 1,024
// (no decimals with the binary suffixes), else a plain integer.
func humanInt(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if m := humanPattern.FindStringSubmatch(s); m != nil {
		decimal := map[string]float64{"k": 1e3, "m": 1e6, "g": 1e9, "t": 1e12}
		binary := map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}
		if mult, ok := decimal[m[2]]; ok {
			f, _ := strconv.ParseFloat(m[1], 64)
			return int64(f * mult), nil
		}
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q: no decimals with the binary suffix %s (did you mean %s%s?)", raw, m[2], m[1], strings.ToLower(m[2]))
		}
		return n * binary[m[2]], nil
	}
	return pyInt(raw)
}

var (
	quotedString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	humanNumber  = regexp.MustCompile(`\b\d+(?:\.\d+)?[kKmMgGtT]\b`)
)

// expandHumanNumbers is vLLM's _expand_json_human_readable_numbers: 80g
// outside JSON strings is 80000000000.
func expandHumanNumbers(s string) string {
	var out strings.Builder
	last := 0
	for _, loc := range quotedString.FindAllStringIndex(s, -1) {
		out.WriteString(humanNumber.ReplaceAllStringFunc(s[last:loc[0]], humanText))
		out.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	out.WriteString(humanNumber.ReplaceAllStringFunc(s[last:], humanText))
	return out.String()
}

func humanText(s string) string {
	n, err := humanInt(s)
	if err != nil {
		return s
	}
	return strconv.FormatInt(n, 10)
}

// checkConfig checks a JSON value against a config's keys: a key it does
// not have (when it is strict), a choice it does not offer, a number or a
// switch that is not one, as pydantic's lax mode reads them.
func (c *Catalog) checkConfig(name string, v any, path string) error {
	cfg, ok := c.Configs[name]
	if !ok {
		return nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s must be a JSON object of %s's keys", orRoot(path), name)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		field, known := cfg.Fields[k]
		if !known {
			if cfg.Strict {
				return fmt.Errorf("%s is not a key of %s%s", path+k, name, closest(k, cfg.Fields))
			}
			continue
		}
		if err := c.checkField(name, path+k, field, obj[k]); err != nil {
			return err
		}
	}
	return nil
}

func orRoot(path string) string {
	if path == "" {
		return "the value"
	}
	return strings.TrimSuffix(path, ".")
}

func (c *Catalog) checkField(config, key string, f Field, v any) error {
	if v == nil {
		return nil
	}
	switch f.Type {
	case "choice":
		s := choiceText(normalNumber(v))
		for _, ch := range f.Choices {
			if ch == s || (f.Fold && strings.EqualFold(ch, s)) {
				return nil
			}
		}
		return fmt.Errorf("%s: %q is not one of %s", key, s, strings.Join(f.Choices, ", "))
	case "config":
		return c.checkConfig(f.Config, v, key+".")
	case "int":
		switch x := v.(type) {
		case json.Number:
			if _, err := strconv.ParseInt(string(x), 10, 64); err == nil {
				return nil
			}
			if fl, err := x.Float64(); err == nil && fl == math.Trunc(fl) {
				return nil
			}
		case string:
			if _, err := pyInt(x); err == nil {
				return nil
			}
		case bool:
			return nil
		}
		return fmt.Errorf("%s: %v is not an integer", key, v)
	case "float":
		switch x := v.(type) {
		case json.Number, bool:
			return nil
		case string:
			if _, err := pyFloat(x); err == nil {
				return nil
			}
		}
		return fmt.Errorf("%s: %v is not a number", key, v)
	case "bool":
		switch x := v.(type) {
		case bool:
			return nil
		case json.Number:
			if x == "0" || x == "1" {
				return nil
			}
		case string:
			switch strings.ToLower(x) {
			case "0", "1", "true", "false", "yes", "no", "on", "off", "t", "f", "y", "n":
				return nil
			}
		}
		return fmt.Errorf("%s: %v is not true or false", key, v)
	}
	return nil
}

func normalNumber(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return f
		}
	}
	return v
}

// closest is " (did you mean x?)" for a misspelt config key, or "".
func closest(key string, fields map[string]Field) string {
	best, bestDist := "", 4
	norm := strings.ReplaceAll(key, "-", "_")
	if _, ok := fields[norm]; ok {
		return fmt.Sprintf(" (did you mean %s? Keys after the dot keep their underscores)", norm)
	}
	for k := range fields {
		if d := distance(key, k); d < bestDist || (d == bestDist && k < best) {
			best, bestDist = k, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean %s?)", best)
}
