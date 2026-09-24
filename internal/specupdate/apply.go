package specupdate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type Result struct {
	Applied     []Patch `json:"applied"`
	Suggestions []Patch `json:"suggestions"`

	Skipped []Patch `json:"skipped"`
	Out     []byte  `json:"-"`
	JSON    bool    `json:"-"`
}

const maxApplyBytes = 32 << 20

func Apply(specRaw []byte, changes []Change) (*Result, error) {
	if len(specRaw) > maxApplyBytes {
		return nil, fmt.Errorf("spec document exceeds the %d MiB cap", maxApplyBytes>>20)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(specRaw, &doc); err != nil {
		return nil, fmt.Errorf("spec does not parse: %w", err)
	}
	root := docRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("spec root is not a mapping")
	}
	res := &Result{JSON: looksJSON(specRaw)}
	ap := &applier{root: root}
	for _, c := range changes {
		if !c.Kind.Additive() {
			res.Suggestions = append(res.Suggestions, Patch{Change: c})
			continue
		}
		if patch, ok := ap.apply(c); ok {
			res.Applied = append(res.Applied, patch)
		} else {
			res.Skipped = append(res.Skipped, Patch{Change: c})
		}
	}
	var buf bytes.Buffer
	if res.JSON {
		writeJSON(&buf, root, 0)
		buf.WriteByte('\n')
	} else {
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(&doc); err != nil {
			return nil, err
		}
		enc.Close()
	}
	res.Out = buf.Bytes()
	return res, nil
}

type applier struct {
	root *yaml.Node
}

func (a *applier) apply(c Change) (Patch, bool) {
	op := a.operationNode(c.Template, c.Method)
	if op == nil {
		return Patch{}, false
	}
	opPath := "/paths/" + esc(c.Template) + "/" + strings.ToLower(c.Method)

	responses := mapGet(op, "responses")
	if responses == nil || responses.Kind != yaml.MappingNode {
		return Patch{}, false
	}

	if c.Kind == AddStatus {
		code := fmt.Sprint(c.Status)
		if mapGet(responses, code) != nil {
			return Patch{}, false
		}
		val := mapping(
			"description", str("Observed in traffic; added by pikopod spec-update."),
			"x-pikopod-observed", evidenceNode(c.Evidence),
		)
		mapSet(responses, code, val)
		return Patch{Change: c, Ops: []Op{{Op: "add", Path: opPath + "/responses/" + code,
			Value: map[string]any{"description": "Observed in traffic; added by pikopod spec-update.",
				"x-pikopod-observed": evidenceValue(c.Evidence)}}}}, true
	}

	respKey, resp := a.findResponse(responses, c.Status, c.StatusClass)
	if resp == nil {
		return Patch{}, false
	}
	content := mapGet(resp, "content")
	schema, schemaPath := (*yaml.Node)(nil), ""
	if content != nil && content.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(content.Content); i += 2 {
			if strings.Contains(strings.ToLower(content.Content[i].Value), "json") {
				schema = mapGet(content.Content[i+1], "schema")
				schemaPath = opPath + "/responses/" + esc(respKey) + "/content/" + esc(content.Content[i].Value) + "/schema"
				break
			}
		}
	} else if s := mapGet(resp, "schema"); s != nil {
		schema, schemaPath = s, opPath+"/responses/"+esc(respKey)+"/schema"
	}
	if schema == nil {
		return Patch{}, false
	}

	target, targetPath, ok := a.navigate(schema, schemaPath, c.Pointer)
	if !ok {
		return Patch{}, false
	}

	switch c.Kind {
	case EnumUnion:
		enum := mapGet(target, "enum")
		if enum == nil || enum.Kind != yaml.SequenceNode {
			return Patch{}, false
		}
		for _, v := range enum.Content {
			if v.Value == c.Value {
				return Patch{}, false
			}
		}
		enum.Content = append(enum.Content, str(c.Value))
		annotate(target, c.Evidence)
		return Patch{Change: c, Ops: []Op{{Op: "add", Path: targetPath + "/enum/-", Value: c.Value}}}, true

	case NullableWrap:
		if n := mapGet(target, "nullable"); n != nil {
			if n.Value == "true" {
				return Patch{}, false
			}
			n.Value, n.Tag = "true", "!!bool"
			annotate(target, c.Evidence)
			return Patch{Change: c, Ops: []Op{{Op: "replace", Path: targetPath + "/nullable", Value: true}}}, true
		}
		mapSet(target, "nullable", boolean(true))
		annotate(target, c.Evidence)
		return Patch{Change: c, Ops: []Op{{Op: "add", Path: targetPath + "/nullable", Value: true}}}, true

	case AddProperty:
		props := mapGet(target, "properties")
		if props == nil {

			if t := mapGet(target, "type"); t == nil || t.Value != "object" {
				return Patch{}, false
			}
			props = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			mapSet(target, "properties", props)
		}
		if props.Kind != yaml.MappingNode || mapGet(props, c.Value) != nil {
			return Patch{}, false
		}
		propVal := mapping()
		if t := openapiType(c.PropType); t != "" {
			mapSet(propVal, "type", str(t))
		}
		mapSet(propVal, "x-pikopod-observed", evidenceNode(c.Evidence))
		mapSet(props, c.Value, propVal)
		opValue := map[string]any{"x-pikopod-observed": evidenceValue(c.Evidence)}
		if t := openapiType(c.PropType); t != "" {
			opValue["type"] = t
		}
		return Patch{Change: c, Ops: []Op{{Op: "add", Path: targetPath + "/properties/" + esc(c.Value), Value: opValue}}}, true
	}
	return Patch{}, false
}

