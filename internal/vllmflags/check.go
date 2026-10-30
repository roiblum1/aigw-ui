package vllmflags

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
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
	// Origin is vllm (vLLM refuses it when it reads its flags) or hub (the
	// hub's own rule: a flag set twice, the GPUs a pod asks for).
	Origin  string `json:"origin"`
	Flag    string `json:"flag,omitempty"`
	Source  string `json:"source,omitempty"`
	Message string `json:"message"`
}

// Setting is one flag as written.
type Setting struct {
	Flag *Flag
	// Option is the spelling, an abbreviation resolved.
	Option string
	// Text is the value as written: "" for a switch, several values joined
	// by spaces.
	Text string
	// On is false for the --no- spelling of a pair.
	On  bool
	Arg Arg
}

// Shown is a setting as written: --flag=value, or --flag for a switch.
func (s Setting) Shown() string {
	if s.Text == "" {
		return s.Option
	}
	return s.Option + "=" + s.Text
}

// Command is the flags of a command as vLLM reads them.
type Command struct {
	Catalog *Catalog
	// Model is the positional argument (KServe's /mnt/models).
	Model string
	// Set is every flag set, by its dest, in the order written.
	Set map[string][]Setting
}

// word is a word after vLLM's own rewriting, and the argument it came from.
type word struct {
	text string
	arg  Arg
}

func vllmError(a Arg, flag, format string, args ...any) Problem {
	return Problem{Level: "error", Origin: "vllm", Flag: flag, Source: a.Source, Message: fmt.Sprintf(format, args...)}
}

func hubProblem(level string, a Arg, flag, format string, args ...any) Problem {
	return Problem{Level: level, Origin: "hub", Flag: flag, Source: a.Source, Message: fmt.Sprintf(format, args...)}
}

// Check reads a command as `vllm serve` reads its flags and returns what it
// would refuse that its help shows: a flag it does not list, a value
// missing or given to a switch, a value outside the choices, a word too
// many. It adds the hub's own rule: a flag set twice.
func (c *Catalog) Check(args []Arg) (*Command, []Problem) {
	cmd := &Command{Catalog: c, Set: map[string][]Setting{}}
	var probs []Problem
	words := rewrite(args)
	values := func(from int) []word {
		var out []word
		for j := from; j < len(words) && !c.optionLike(words[j].text); j++ {
			out = append(out, words[j])
		}
		return out
	}
	positional := 0
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
		key, _, hasValue := strings.Cut(w.text, "=")

		// A dotted key (--speculative-config.method=mtp) is one key of a
		// flag's JSON value; vLLM merges them into one.
		if base, _, dotted := strings.Cut(key, "."); dotted {
			m, _ := c.resolve(base)
			f := m.flag
			switch {
			case f == nil:
				probs = append(probs, vllmError(w.arg, base, "%s is not a flag of vLLM %s%s", base, c.Version, c.suggest(base)))
			case f.Value == NoValue:
				probs = append(probs, vllmError(w.arg, base, "%s takes no value, so it has no keys (%s)", base, key))
			case !hasValue && len(values(i+1)) == 0:
				probs = append(probs, vllmError(w.arg, base, "%s needs a value", strings.TrimSuffix(key, "+")))
			}
			if !hasValue && len(values(i+1)) > 0 {
				i++
			}
			continue
		}

		m, ambiguous := c.resolve(w.text)
		if m.flag == nil {
			if len(ambiguous) > 1 {
				probs = append(probs, vllmError(w.arg, key, "%s is short for more than one flag (%s): write it in full", key, strings.Join(ambiguous, ", ")))
			} else {
				probs = append(probs, vllmError(w.arg, key, "%s is not a flag of vLLM %s%s", key, c.Version, c.suggest(key)))
			}
			if !hasValue && len(values(i+1)) > 0 {
				i++ // its value: one problem for the pair
			}
			continue
		}
		f := m.flag
		if m.option != w.text && !strings.HasPrefix(w.text, m.option+"=") && len(m.option) > 2 && strings.HasPrefix(m.option, "--") {
			probs = append(probs, hubProblem("warning", w.arg, f.Name(), "%s is short for %s: write it in full (it stops working when vLLM adds a flag that starts the same)", key, m.option))
		}
		set := func(text string) {
			on := !(strings.HasPrefix(m.option, "--no-") && m.option != f.Options[0])
			cmd.Set[f.Dest] = append(cmd.Set[f.Dest], Setting{Flag: f, Option: m.option, Text: text, On: on, Arg: w.arg})
		}
		var got []string
		switch next := values(i + 1); {
		case f.Value == NoValue:
			if m.has {
				probs = append(probs, vllmError(w.arg, f.Name(), "%s takes no value (%q)%s", m.option, m.explicit, pairHint(f)))
				continue
			}
		case m.has:
			got = []string{m.explicit}
		case f.Value == OneValue && len(next) == 0:
			probs = append(probs, vllmError(w.arg, f.Name(), "%s needs a value", m.option))
			continue
		case f.Value == OneOrMore && len(next) == 0:
			probs = append(probs, vllmError(w.arg, f.Name(), "%s needs at least one value", m.option))
			continue
		case f.Value == OneValue || (f.Value == Optional && len(next) > 0):
			got = []string{next[0].text}
		case f.Value == OneOrMore || f.Value == AnyNumber:
			for _, n := range next {
				got = append(got, n.text)
			}
		}
		if !m.has {
			i += len(got)
		}
		if bad := notAChoice(f, got); bad != "" {
			probs = append(probs, vllmError(w.arg, f.Name(), "%s: %q is not one of its choices (%s)", m.option, bad, shortList(f.Choices)))
			continue
		}
		set(strings.Join(got, " "))
	}
	probs = append(probs, cmd.duplicates()...)
	if s, ok := cmd.last("config"); ok {
		probs = append(probs, hubProblem("warning", s.Arg, "--config", "the flags in the --config file are read in the pod; the hub does not check them"))
	}
	return cmd, probs
}

