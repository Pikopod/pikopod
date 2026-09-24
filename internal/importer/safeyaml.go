package importer

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pikopod/pikopod/internal/ir"
)

func parseYAMLSafely(text string, limits ParseLimits) (any, error) {
	doc, _, err := parseYAMLWithPositions(text, limits, false)
	return doc, err
}

func parseYAMLWithPositions(text string, limits ParseLimits, track bool) (any, Positions, error) {
	if len(text) > limits.MaxDocumentBytes {
		return nil, nil, specErr(SpecTooLarge, "document exceeds the size limit")
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return nil, nil, specErr(SpecParseError, fmt.Sprintf("YAML parse failed: %v", err))
	}
	c := &yamlConverter{limits: limits}
	if track {
		c.positions = Positions{}
	}
	var doc any
	if root.Kind != 0 {
		node := &root
		if root.Kind == yaml.DocumentNode {
			if len(root.Content) == 0 {
				return nil, nil, enforceStructuralLimits(nil, limits)
			}
			node = root.Content[0]
		}
		var err error
		doc, err = c.convertAt(node, "#")
		if err != nil {
			return nil, nil, err
		}
	}
	if err := enforceStructuralLimits(doc, limits); err != nil {
		return nil, nil, err
	}
	return doc, c.positions, nil
}

type Positions map[string]ir.Position

type yamlConverter struct {
	limits    ParseLimits
	aliases   int
	nodes     int
	depth     int
	positions Positions
}

func (c *yamlConverter) convert(node *yaml.Node) (any, error) {
	return c.convertAt(node, "")
}

func (c *yamlConverter) convertAt(node *yaml.Node, pointer string) (any, error) {
	if c.positions != nil && pointer != "" {
		if _, set := c.positions[pointer]; !set {
			c.positions[pointer] = ir.Position{Line: node.Line, Col: node.Column}
		}
	}
	c.nodes++
	if c.limits.MaxNodes > 0 && c.nodes > c.limits.MaxNodes {
		return nil, specErr(SpecTooLarge, "spec exceeds the node budget during YAML conversion")
	}
	c.depth++
	defer func() { c.depth-- }()
	if c.limits.MaxDepth > 0 && c.depth > c.limits.MaxDepth {
		return nil, specErr(SpecTooLarge, "spec exceeds the nesting depth budget")
	}
	switch node.Kind {
	case yaml.AliasNode:
		c.aliases++
		if c.aliases > c.limits.MaxAliasExpansions {
			return nil, specErr(SpecParseError, "YAML parse failed: too many alias expansions")
		}
		return c.convertAt(node.Alias, pointer)
	case yaml.MappingNode:
		out := NewOrdMap()
		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode, valueNode := node.Content[i], node.Content[i+1]
			var key string
			if keyNode.Kind == yaml.ScalarNode {
				kv, err := c.scalar(keyNode)
				if err != nil {
					return nil, err
				}
				key = scalarKeyString(kv)
			} else {

				return nil, specErr(SpecParseError, "YAML parse failed: non-scalar mapping key")
			}
			if key == "<<" {

				key = "<<"
			}
			if out.Has(key) {
				return nil, specErr(SpecParseError, fmt.Sprintf("YAML parse failed: duplicate key %q", key))
			}
			childPtr := ""
			if pointer != "" {
				childPtr = pointer + "/" + jpescape(key)
				if c.positions != nil {
					c.positions[childPtr] = ir.Position{Line: keyNode.Line, Col: keyNode.Column}
				}
			}
			value, err := c.convertAt(valueNode, childPtr)
			if err != nil {
				return nil, err
			}
			out.Set(key, value)
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(node.Content))
		for i, item := range node.Content {
			childPtr := ""
			if pointer != "" {
				childPtr = pointer + "/" + strconv.Itoa(i)
			}
			v, err := c.convertAt(item, childPtr)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		return c.scalar(node)
	default:
		return nil, specErr(SpecParseError, "YAML parse failed: unsupported node kind")
	}
}

func (c *yamlConverter) scalar(node *yaml.Node) (any, error) {
	v := node.Value
	if node.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle|yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return v, nil
	}
	if node.Tag == "!!str" {
		return v, nil
	}
	switch v {
	case "null", "Null", "NULL", "~", "":
		return nil, nil
	case "true", "True", "TRUE":
		return true, nil
	case "false", "False", "FALSE":
		return false, nil
	}
	if n, ok := coreNumber(v); ok {
		return n, nil
	}
	return v, nil
}

func coreNumber(v string) (float64, bool) {
	s := v
	neg := false
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	} else if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	switch s {
	case ".inf", ".Inf", ".INF":
		if neg {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	case ".nan", ".NaN", ".NAN":
		return math.NaN(), true
	}
	if strings.HasPrefix(s, "0x") {
		if n, err := strconv.ParseUint(s[2:], 16, 64); err == nil {
			f := float64(n)
			if neg {
				f = -f
			}
			return f, true
		}
		return 0, false
	}
	if strings.HasPrefix(s, "0o") {
		if n, err := strconv.ParseUint(s[2:], 8, 64); err == nil {
			f := float64(n)
			if neg {
				f = -f
			}
			return f, true
		}
		return 0, false
	}
	if s == "" || !isCoreNumericShape(s) {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func isCoreNumericShape(s string) bool {
	seenDigit := false
	i := 0
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		seenDigit = true
	}
	if i < len(s) && s[i] == '.' {
		i++
		for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
			seenDigit = true
		}
	}
	if !seenDigit {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if i >= len(s) {
			return false
		}
		for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		}
	}
	return i == len(s)
}

func scalarKeyString(v any) string {
	switch k := v.(type) {
	case nil:
		return "null"
	case bool:
		if k {
			return "true"
		}
		return "false"
	case float64:
		return ir.FormatJSNumber(k)
	case string:
		return k
	default:
		return fmt.Sprint(k)
	}
}

func enforceStructuralLimits(root any, limits ParseLimits) error {
	nodes := 0
	var walk func(value any, depth int) error
	walk = func(value any, depth int) error {
		if depth > limits.MaxDepth {
			return specErr(SpecDepthExceeded, fmt.Sprintf("nesting exceeds %d", limits.MaxDepth))
		}
		nodes++
		if nodes > limits.MaxNodes {
			return specErr(SpecTooLarge, fmt.Sprintf("document exceeds %d nodes", limits.MaxNodes))
		}
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				if err := walk(item, depth+1); err != nil {
					return err
				}
			}
		case *OrdMap:
			for _, k := range v.keys {
				if err := walk(v.values[k], depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(root, 0)
}
