package parser

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/goccy/go-json"
	"github.com/icza/dyno"
	"gopkg.in/yaml.v3"
)

// Writing YAML configuration files back to disk.
//
// Replacements are applied with the same engine as JSON files (IterateOverJson)
// so wildcard matches, if_value and type handling behave identically. Rather
// than re-marshaling the whole document, which sorts every key and drops all
// comments and blank lines, the result is written back with as few changes to
// the original text as possible:
//
//  1. If no value changed, the file is left untouched.
//  2. If only scalar values changed, just those values are replaced in the
//     original text. Comments, blank lines, key order, indentation and quoting
//     are preserved exactly. The result is re-parsed and verified.
//  3. Otherwise (keys added, a value changed to a list or map, values inside
//     flow collections, ...) the changes are applied to the parsed node tree,
//     which keeps key order and comments, and the tree is re-encoded.

// updateYaml applies the configured replacements to the YAML document src. It
// returns the new document, and whether it differs from src.
func (f *ConfigurationFile) updateYaml(src []byte) ([]byte, bool, error) {
	orig, err := yamlToJsonValue(src)
	if err != nil {
		return nil, false, err
	}
	origJson, err := json.Marshal(orig)
	if err != nil {
		return nil, false, err
	}

	// Treat the data just like JSON and pass it to the iterator function to
	// update values as necessary.
	data, err := f.IterateOverJson(origJson)
	if err != nil {
		return nil, false, err
	}

	origValue, err := decodeJsonValue(origJson)
	if err != nil {
		return nil, false, err
	}
	next, err := decodeJsonValue(data)
	if err != nil {
		return nil, false, err
	}
	if reflect.DeepEqual(origValue, next) {
		return src, false, nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, false, err
	}
	if len(doc.Content) == 0 {
		// Empty document, there is nothing to preserve.
		out, err := yaml.Marshal(normalizeYamlTypes(next))
		return out, true, err
	}
	root := doc.Content[0]

	if out, ok := patchYamlScalars(src, root, origValue, next); ok {
		return out, true, nil
	}

	mergeYamlNode(root, origValue, next)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(detectYamlIndent(src))
	if err := enc.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	out := buf.Bytes()
	if bytes.Contains(src, []byte("\r\n")) {
		out = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
	}
	return out, true, nil
}

// yamlToJsonValue decodes the first YAML document in src into a value that can
// be marshaled as JSON, converting all map keys to strings.
func yamlToJsonValue(src []byte) (map[string]interface{}, error) {
	i := make(map[string]interface{})
	if err := yaml.Unmarshal(src, &i); err != nil {
		return nil, err
	}
	return dyno.ConvertMapI2MapS(i).(map[string]interface{}), nil
}

