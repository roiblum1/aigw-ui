// Package vllmflags checks the arguments of `vllm serve` before they reach a
// pod. KServe hands the arguments to vLLM as they are, and vLLM refuses a
// flag it does not have only when the pod starts. The flags of each vLLM
// version are what `vllm serve --help=all` prints in that version's image
// (hack/vllm-flags.sh writes catalogs/vllm-<version>.txt). The check knows
// what the help shows: each flag's spellings, whether it takes a value, and
// a value's choices. It does not know value types, the keys of JSON
// configs, or the combinations vLLM refuses when it starts.
package vllmflags

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed catalogs/*.txt
var catalogFiles embed.FS

// Catalog is the flags of one vLLM version's `vllm serve`.
type Catalog struct {
	Version string
	// Image is the image the help was read from.
	Image string
	Flags []*Flag

	options map[string]*Flag
	names   []string // every spelling, sorted
}

// Value kinds, from the metavar the help shows after a flag.
const (
	NoValue   = ""         // --enforce-eager, or a --x / --no-x pair
	OneValue  = "one"      // --port PORT, --dtype {auto,half}
	Optional  = "optional" // --hf-token [HF_TOKEN]
	OneOrMore = "many"     // --api-key API_KEY [API_KEY ...]
	AnyNumber = "any"      // --x [X ...]
)

// Flag is one option of `vllm serve`, as one line of its help.
type Flag struct {
	// Options are its spellings: --tensor-parallel-size, -tp; a pair is
	// --x and --no-x.
	Options []string
	// Dest is the long name with underscores: tensor_parallel_size.
	Dest  string
	Value string
	// Choices are the values argparse takes, when the help shows them as
	// {a,b,c}. A list such as ['auto', 'hf'] only names the known ones.
	Choices []string
}

// Name is the flag's main spelling.
func (f *Flag) Name() string { return f.Options[0] }

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

var fileVersion = regexp.MustCompile(`^vllm-(\d+\.\d+\.\d+)\.txt$`)

func load(files fs.FS) ([]*Catalog, error) {
	paths, err := fs.Glob(files, "catalogs/*.txt")
	if err != nil {
		return nil, err
	}
	var out []*Catalog
	for _, p := range paths {
		m := fileVersion.FindStringSubmatch(path.Base(p))
		if m == nil {
			return nil, fmt.Errorf("%s: not named vllm-<version>.txt", p)
		}
		raw, err := fs.ReadFile(files, p)
		if err != nil {
			return nil, err
		}
		c, err := ParseHelp(m[1], string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i].Version, out[j].Version) })
	return out, nil
}

var (
	choiceList    = regexp.MustCompile(`^\{([^{} ]*)\}`)
	optionalValue = regexp.MustCompile(`^\[[A-Z0-9_]+\]$`)
)

// ParseHelp reads what hack/vllm-flags.sh writes: a "# image:" line, then
// `vllm serve --help=all`. A flag's line starts with two spaces and a dash;
// what follows two more spaces is its help text.
func ParseHelp(version, help string) (*Catalog, error) {
	c := &Catalog{Version: version, options: map[string]*Flag{}}
	for _, line := range strings.Split(help, "\n") {
		if img, ok := strings.CutPrefix(line, "# image: "); ok {
			c.Image = strings.TrimSpace(img)
			continue
		}
		if !strings.HasPrefix(line, "  -") {
			continue
		}
		usage := strings.TrimSpace(line)
		if i := strings.Index(usage, "  "); i >= 0 {
			usage = usage[:i]
		}
		parts := strings.Split(usage, ", -")
		for i := 1; i < len(parts); i++ {
			parts[i] = "-" + parts[i]
		}
		f := &Flag{}
		for i, p := range parts {
			name, metavar, _ := strings.Cut(p, " ")
			f.Options = append(f.Options, name)
			if i == 0 {
				f.Value, f.Choices = valueOf(metavar)
			}
		}
		if f.Name() == "-h" || f.Name() == "--help" {
			continue // it prints the help and exits: not for a pod
		}
		for _, o := range f.Options {
			if strings.HasPrefix(o, "--") && f.Dest == "" {
				f.Dest = strings.ReplaceAll(strings.TrimPrefix(o, "--"), "-", "_")
			}
			c.options[o] = f
			c.names = append(c.names, o)
		}
		c.Flags = append(c.Flags, f)
	}
	if len(c.Flags) == 0 || c.options["--port"] == nil {
		return nil, fmt.Errorf("not the help of vllm serve --help=all: no --port")
	}
	sort.Strings(c.names)
	return c, nil
}

// valueOf reads a metavar: none, [X], X [X ...], [X ...], {a,b} or X.
func valueOf(metavar string) (kind string, choices []string) {
	if m := choiceList.FindStringSubmatch(metavar); m != nil && (metavar == m[0] || strings.HasPrefix(metavar, m[0]+" [")) {
		choices = strings.Split(m[1], ",")
	}
	switch {
	case metavar == "":
		return NoValue, nil
	case strings.HasPrefix(metavar, "[") && strings.HasSuffix(metavar, " ...]"):
		return AnyNumber, choices
	case strings.HasSuffix(metavar, " ...]"):
		return OneOrMore, choices
	case optionalValue.MatchString(metavar):
		return Optional, nil
	}
	return OneValue, choices
}

// Flag is the flag spelled exactly so, or nil.
func (c *Catalog) Flag(option string) *Flag { return c.options[option] }

// FlagOf is the flag that sets dest, or nil.
func (c *Catalog) FlagOf(dest string) *Flag {
	return c.options["--"+strings.ReplaceAll(dest, "_", "-")]
}

var vllmTag = regexp.MustCompile(`^v(\d+\.\d+\.\d+)(-[A-Za-z0-9_.-]+)?$`)

// ForImage is the list for an image's vLLM. exact says it is the image's
// own version: the image is the one a list was read from, or a vllm build
// (a repository named vllm*, not under lmcache/) tagged vX.Y.Z with a list
// for X.Y.Z. Otherwise it is the newest list, and why says what is not
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
