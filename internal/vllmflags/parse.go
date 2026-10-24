package vllmflags

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Arg is one word of the command as vLLM receives it (after the bash eval
// of KServe's launcher), and who wrote it.
type Arg struct {
	Text string `json:"text"`
	// Source is who wrote it: KServe, the chart (with the value that made
	// it), llmisvc.extraArgs or llmisvc.args.
	Source string `json:"source"`
	// Fixed: KServe or the chart writes it, not the values' own flags.
	// Soft: a fixed flag that a later one may change on purpose (KServe's
	// --shutdown-timeout).
	Fixed bool `json:"fixed,omitempty"`
	Soft  bool `json:"soft,omitempty"`
}

// Problem is one thing wrong with a command.
type Problem struct {
	// Level is error (a save is refused) or warning.
	Level string `json:"level"`
	// Origin is vllm (vLLM refuses it: when it parses, before it loads the
	// model, or when the engine starts) or hub (the hub's own rule: a flag
	// set twice, the GPUs a pod asks for).
	Origin  string `json:"origin"`
	Flag    string `json:"flag,omitempty"`
	Source  string `json:"source,omitempty"`
	Message string `json:"message"`
}

// Setting is one flag as vLLM reads it.
type Setting struct {
	Flag *Flag
	// Option is the spelling, an abbreviation resolved.
	Option string
	// Value is bool, int64, float64, string, []any, map[string]any or nil.
	Value any
	// Text is the value as written, "" for a switch.
	Text string
	Arg  Arg
}

// Command is what vLLM makes of the words of a command.
type Command struct {
	Catalog *Catalog
	// Model is the positional argument (KServe's /mnt/models).
	Model string
	// Set is every flag set, by its dest, in the order written.
	Set map[string][]Setting
	// moved: --model was written, which vLLM moves to the front as the
	// model, so the positional one becomes a word too many.
	moved bool
}

// word is a word after vLLM's own rewriting (FlexibleArgumentParser), and
// the argument it came from.
type word struct {
	text string
	arg  Arg
}

// Parse reads a command's words as `vllm serve` does: vLLM's rewriting
// (underscores, -O, dotted JSON keys) and then argparse's rules (exact
// flags, --flag=value, a unique abbreviation, how many values each takes).
func (c *Catalog) Parse(args []Arg) (*Command, []Problem) {
	words, probs := c.rewrite(args)
	cmd, more := c.argparse(words)
	for _, a := range args {
		if a.Text == "--model" || strings.HasPrefix(a.Text, "--model=") {
			cmd.moved = true
		}
	}
	return cmd, append(probs, more...)
}

func vllmError(a Arg, flag, format string, args ...any) Problem {
	return Problem{Level: "error", Origin: "vllm", Flag: flag, Source: a.Source, Message: fmt.Sprintf(format, args...)}
}

func hubProblem(level string, a Arg, flag, format string, args ...any) Problem {
	return Problem{Level: level, Origin: "hub", Flag: flag, Source: a.Source, Message: fmt.Sprintf(format, args...)}
}

// rewrite is FlexibleArgumentParser.parse_args before argparse: --a_b is
// --a-b (up to the first dot), -O3 is --optimization-level 3, and dotted
// keys (--speculative-config.method=mtp) are merged per flag into one JSON
// value, put at the end.
func (c *Catalog) rewrite(args []Arg) ([]word, []Problem) {
	var probs []Problem
	words := make([]word, 0, len(args))
	for i, a := range args {
		t := a.Text
		switch {
		case strings.HasPrefix(t, "--"):
			key, value, eq := strings.Cut(t, "=")
			key = dashed(key)
			t = key
			if eq {
				t = key + "=" + value
			}
		case strings.HasPrefix(t, "-O") && t != "-O":
			level := strings.TrimPrefix(t[2:], "=")
			words = append(words, word{"--optimization-level", a}, word{level, a})
			continue
		case t == "-O" && i+1 < len(args) && strings.Contains("0123", args[i+1].Text) && len(args[i+1].Text) == 1:
			t = "--optimization-level"
		}
		words = append(words, word{t, a})
	}

	type merged struct {
		value map[string]any
		arg   Arg
	}
	var order []string
	dicts := map[string]*merged{}
	drop := map[int]bool{}
	for i, w := range words {
		if drop[i] {
			continue
		}
		t := w.text
		if strings.HasPrefix(t, "--") && !strings.Contains(t, ".") {
			continue
		}
		if !strings.HasPrefix(t, "-") || !strings.Contains(t, ".") {
			continue
		}
		key, raw, eq := strings.Cut(t, "=")
		if eq && !strings.Contains(key, ".") {
			continue // the dot was in the value
		}
		if !eq {
			if i+1 >= len(words) {
				probs = append(probs, vllmError(w.arg, key, "%s needs a value", key))
				drop[i] = true
				continue
			}
			raw = words[i+1].text
			drop[i+1] = true
		}
		if strings.HasSuffix(key, "+") {
			key = strings.TrimSuffix(key, "+")
			items := []any{}
			for _, s := range strings.Split(raw, ",") {
				items = append(items, s)
			}
			b, _ := json.Marshal(items)
			raw = string(b)
		}
		parts := strings.Split(key, ".")
		var value any
		if v, err := loadJSON(raw); err == nil {
			value = v
		} else if n, err := humanInt(raw); err == nil {
			value = json.Number(fmt.Sprint(n))
		} else {
			value = raw
		}
		for j := len(parts) - 1; j >= 1; j-- {
			value = map[string]any{parts[j]: value}
		}
		m := dicts[parts[0]]
		if m == nil {
			m = &merged{value: map[string]any{}, arg: w.arg}
			dicts[parts[0]] = m
			order = append(order, parts[0])
		}
		for _, d := range mergeInto(m.value, value.(map[string]any), "") {
			probs = append(probs, hubProblem("error", w.arg, parts[0], "%s.%s is set twice: vLLM takes the last", parts[0], d))
		}
		drop[i] = true
	}
	out := make([]word, 0, len(words)+2*len(order))
	for i, w := range words {
		if !drop[i] {
			out = append(out, w)
		}
	}
	for _, k := range order {
		b, _ := json.Marshal(dicts[k].value)
		out = append(out, word{k, dicts[k].arg}, word{string(b), dicts[k].arg})
	}
	return out, probs
}

// dashed is a flag with dashes for underscores, up to its first dot.
func dashed(key string) string {
	head, tail, dot := strings.Cut(key[2:], ".")
	key = "--" + strings.ReplaceAll(head, "_", "-")
	if dot {
		key += "." + tail
	}
	return key
}

// mergeInto is vLLM's recursive_dict_update: maps merge, lists append,
// anything else replaces; it returns the keys set twice.
func mergeInto(dst, src map[string]any, prefix string) []string {
	var dups []string
	for k, v := range src {
		sub, isMap := v.(map[string]any)
		old, oldMap := dst[k].(map[string]any)
		list, isList := v.([]any)
		oldList, oldIsList := dst[k].([]any)
		switch {
		case isMap && oldMap:
			dups = append(dups, mergeInto(old, sub, prefix+k+".")...)
		case isList && oldIsList:
			dst[k] = append(oldList, list...)
		default:
			if _, ok := dst[k]; ok {
				dups = append(dups, prefix+k)
			}
			dst[k] = v
		}
	}
	sort.Strings(dups)
	return dups
}

func loadJSON(s string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("extra data after the JSON value")
	}
	return v, nil
}

