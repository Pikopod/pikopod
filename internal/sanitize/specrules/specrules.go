// Package specrules turns a contract's declared enums into sanitizer rules,
// so the sanitizer itself never learns about the IR.
package specrules

import (
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sanitize"
)

const maxDepth = 12

// FromContract emits one ALLOW rule per response field name whose schema
// declares an enum, admitting exactly the declared string values. Only
// EXPLICIT and DERIVED enums count: enforcing a guess produces a wrong test,
// but relaxing redaction on a guess writes someone's data to disk.
func FromContract(def *ir.ApiDefinition) []sanitize.Rule {
	if def == nil {
		return nil
	}
	named := map[string]*ir.IrSchemaNode{}
	for i := range def.Schemas {
		named[def.Schemas[i].ID] = &def.Schemas[i].Schema
	}
	values := map[string]map[string]bool{}
	for i := range def.Endpoints {
		e := &def.Endpoints[i]
		for j := range e.Responses {
			resp := &e.Responses[j]
			if !strings.HasPrefix(resp.StatusCode, "2") {
				continue
			}
			for k := range resp.Content {
				walk(&resp.Content[k].Schema, "", named, 0, map[string]bool{}, values)
			}
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	rules := make([]sanitize.Rule, 0, len(names))
	for _, name := range names {
		set := values[name]
		allowed := make([]string, 0, len(set))
		for v := range set {
			allowed = append(allowed, v)
		}
		sort.Strings(allowed)
		rules = append(rules, sanitize.Rule{Field: name, Mode: sanitize.ModeAllow, AllowedValues: allowed})
	}
	return rules
}

// ForContracts maps each upstream to its rules; an upstream without a
// contract gets none, which is today's behaviour.
func ForContracts(contracts map[string]*ir.ApiDefinition) map[string][]sanitize.Rule {
	out := map[string][]sanitize.Rule{}
	for upstream, def := range contracts {
		if rules := FromContract(def); len(rules) > 0 {
			out[upstream] = rules
		}
	}
	return out
}

func trusted(provenance string) bool {
	return provenance == ir.ProvenanceExplicit || provenance == ir.ProvenanceDerived
}

func add(values map[string]map[string]bool, field string, vals []any, provenance string) {
	if field == "" || !trusted(provenance) {
		return
	}
	for _, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if values[field] == nil {
			values[field] = map[string]bool{}
		}
		values[field][s] = true
	}
}

func walk(node *ir.IrSchemaNode, field string, named map[string]*ir.IrSchemaNode, depth int, seen map[string]bool, values map[string]map[string]bool) {
	if node == nil || depth > maxDepth {
		return
	}
	if node.Ref != nil {
		if seen[*node.Ref] {
			return
		}
		seen[*node.Ref] = true
		walk(named[*node.Ref], field, named, depth+1, seen, values)
		return
	}
	if node.EnumValues != nil {
		add(values, field, node.EnumValues.Value, node.EnumValues.Provenance)
	}
	for i := range node.Properties {
		p := &node.Properties[i]
		walk(&p.Schema, p.Name, named, depth+1, seen, values)
	}
	if node.Items != nil {
		walk(node.Items, field, named, depth+1, seen, values)
	}
	if node.Composition != nil {
		for i := range node.Composition.Members {
			walk(&node.Composition.Members[i], field, named, depth+1, seen, values)
		}
	}
}
