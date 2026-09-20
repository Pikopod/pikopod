package agent

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sanitize"
)

// sanitizeRules derives narrowly scoped raw-value allowances from trusted
// contract enums. Heuristic and model-extracted enums never relax redaction.
func sanitizeRules(def *ir.ApiDefinition) []sanitize.Rule {
	if def == nil {
		return nil
	}
	type ruleKey struct {
		field string
		value string
	}
	seen := map[ruleKey]bool{}
	var out []sanitize.Rule

	var walk func(*ir.IrSchemaNode, string)
	walk = func(node *ir.IrSchemaNode, field string) {
		if node == nil {
			return
		}
		if field != "" && node.EnumValues != nil && trusted(node.EnumValues.Provenance) {
			for _, value := range node.EnumValues.Value {
				key := ruleKey{field: field, value: enumKey(value)}
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, sanitize.Rule{
					Field: field, Mode: sanitize.ModeAllow,
					AllowedValues: []any{value},
				})
			}
		}
		for i := range node.Properties {
			prop := &node.Properties[i]
			walk(&prop.Schema, prop.Name)
		}
		if node.Items != nil {
			walk(node.Items, field)
		}
	}

	for i := range def.Schemas {
		walk(&def.Schemas[i].Schema, "")
	}
	for i := range def.Endpoints {
		endpoint := &def.Endpoints[i]
		if endpoint.RequestBody != nil {
			for j := range endpoint.RequestBody.Content {
				walk(&endpoint.RequestBody.Content[j].Schema, "")
			}
		}
		for j := range endpoint.Parameters {
			walk(&endpoint.Parameters[j].Schema, endpoint.Parameters[j].Name)
		}
		for j := range endpoint.Responses {
			for k := range endpoint.Responses[j].Content {
				walk(&endpoint.Responses[j].Content[k].Schema, "")
			}
		}
	}
	for i := range def.Webhooks {
		if def.Webhooks[i].PayloadSchema != nil {
			walk(def.Webhooks[i].PayloadSchema, "")
		}
	}
	return out
}

func trusted(provenance string) bool {
	return provenance == ir.ProvenanceExplicit || provenance == ir.ProvenanceDerived
}

func enumKey(value any) string {
	switch v := value.(type) {
	case string:
		return "s:" + v
	case bool:
		return fmt.Sprintf("b:%t", v)
	case json.Number:
		return "n:" + v.String()
	case float64:
		return fmt.Sprintf("n:%.17g", v)
	case float32:
		return fmt.Sprintf("n:%.9g", v)
	case int:
		return "n:" + strconv.Itoa(v)
	case int64:
		return "n:" + strconv.FormatInt(v, 10)
	case int32:
		return "n:" + strconv.FormatInt(int64(v), 10)
	case uint:
		return "n:" + strconv.FormatUint(uint64(v), 10)
	case uint64:
		return "n:" + strconv.FormatUint(v, 10)
	case uint32:
		return "n:" + strconv.FormatUint(uint64(v), 10)
	default:
		return fmt.Sprintf("%T:%v", value, value)
	}
}