var negativeNumber = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

// optionLike says whether argparse takes a word for a flag (known or not)
// rather than a value.
func (c *Catalog) optionLike(t string) bool {
	if len(t) < 2 || t[0] != '-' {
		return false
	}
	if c.options[t] != nil {
		return true
	}
	if key, _, eq := strings.Cut(t, "="); eq && c.options[key] != nil {
		return true
	}
	if len(c.prefixMatches(t)) > 0 {
		return true
	}
	if negativeNumber.MatchString(t) || strings.Contains(t, " ") {
		return false
	}
	return true
}

// match is a flag a word names, and the value written in the word itself.
type match struct {
	flag     *Flag
	option   string
	explicit string
	has      bool
}

// prefixMatches is argparse's _get_option_tuples: the flags a word may
// abbreviate (--max-num-seq), or a short flag with its value attached (-q
// fp8 written -qfp8).
func (c *Catalog) prefixMatches(t string) []match {
	var out []match
	if strings.HasPrefix(t, "--") {
		if !c.AllowAbbrev {
			return nil
		}
		prefix, explicit, eq := strings.Cut(t, "=")
		for _, o := range c.names {
			if strings.HasPrefix(o, prefix) {
				out = append(out, match{c.options[o], o, explicit, eq})
			}
		}
		return out
	}
	short, attached := t[:2], t[2:]
	for _, o := range c.names {
		switch {
		case o == short:
			out = append(out, match{c.options[o], o, attached, attached != ""})
		case strings.HasPrefix(o, t):
			out = append(out, match{c.options[o], o, "", false})
		}
	}
	return out
}

// resolve finds the flag a word names, as argparse's _parse_optional does.
func (c *Catalog) resolve(t string) (match, []string) {
	if f := c.options[t]; f != nil {
		return match{f, t, "", false}, nil
	}
	if key, value, eq := strings.Cut(t, "="); eq {
		if f := c.options[key]; f != nil {
			return match{f, key, value, true}, nil
		}
	}
	ms := c.prefixMatches(t)
	if len(ms) == 1 {
		return ms[0], nil
	}
	var names []string
	for _, m := range ms {
		names = append(names, m.option)
	}
	return match{}, names
}