// rewrite is what vLLM's FlexibleArgumentParser does before argparse:
// --a_b is --a-b (up to the first dot), and -O3 is --optimization-level 3.
func rewrite(args []Arg) []word {
	words := make([]word, 0, len(args))
	for i, a := range args {
		t := a.Text
		switch {
		case strings.HasPrefix(t, "--"):
			key, value, eq := strings.Cut(t, "=")
			head, tail, dot := strings.Cut(key[2:], ".")
			t = "--" + strings.ReplaceAll(head, "_", "-")
			if dot {
				t += "." + tail
			}
			if eq {
				t += "=" + value
			}
		case strings.HasPrefix(t, "-O") && t != "-O":
			words = append(words, word{"--optimization-level", a}, word{strings.TrimPrefix(t[2:], "="), a})
			continue
		case t == "-O" && i+1 < len(args) && len(args[i+1].Text) == 1 && strings.Contains("0123", args[i+1].Text):
			t = "--optimization-level"
		}
		words = append(words, word{t, a})
	}
	return words
}

// notAChoice is the first value outside a flag's choices, or "". Values are
// compared lowercased with underscores for dashes, as vLLM reads some of
// them, so the check never refuses a value vLLM takes.
func notAChoice(f *Flag, got []string) string {
	if len(f.Choices) == 0 {
		return ""
	}
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "-", "_") }
	for _, v := range got {
		ok := false
		for _, c := range f.Choices {
			ok = ok || norm(c) == norm(v)
		}
		if !ok {
			return v
		}
	}
	return ""
}

func shortList(choices []string) string {
	if len(choices) <= 12 {
		return strings.Join(choices, ", ")
	}
	return strings.Join(choices[:12], ", ") + fmt.Sprintf(" and %d more: vllm serve --help", len(choices)-12)
}

func pairHint(f *Flag) string {
	if len(f.Options) == 2 && strings.HasPrefix(f.Options[1], "--no-") {
		return fmt.Sprintf(": write %s or %s", f.Options[0], f.Options[1])
	}
	return ""
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
	return !negativeNumber.MatchString(t) && !strings.Contains(t, " ")
}

// match is a flag a word names, and the value written in the word itself.
type match struct {
	flag     *Flag
	option   string
	explicit string
	has      bool
}

// prefixMatches is argparse's _get_option_tuples (vLLM keeps allow_abbrev):
// the flags a word may abbreviate (--max-num-seq), or a short flag with its
// value attached (-q fp8 written -qfp8).
func (c *Catalog) prefixMatches(t string) []match {
	var out []match
	if strings.HasPrefix(t, "--") {
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

func (cmd *Command) last(dest string) (Setting, bool) {
	s := cmd.Set[dest]
	if len(s) == 0 {
		return Setting{}, false
	}
	return s[len(s)-1], true
}

// Int is dest's integer when the command sets it.
func (cmd *Command) Int(dest string) (int64, bool) {
	s, ok := cmd.last(dest)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s.Text, 10, 64)
	return n, err == nil
}

// IntOr is dest's integer when the command sets it, else def.
func (cmd *Command) IntOr(dest string, def int64) int64 {
	if n, ok := cmd.Int(dest); ok {
		return n
	}
	return def
}

// On says whether the command turns a switch on: set, and not by its --no-
// spelling. A switch left out is off.
func (cmd *Command) On(dest string) bool {
	s, ok := cmd.last(dest)
	return ok && s.On && s.Text == ""
}

// duplicates finds a flag set more than once. vLLM takes the last one and
// says nothing, so a cluster's flag in llmisvc.extraArgs that its recipe's
// llmisvc.args sets too (they come after) does nothing, and a flag KServe
// sets is changed under it.
func (cmd *Command) duplicates() []Problem {
	var out []Problem
	dests := make([]string, 0, len(cmd.Set))
	for d := range cmd.Set {
		dests = append(dests, d)
	}
	sort.Strings(dests)
	for _, d := range dests {
		s := cmd.Set[d]
		if len(s) < 2 {
			continue
		}
		last := s[len(s)-1]
		name := last.Flag.Name()
		for _, prev := range s[:len(s)-1] {
			switch {
			case prev.Arg.Fixed && last.Arg.Fixed:
			case prev.Arg.Fixed && prev.Arg.Soft:
				out = append(out, hubProblem("warning", last.Arg, name, "%s sets %s; %s changes it to %s", prev.Arg.Source, prev.Shown(), last.Arg.Source, last.Shown()))
			case prev.Arg.Fixed:
				out = append(out, hubProblem("error", last.Arg, name, "%s sets %s already: %s sets it again and wins (%s); remove it from %s", prev.Arg.Source, prev.Shown(), last.Arg.Source, last.Shown(), last.Arg.Source))
			case last.Arg.Fixed:
				out = append(out, hubProblem("error", prev.Arg, name, "%s sets %s, and %s sets it after it (%s), which wins: set it in one place", prev.Arg.Source, prev.Shown(), last.Arg.Source, last.Shown()))
			default:
				out = append(out, hubProblem("error", last.Arg, name, "%s is set twice: %s in %s, %s in %s; vLLM takes the last, without a word", name, prev.Shown(), prev.Arg.Source, last.Shown(), last.Arg.Source))
			}
		}
	}
	return out
}

// Errors are the problems that refuse a save.
func Errors(probs []Problem) []Problem {
	var out []Problem
	for _, p := range probs {
		if p.Level == "error" {
			out = append(out, p)
		}
	}
	return out
}
