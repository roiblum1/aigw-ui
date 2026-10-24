// Package vllmflags checks the arguments of `vllm serve` before they reach a
// pod. KServe hands the arguments to vLLM as they are, and vLLM refuses a
// flag it does not have, a bad value or a bad combination only when the pod
// starts. This package knows the flags of each vLLM version from a list
// written by vLLM's own parser (hack/vllm-flags.py, run inside the image),
// reads arguments the way vLLM's parser reads them, and applies the checks
// vLLM runs before it loads a model. testdata/verdicts-<version>.json holds
// vLLM's verdicts on testdata/cases.json; TestVerdictsMatchVLLM keeps the two
// in agreement.
package vllmflags

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed catalogs/*.json
var catalogFiles embed.FS

// Catalog is the flags of one vLLM version's `vllm serve`.
type Catalog struct {
	Version string `json:"vllm"`
	// Image is the image the list was written from.
	Image  string `json:"image"`
	Python string `json:"python"`
	// AllowAbbrev: argparse takes a unique start of a long flag for the
	// flag (--max-num-seq for --max-num-seqs).
	AllowAbbrev bool              `json:"allow_abbrev"`
	Flags       []*Flag           `json:"flags"`
	Configs     map[string]Config `json:"configs"`

	options map[string]*Flag
	names   []string // every option, sorted
}

// Flag is one option of `vllm serve`.
type Flag struct {
	// Options are its spellings: --tensor-parallel-size, -tp; a boolean
	// has --x and --no-x.
	Options []string `json:"options"`
	Dest    string   `json:"dest"`
	// Kind is value, switch (no value), bool (--x / --no-x), append or count.
	Kind  string `json:"kind"`
	Nargs string `json:"nargs,omitempty"`
	// Type is what a value must be: int, float, str, hint (an integer that
	// may end in k/K/m/M/g/G/t/T), hint_auto (hint, -1 or auto), json,
	// config (a JSON object of Config), dict_or_str, port or any.
	Type     string `json:"type,omitempty"`
	Optional bool   `json:"optional,omitempty"`
	// Normalize lower_underscore: the value is lowercased, dashes made
	// underscores, before it is compared with Choices.
	Normalize string `json:"normalize,omitempty"`
	// Config names the JSON object's config; ConfigAt "startup" when vLLM
	// builds it only when the engine starts (--speculative-config).
	Config     string          `json:"config,omitempty"`
	ConfigAt   string          `json:"config_at,omitempty"`
	Choices    []string        `json:"choices,omitempty"`
	Const      json.RawMessage `json:"const,omitempty"`
	Default    json.RawMessage `json:"default,omitempty"`
	Deprecated bool            `json:"deprecated,omitempty"`
	Group      string          `json:"group,omitempty"`
	Help       string          `json:"help,omitempty"`
}

// Name is the flag's main spelling.
func (f *Flag) Name() string { return f.Options[0] }

// Config is the keys of a JSON config (KVTransferConfig, ...).
type Config struct {
	// Strict: a key the config does not have is refused.
	Strict bool             `json:"strict"`
	Fields map[string]Field `json:"fields"`
}

// Field is one key of a config.
type Field struct {
	// Type is choice, config, bool, int, float, str, object, list or any.
	Type    string   `json:"type"`
	Choices []string `json:"choices,omitempty"`
	// Fold: the choice is compared in any case (an enum's member name).
	Fold     bool   `json:"fold,omitempty"`
	Config   string `json:"config,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

var (
	loadOnce sync.Once
	loaded   []*Catalog
	loadErr  error
)

// Catalogs are the lists the hub has, oldest version first.
func Catalogs() ([]*Catalog, error) {
	loadOnce.Do(func() { loaded, loadErr = load(catalogFiles) })
	return loaded, loadErr
}

func load(files fs.FS) ([]*Catalog, error) {
	paths, err := fs.Glob(files, "catalogs/*.json")
	if err != nil {
		return nil, err
	}
	var out []*Catalog
	for _, p := range paths {
		raw, err := fs.ReadFile(files, p)
		if err != nil {
			return nil, err
		}
		c, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i].Version, out[j].Version) })
	return out, nil
}

// Parse reads a list written by hack/vllm-flags.py.
func Parse(raw []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.Version == "" || len(c.Flags) == 0 {
		return nil, fmt.Errorf("not a flag list: no vllm version or no flags")
	}
	c.options = make(map[string]*Flag)
	for _, f := range c.Flags {
		for _, o := range f.Options {
			c.options[o] = f
			c.names = append(c.names, o)
		}
	}
	sort.Strings(c.names)
	return &c, nil
}

// Flag is the flag spelled exactly so, or nil.
func (c *Catalog) Flag(option string) *Flag { return c.options[option] }

// FlagOf is the flag that sets dest, or nil.
func (c *Catalog) FlagOf(dest string) *Flag {
	for _, f := range c.Flags {
		if f.Dest == dest {
			return f
		}
	}
	return nil
}

var vllmTag = regexp.MustCompile(`^v(\d+\.\d+\.\d+)(-[A-Za-z0-9_.-]+)?$`)

// ForImage is the list for an image's vLLM. exact says it is the image's
// own version: the image is the one a list was written from, or a vllm
// build (a repository named vllm*, not under lmcache/) tagged vX.Y.Z with a
// list for X.Y.Z. Otherwise it is the newest list, and why says what is not
// known; the check's findings are then only warnings.
func ForImage(image string) (c *Catalog, exact bool, why string) {
	all, err := Catalogs()
	if err != nil || len(all) == 0 {
		return nil, false, "the hub has no vLLM flag list"
	}
	newest := all[len(all)-1]
	if image == "" {
		return newest, false, "no image in the values (llmisvc's image is KServe's default): the vLLM version is not known"
	}
	for _, c := range all {
		if c.Image == image {
			return c, true, ""
		}
	}
	repo, tag := splitImage(image)
	segments := strings.Split(repo, "/")
	last := segments[len(segments)-1]
	vllmBuild := strings.HasPrefix(last, "vllm")
	for _, s := range segments[:len(segments)-1] {
		if strings.Contains(s, "lmcache") {
			vllmBuild = false
		}
	}
	m := vllmTag.FindStringSubmatch(tag)
	if !vllmBuild || m == nil {
		return newest, false, fmt.Sprintf("the vLLM version of %s is not known from its name", image)
	}
	for _, c := range all {
		if c.Version == m[1] {
			return c, true, ""
		}
	}
	return newest, false, fmt.Sprintf("the hub has no flag list for vLLM %s (hack/vllm-flags.sh writes one)", m[1])
}

// splitImage is an image's repository and tag; a digest has no tag.
func splitImage(image string) (repo, tag string) {
	if i := strings.Index(image, "@"); i >= 0 {
		return image[:i], ""
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		return image[:colon], image[colon+1:]
	}
	return image, ""
}

// versionLess orders X.Y.Z versions by number.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

// AtLeast says whether version is v or later.
func AtLeast(version, v string) bool { return !versionLess(version, v) }