// argparse reads the rewritten words with argparse's rules for the serve
// parser.
func (c *Catalog) argparse(words []word) (*Command, []Problem) {
	cmd := &Command{Catalog: c, Set: map[string][]Setting{}}
	var probs []Problem
	positional := 0
	values := func(from int) []word {
		var out []word
		for j := from; j < len(words) && !c.optionLike(words[j].text); j++ {
			out = append(out, words[j])
		}
		return out
	}
	for i := 0; i < len(words); i++ {
		w := words[i]
		if !c.optionLike(w.text) {
			if positional == 0 {
				cmd.Model = w.text
			} else {
				probs = append(probs, vllmError(w.arg, "", "%q is not a value of any flag: vLLM refuses the extra word", w.text))
			}
			positional++
			continue
		}
		m, ambiguous := c.resolve(w.text)
		if m.flag == nil {
			name, _, _ := strings.Cut(w.text, "=")
			if len(ambiguous) > 1 {
				probs = append(probs, vllmError(w.arg, name, "%s is short for more than one flag (%s): write it in full", name, strings.Join(ambiguous, ", ")))
			} else {
				probs = append(probs, vllmError(w.arg, name, "%s is not a flag of vLLM %s%s", name, c.Version, c.suggest(name)))
			}
			if next := values(i + 1); len(next) > 0 && !strings.Contains(w.text, "=") {
				i++ // its value: one problem for the pair
			}
			continue
		}
		f := m.flag
		if m.option != w.text && !strings.HasPrefix(w.text, m.option+"=") && len(m.option) > 2 && strings.HasPrefix(m.option, "--") {
			written, _, _ := strings.Cut(w.text, "=")
			probs = append(probs, hubProblem("warning", w.arg, f.Name(), "%s is short for %s: write it in full (it stops working when vLLM adds a flag that starts the same)", written, m.option))
		}
		if f.Deprecated {
			probs = append(probs, hubProblem("warning", w.arg, f.Name(), "%s is deprecated in vLLM %s", m.option, c.Version))
		}
		set := func(v any, text string) {
			cmd.Set[f.Dest] = append(cmd.Set[f.Dest], Setting{Flag: f, Option: m.option, Value: v, Text: text, Arg: w.arg})
		}
		switch f.Kind {
		case "switch", "bool", "count":
			if m.has {
				probs = append(probs, vllmError(w.arg, f.Name(), "%s takes no value (%q)%s", m.option, m.explicit, switchHint(f)))
				continue
			}
			on := true
			if f.Kind == "bool" && strings.HasPrefix(m.option, "--no-") {
				on = false
			}
			if f.Kind == "switch" && string(f.Const) == "false" {
				on = false
			}
			set(on, "")
			continue
		}
		var raws []string
		switch {
		case m.has:
			raws = []string{m.explicit}
		case f.Nargs == "":
			next := values(i + 1)
			if len(next) == 0 {
				probs = append(probs, vllmError(w.arg, f.Name(), "%s needs a value", m.option))
				continue
			}
			raws = []string{next[0].text}
			i++
		case f.Nargs == "?":
			if next := values(i + 1); len(next) > 0 {
				raws = []string{next[0].text}
				i++
			} else {
				var v any
				_ = json.Unmarshal(f.Const, &v)
				set(v, "")
				continue
			}
		case f.Nargs == "*" || f.Nargs == "+":
			next := values(i + 1)
			if len(next) == 0 && f.Nargs == "+" {
				probs = append(probs, vllmError(w.arg, f.Name(), "%s needs at least one value", m.option))
				continue
			}
			for _, n := range next {
				raws = append(raws, n.text)
			}
			i += len(next)
		default:
			n := 0
			fmt.Sscan(f.Nargs, &n)
			next := values(i + 1)
			if len(next) < n {
				probs = append(probs, vllmError(w.arg, f.Name(), "%s needs %d values", m.option, n))
				continue
			}
			for _, x := range next[:n] {
				raws = append(raws, x.text)
			}
			i += n
		}
		var parsed []any
		ok := true
		for _, raw := range raws {
			v, err := c.convert(f, raw)
			if err != nil {
				probs = append(probs, vllmError(w.arg, f.Name(), "%s: %v", m.option, err))
				ok = false
				break
			}
			parsed = append(parsed, v)
		}
		if !ok {
			continue
		}
		if f.Nargs == "*" || f.Nargs == "+" || (f.Nargs != "" && f.Nargs != "?") {
			set(parsed, strings.Join(raws, " "))
		} else {
			set(parsed[0], raws[0])
		}
	}
	return cmd, probs
}

func switchHint(f *Flag) string {
	if f.Kind == "bool" && len(f.Options) > 1 {
		return fmt.Sprintf(": write %s or %s", f.Options[0], f.Options[len(f.Options)-1])
	}
	return ""
}

// suggest is " (did you mean --x?)" for a misspelt flag, or "".
func (c *Catalog) suggest(name string) string {
	if !strings.HasPrefix(name, "--") {
		return ""
	}
	best, bestDist := "", 4
	for _, o := range c.names {
		if !strings.HasPrefix(o, "--") {
			continue
		}
		if d := distance(name, o); d < bestDist {
			best, bestDist = o, d
		}
	}
	if best == "" {
		for _, o := range c.names {
			if strings.HasPrefix(o, "--") && len(name) > 6 && (strings.Contains(o, name[2:]) || strings.Contains(name, o[2:])) {
				return fmt.Sprintf(" (did you mean %s?)", o)
			}
		}
		return ""
	}
	return fmt.Sprintf(" (did you mean %s?)", best)
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