func (a *applier) operationNode(template, method string) *yaml.Node {
	paths := mapGet(a.root, "paths")
	if paths == nil {
		return nil
	}
	item := mapGet(paths, template)
	if item == nil {
		return nil
	}
	item = a.deref(item)
	return mapGet(item, strings.ToLower(method))
}

func (a *applier) findResponse(responses *yaml.Node, status int, class string) (string, *yaml.Node) {
	if status > 0 {

		code := fmt.Sprint(status)
		if n := mapGet(responses, code); n != nil {
			return code, a.deref(n)
		}
		class = code[:1] + "xx"
	} else if len(class) == 3 {

		for i := 0; i+1 < len(responses.Content); i += 2 {
			k := responses.Content[i].Value
			if len(k) == 3 && k[0] == class[0] && k[1] >= '0' && k[1] <= '9' {
				return k, a.deref(responses.Content[i+1])
			}
		}
	}
	if len(class) == 3 {
		for i := 0; i+1 < len(responses.Content); i += 2 {
			if strings.EqualFold(responses.Content[i].Value, class[:1]+"XX") {
				return responses.Content[i].Value, a.deref(responses.Content[i+1])
			}
		}
	}
	if n := mapGet(responses, "default"); n != nil {
		return "default", a.deref(n)
	}
	return "", nil
}

func (a *applier) navigate(schema *yaml.Node, path, pointer string) (*yaml.Node, string, bool) {
	node := schema
	for hops := 0; ; hops++ {
		if hops > 32 || node == nil {
			return nil, "", false
		}
		ref := mapGet(node, "$ref")
		if ref == nil {
			break
		}
		target, refPath := a.resolveRef(ref.Value)
		if target == nil {
			return nil, "", false
		}
		node, path = target, refPath
	}
	if pointer == "" || pointer == "/" {
		return node, path, true
	}
	for _, seg := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		for hops := 0; ; hops++ {
			if hops > 32 || node == nil {
				return nil, "", false
			}
			ref := mapGet(node, "$ref")
			if ref == nil {
				break
			}
			target, refPath := a.resolveRef(ref.Value)
			if target == nil {
				return nil, "", false
			}
			node, path = target, refPath
		}
		if seg == "*" {
			items := mapGet(node, "items")
			if items == nil {
				return nil, "", false
			}
			node, path = items, path+"/items"
			continue
		}
		props := mapGet(node, "properties")
		if props == nil {
			return nil, "", false
		}
		child := mapGet(props, seg)
		if child == nil {
			return nil, "", false
		}
		node, path = child, path+"/properties/"+esc(seg)
	}

	for hops := 0; ; hops++ {
		if hops > 32 || node == nil {
			return nil, "", false
		}
		ref := mapGet(node, "$ref")
		if ref == nil {
			break
		}
		target, refPath := a.resolveRef(ref.Value)
		if target == nil {
			return nil, "", false
		}
		node, path = target, refPath
	}
	return node, path, true
}