// decodeJsonValue decodes JSON data keeping numbers as json.Number, so values
// can be compared and written without losing precision.
func decodeJsonValue(data []byte) (interface{}, error) {
	var v interface{}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// yamlKey returns the key of a mapping entry as it appears in the JSON
// representation of the document (see dyno.ConvertMapI2MapS).
func yamlKey(n *yaml.Node) string {
	var k interface{}
	if err := n.Decode(&k); err != nil {
		return n.Value
	}
	return fmt.Sprint(k)
}

type yamlPatch struct {
	line       int // index into the lines of the document
	start, end int // rune offsets within the line
	text       string
}

// patchYamlScalars replaces the changed scalar values of the document in place.
// It returns false if a change cannot be applied this way, or the patched
// document does not decode to the expected value.
func patchYamlScalars(src []byte, root *yaml.Node, orig, next interface{}) ([]byte, bool) {
	lines := strings.Split(string(src), "\n")
	runes := make([][]rune, len(lines))
	for i, l := range lines {
		runes[i] = []rune(l)
	}

	var patches []yamlPatch
	if !collectYamlPatches(root, orig, next, false, runes, &patches) || len(patches) == 0 {
		return nil, false
	}

	// Apply from the end of the document so earlier offsets stay valid.
	sort.Slice(patches, func(i, j int) bool {
		if patches[i].line != patches[j].line {
			return patches[i].line > patches[j].line
		}
		return patches[i].start > patches[j].start
	})
	for _, p := range patches {
		l := runes[p.line]
		updated := make([]rune, 0, len(l)+len(p.text))
		updated = append(updated, l[:p.start]...)
		updated = append(updated, []rune(p.text)...)
		updated = append(updated, l[p.end:]...)
		runes[p.line] = updated
	}
	for i := range lines {
		lines[i] = string(runes[i])
	}
	out := []byte(strings.Join(lines, "\n"))

	// Make sure the patched document means exactly what was intended.
	got, err := yamlToJsonValue(out)
	if err != nil {
		return nil, false
	}
	gotJson, err := json.Marshal(got)
	if err != nil {
		return nil, false
	}
	gotValue, err := decodeJsonValue(gotJson)
	if err != nil || !reflect.DeepEqual(gotValue, next) {
		return nil, false
	}
	return out, true
}

func collectYamlPatches(n *yaml.Node, orig, next interface{}, inFlow bool, lines [][]rune, patches *[]yamlPatch) bool {
	if reflect.DeepEqual(orig, next) {
		return true
	}
	inFlow = inFlow || n.Style&yaml.FlowStyle != 0

	switch n.Kind {
	case yaml.MappingNode:
		nextMap, ok := next.(map[string]interface{})
		if !ok {
			return false
		}
		origMap, _ := orig.(map[string]interface{})
		seen := make(map[string]bool, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := yamlKey(n.Content[i])
			seen[k] = true
			nv, ok := nextMap[k]
			if !ok {
				continue
			}
			if !collectYamlPatches(n.Content[i+1], origMap[k], nv, inFlow, lines, patches) {
				return false
			}
		}
		// Keys that were added (or changed but only present through a merge key)
		// cannot be patched in place.
		for k, nv := range nextMap {
			if seen[k] {
				continue
			}
			if ov, ok := origMap[k]; !ok || !reflect.DeepEqual(ov, nv) {
				return false
			}
		}
		return true
	case yaml.SequenceNode:
		nextArr, ok := next.([]interface{})
		origArr, _ := orig.([]interface{})
		if !ok || len(nextArr) != len(n.Content) || len(origArr) != len(n.Content) {
			return false
		}
		for i, c := range n.Content {
			if !collectYamlPatches(c, origArr[i], nextArr[i], inFlow, lines, patches) {
				return false
			}
		}
		return true
	case yaml.ScalarNode:
		if inFlow {
			return false
		}
		p, ok := scalarPatch(n, next, lines)
		if !ok {
			return false
		}
		*patches = append(*patches, p)
		return true
	default:
		// Aliases (and anything else) are re-encoded instead.
		return false
	}
}

// scalarPatch returns a patch replacing the single-line scalar n in the source
// with the value v.
func scalarPatch(n *yaml.Node, v interface{}, lines [][]rune) (yamlPatch, bool) {
	line, start := n.Line-1, n.Column-1
	if line < 0 || line >= len(lines) || start < 0 || start >= len(lines[line]) || n.Anchor != "" {
		return yamlPatch{}, false
	}
	l := lines[line]

	var end int
	switch n.Style {
	case 0:
		// A plain scalar runs to a comment or the end of the line.
		end = len(l)
		for i := start; i < len(l); i++ {
			if l[i] == '#' && i > start && (l[i-1] == ' ' || l[i-1] == '\t') {
				end = i
				break
			}
		}
		for end > start && (l[end-1] == ' ' || l[end-1] == '\t' || l[end-1] == '\r') {
			end--
		}
		// Only a single-line plain scalar without a tag reads back as its value.
		if string(l[start:end]) != n.Value {
			return yamlPatch{}, false
		}
	case yaml.DoubleQuotedStyle:
		if l[start] != '"' {
			return yamlPatch{}, false
		}
		end = -1
		for i := start + 1; i < len(l); i++ {
			if l[i] == '\\' {
				i++
				continue
			}
			if l[i] == '"' {
				end = i + 1
				break
			}
		}
		if end < 0 {
			return yamlPatch{}, false
		}
	case yaml.SingleQuotedStyle:
		if l[start] != '\'' {
			return yamlPatch{}, false
		}
		end = -1
		for i := start + 1; i < len(l); i++ {
			if l[i] == '\'' {
				if i+1 < len(l) && l[i+1] == '\'' {
					i++
					continue
				}
				end = i + 1
				break
			}
		}
		if end < 0 {
			return yamlPatch{}, false
		}
	default:
		// Literal and folded block scalars span multiple lines.
		return yamlPatch{}, false
	}

	text, ok := renderYamlScalar(n, v)
	if !ok {
		return yamlPatch{}, false
	}
	return yamlPatch{line: line, start: start, end: end, text: text}, true
}

// renderYamlScalar renders v as a single-line YAML scalar, keeping the quoting
// style of n when both are strings.
func renderYamlScalar(n *yaml.Node, v interface{}) (string, bool) {
	s := &yaml.Node{Kind: yaml.ScalarNode}
	switch typed := v.(type) {
	case string:
		s.Tag, s.Value = "!!str", typed
		if n.ShortTag() == "!!str" {
			s.Style = n.Style
		}
	case bool:
		s.Tag, s.Value = "!!bool", fmt.Sprint(typed)
	case json.Number:
		s.Tag, s.Value = "!!int", typed.String()
		if strings.ContainsAny(typed.String(), ".eE") {
			s.Tag = "!!float"
		}
	case nil:
		s.Tag, s.Value = "!!null", "null"
	default:
		return "", false
	}
	out, err := yaml.Marshal(s)
	if err != nil {
		return "", false
	}
	text := strings.TrimSuffix(string(out), "\n")
	if strings.Contains(text, "\n") {
		return "", false
	}
	return text, true
}

// mergeYamlNode applies the differences between orig and next to the node tree,
// keeping key order and comments.
func mergeYamlNode(n *yaml.Node, orig, next interface{}) {
	if reflect.DeepEqual(orig, next) {
		return
	}

	switch nextTyped := next.(type) {
	case map[string]interface{}:
		if n.Kind == yaml.MappingNode {
			origMap, _ := orig.(map[string]interface{})
			seen := make(map[string]bool, len(n.Content)/2)
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := yamlKey(n.Content[i])
				seen[k] = true
				if nv, ok := nextTyped[k]; ok {
					mergeYamlNode(n.Content[i+1], origMap[k], nv)
				}
			}
			var added []string
			for k, nv := range nextTyped {
				if seen[k] {
					continue
				}
				if ov, ok := origMap[k]; !ok || !reflect.DeepEqual(ov, nv) {
					added = append(added, k)
				}
			}
			sort.Strings(added)
			for _, k := range added {
				n.Content = append(n.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
					buildYamlNode(nextTyped[k]))
			}
			return
		}
	case []interface{}:
		if n.Kind == yaml.SequenceNode {
			origArr, _ := orig.([]interface{})
			for i, nv := range nextTyped {
				if i < len(n.Content) {
					var ov interface{}
					if i < len(origArr) {
						ov = origArr[i]
					}
					mergeYamlNode(n.Content[i], ov, nv)
				} else {
					n.Content = append(n.Content, buildYamlNode(nv))
				}
			}
			if len(n.Content) > len(nextTyped) {
				n.Content = n.Content[:len(nextTyped)]
			}
			return
		}
	}

	// Replace the node, keeping its comments and, for strings, its style.
	replacement := buildYamlNode(next)
	replacement.HeadComment, replacement.LineComment, replacement.FootComment = n.HeadComment, n.LineComment, n.FootComment
	if n.Kind == yaml.ScalarNode && replacement.Kind == yaml.ScalarNode &&
		n.ShortTag() == "!!str" && replacement.ShortTag() == "!!str" {
		replacement.Style = n.Style
	}
	*n = *replacement
}

func buildYamlNode(v interface{}) *yaml.Node {
	n := &yaml.Node{}
	if err := n.Encode(normalizeYamlTypes(v)); err != nil {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprint(v)}
	}
	return n
}

// detectYamlIndent returns the indentation width used by the document, falling
// back to two spaces.
func detectYamlIndent(src []byte) int {
	indent := 0
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.TrimSpace(trimmed) == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if n := len(line) - len(trimmed); n > 0 && (indent == 0 || n < indent) {
			indent = n
		}
	}
	if indent < 2 || indent > 8 {
		return 2
	}
	return indent
}
