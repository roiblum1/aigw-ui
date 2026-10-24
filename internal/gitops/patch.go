package gitops

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
)

// Patch rewrites a values file so that it reads as want, touching only
// what differs: the comments and the order of everything that stays are
// kept, so a recipe that documents itself (operators/glm53/values.yaml)
// keeps its documentation through an edit on the page. New keys go at the
// end of their map in alphabetical order. A file that holds no values yet
// gets header, then want.
func Patch(original []byte, header string, want map[string]any) ([]byte, error) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(original, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yamlv3.MappingNode {
		return encode(header, want)
	}
	p := patcher{}
	if len(want) == 0 {
		// Every value gone: the file keeps its comments and holds {}.
		doc.Content[0].Content = nil
		doc.Content[0].Style = yamlv3.FlowStyle
		p.structural = true
	} else if err := p.patchMap(doc.Content[0], want); err != nil {
		return nil, err
	}
	if !p.structural {
		// Only plain values changed: change them where they stand, so the
		// file keeps its blank lines, flow maps and comment columns too.
		if out, ok := p.inPlace(original); ok {
			return out, nil
		}
	}
	var out bytes.Buffer
	enc := yamlv3.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// patcher makes a node tree hold new values. edits are the plain values it
// replaced; structural is set by any other change (a key added or removed,
// a list or map replaced), after which only a full re-encode is right.
type patcher struct {
	edits      []edit
	structural bool
}

// edit is one plain value replaced: where its text starts and what it was.
type edit struct {
	line, column int
	old, new     string
}

// patchMap makes the mapping node n hold want.
func (p *patcher) patchMap(n *yamlv3.Node, want map[string]any) error {
	seen := make(map[string]bool, len(want))
	var content []*yamlv3.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		w, ok := want[k.Value]
		if !ok {
			p.structural = true
			continue
		}
		seen[k.Value] = true
		if wm, isMap := w.(map[string]any); isMap && v.Kind == yamlv3.MappingNode && len(wm) > 0 {
			if err := p.patchMap(v, wm); err != nil {
				return err
			}
		} else if !sameValue(v, w) {
			nv, err := valueNode(w)
			if err != nil {
				return err
			}
			if v.Kind == yamlv3.ScalarNode && v.Style == 0 && nv.Kind == yamlv3.ScalarNode && !strings.Contains(nv.Value, "\n") {
				text, err := yamlv3.Marshal(w)
				if err != nil {
					return err
				}
				p.edits = append(p.edits, edit{line: v.Line, column: v.Column, old: v.Value, new: strings.TrimSuffix(string(text), "\n")})
			} else {
				p.structural = true
			}
			nv.LineComment = v.LineComment
			v = nv
		}
		content = append(content, k, v)
	}
	var added []string
	for k := range want {
		if !seen[k] {
			added = append(added, k)
		}
	}
	sort.Strings(added)
	if len(added) > 0 {
		p.structural = true
	}
	for _, k := range added {
		nv, err := valueNode(want[k])
		if err != nil {
			return err
		}
		content = append(content, &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: k}, nv)
	}
	if len(n.Content) == 0 && len(content) > 0 && n.Style == yamlv3.FlowStyle {
		// {} that gets values: written as a block.
		n.Style = 0
	}
	n.Content = content
	return nil
}

// inPlace applies the edits to the original text, or reports that one of
// them is not where the parser said (a value spread over lines).
func (p *patcher) inPlace(original []byte) ([]byte, bool) {
	lines := strings.Split(string(original), "\n")
	// Right to left on a line, so an earlier column is still where it was.
	sort.Slice(p.edits, func(i, j int) bool {
		if p.edits[i].line != p.edits[j].line {
			return p.edits[i].line < p.edits[j].line
		}
		return p.edits[i].column > p.edits[j].column
	})
	for _, e := range p.edits {
		if e.line < 1 || e.line > len(lines) {
			return nil, false
		}
		line := lines[e.line-1]
		start := e.column - 1
		if start < 0 || start+len(e.old) > len(line) || line[start:start+len(e.old)] != e.old {
			return nil, false
		}
		lines[e.line-1] = line[:start] + e.new + line[start+len(e.old):]
	}
	return []byte(strings.Join(lines, "\n")), true
}

// valueNode is v as a YAML node, maps with their keys in order.
func valueNode(v any) (*yamlv3.Node, error) {
	var n yamlv3.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return &n, nil
}

// sameValue says whether the node already holds v. Both go through JSON, so
// 8192 and 8192.0 are the same and so are a map's two orders.
func sameValue(n *yamlv3.Node, v any) bool {
	var cur any
	if err := n.Decode(&cur); err != nil {
		return false
	}
	return reflect.DeepEqual(normal(cur), normal(v))
}

func normal(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(raw, &out) != nil {
		return v
	}
	return out
}