func (a *applier) resolveRef(ref string) (*yaml.Node, string) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, ""
	}
	node := a.root
	path := ""
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		node = mapGet(node, seg)
		if node == nil {
			return nil, ""
		}
		path += "/" + esc(seg)
	}
	return node, path
}

func (a *applier) deref(n *yaml.Node) *yaml.Node {
	for hops := 0; n != nil && hops < 32; hops++ {
		ref := mapGet(n, "$ref")
		if ref == nil {
			return n
		}
		n, _ = a.resolveRef(ref.Value)
	}
	return n
}

func annotate(schema *yaml.Node, ev Evidence) {
	if existing := mapGet(schema, "x-pikopod-observed"); existing != nil {
		*existing = *evidenceNode(ev)
		return
	}
	mapSet(schema, "x-pikopod-observed", evidenceNode(ev))
}

func evidenceNode(ev Evidence) *yaml.Node {
	m := mapping()
	if ev.Occurrences > 0 {
		mapSet(m, "occurrences", intNode(ev.Occurrences))
	}
	if ev.Presence > 0 {
		mapSet(m, "presence", floatNode(ev.Presence))
	}
	if ev.Since != "" {
		mapSet(m, "since", str(ev.Since))
	}
	return m
}

func evidenceValue(ev Evidence) map[string]any {
	out := map[string]any{}
	if ev.Occurrences > 0 {
		out["occurrences"] = ev.Occurrences
	}
	if ev.Presence > 0 {
		out["presence"] = ev.Presence
	}
	if ev.Since != "" {
		out["since"] = ev.Since
	}
	return out
}

func openapiType(t string) string {
	switch t {
	case "string", "number", "boolean", "object", "array", "integer":
		return t
	default:
		return ""
	}
}

func docRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil {
		return nil
	}
	if m.Kind == yaml.AliasNode {
		m = m.Alias
	}
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content, str(key), val)
}

func mapping(kv ...any) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		mapSet(m, kv[i].(string), kv[i+1].(*yaml.Node))
	}
	return m
}

func str(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func boolean(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(v)}
}

func intNode(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(v)}
}

func floatNode(v float64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".")}
}

func esc(seg string) string {
	return strings.ReplaceAll(strings.ReplaceAll(seg, "~", "~0"), "/", "~1")
}

func looksJSON(raw []byte) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

func writeJSON(buf *bytes.Buffer, n *yaml.Node, indent int) {
	if n == nil {
		buf.WriteString("null")
		return
	}
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	pad := strings.Repeat("  ", indent)
	switch n.Kind {
	case yaml.MappingNode:
		if len(n.Content) == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteString("{\n")
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				buf.WriteString(",\n")
			}
			buf.WriteString(pad + "  ")
			key, _ := json.Marshal(n.Content[i].Value)
			buf.Write(key)
			buf.WriteString(": ")
			writeJSON(buf, n.Content[i+1], indent+1)
		}
		buf.WriteString("\n" + pad + "}")
	case yaml.SequenceNode:
		if len(n.Content) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteString("[\n")
		for i, c := range n.Content {
			if i > 0 {
				buf.WriteString(",\n")
			}
			buf.WriteString(pad + "  ")
			writeJSON(buf, c, indent+1)
		}
		buf.WriteString("\n" + pad + "]")
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!int", "!!float":
			buf.WriteString(n.Value)
		case "!!bool":
			buf.WriteString(n.Value)
		case "!!null":
			buf.WriteString("null")
		default:
			enc, _ := json.Marshal(n.Value)
			buf.Write(enc)
		}
	default:
		buf.WriteString("null")
	}
}
