package importer

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pikopod/pikopod/internal/ir"
)

// parseYAMLSafely loads core-schema YAML only: no custom tags, bounded alias
// expansion, duplicate keys a hard error, no YAML 1.1 leftovers from yaml.v3.
func parseYAMLSafely(text string, limits ParseLimits) (any, error) {
	if len(text) > limits.MaxDocumentBytes {
		return nil, specErr(SpecTooLarge, "document exceeds the size limit")
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return nil, specErr(SpecParseError, fmt.Sprintf("YAML parse failed: %v", err))
	}
	c := &yamlConverter{limits: limits}
	var doc any
	if root.Kind != 0 { // zero Kind means an empty document
		node := &root
		if root.Kind == yaml.DocumentNode {
			if len(root.Content) == 0 {
				return nil, enforceStructuralLimits(nil, limits)
			}
			node = root.Content[0]
		}
		var err error
		doc, err = c.convert(node)
		if err != nil {
			return nil, err
		}
	}
	if err := enforceStructuralLimits(doc, limits); err != nil {
		return nil, err
	}
	return doc, nil
}

type yamlConverter struct {
	limits  ParseLimits
	aliases int
	nodes   int
	depth   int
}

// convert enforces node/depth budgets DURING conversion: an alias-amplified
// subtree must fail before it allocates, not once the multi-GB tree exists.
func (c *yamlConverter) convert(node *yaml.Node) (any, error) {
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
		return c.convert(node.Alias)
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
				// Complex keys have no JS-object analogue; reject like the
				// reference's strict mapping handling would.
				return nil, specErr(SpecParseError, "YAML parse failed: non-scalar mapping key")
			}
			if key == "<<" {
				// Merge keys are YAML 1.1; the core schema treats them as
				// ordinary keys, so do the same (yaml.v3 would merge).
				key = "<<"
			}
			if out.Has(key) {
				return nil, specErr(SpecParseError, fmt.Sprintf("YAML parse failed: duplicate key %q", key))
			}
			value, err := c.convert(valueNode)
			if err != nil {
				return nil, err
			}
			out.Set(key, value)
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(node.Content))
		for _, item := range node.Content {
			v, err := c.convert(item)
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

// scalar resolves a scalar node against the YAML 1.2 core schema.
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

// coreNumber parses YAML 1.2 core-schema numbers: decimal ints, 0o octal,
// 0x hex, floats, and .inf/.nan forms.
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

// isCoreNumericShape rejects strings ParseFloat would accept but the core
// schema would not (e.g. "1_000", "0x" handled above, "Inf").
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

// scalarKeyString renders a scalar as a JS object key.
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
