package agent

import (
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sanitize"
)

// enumRulesFromContract derives sanitize rules that let a field's value
// survive redaction only because the provider's OWN spec declares it a
// member of that field's enum (github issue #28: the sanitizer's
// lowercase-only shape check either drops or tokenizes every
// SCREAMING_SNAKE/ISO-code vocabulary, which is most real-world APIs).
//
// Only EXPLICIT and DERIVED enum values qualify — deliberately stricter than
// Prov.IsGuess(), which excludes INFERRED but NOT LLM_EXTRACTED. Enforcing a
// guessed enum produces a wrong test; relaxing redaction on one writes
// unverified, possibly-model-hallucinated strings to disk, and that is not
// recoverable.
func enumRulesFromContract(def *ir.ApiDefinition) []sanitize.Rule {
	if def == nil {
		return nil
	}
	schemas := make(map[string]*ir.IrSchemaNode, len(def.Schemas))
	for i := range def.Schemas {
		schemas[def.Schemas[i].ID] = &def.Schemas[i].Schema
	}

	byField := map[string]map[string]bool{}
	visited := map[string]bool{}
	walk := func(node *ir.IrSchemaNode, fieldName string) {
		collectEnumFields(node, fieldName, schemas, visited, byField)
	}

	for _, ep := range def.Endpoints {
		for _, p := range ep.Parameters {
			s := p.Schema
			walk(&s, p.Name)
		}
		if ep.RequestBody != nil {
			for _, mt := range ep.RequestBody.Content {
				s := mt.Schema
				walk(&s, "")
			}
		}
		for _, resp := range ep.Responses {
			for _, mt := range resp.Content {
				s := mt.Schema
				walk(&s, "")
			}
		}
	}
	for _, wh := range def.Webhooks {
		if wh.PayloadSchema != nil {
			walk(wh.PayloadSchema, "")
		}
	}
	for _, e := range def.ErrorCatalogue {
		if e.Schema != nil {
			walk(e.Schema, "")
		}
	}

	if len(byField) == 0 {
		return nil
	}
	rules := make([]sanitize.Rule, 0, len(byField))
	for field, members := range byField {
		values := make([]string, 0, len(members))
		for v := range members {
			values = append(values, v)
		}
		sort.Strings(values)
		rules = append(rules, sanitize.Rule{Field: field, Mode: sanitize.ModeAllow, Values: values})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Field < rules[j].Field })
	return rules
}

// collectEnumFields walks one schema tree, naming every node it descends
// into by the property/parameter name that reaches it, and records the
// declared string enum members of EXPLICIT/DERIVED fields under that name.
//
// visited is keyed by $ref id only (not by field name), so a schema reused
// under a second field name is not re-walked; the declared members recorded
// under that second name are then only what other paths already contributed.
// That under-covers a shared-schema-under-two-names case, but never in the
// unsafe direction: a field with no rule keeps today's fail-closed behaviour.
func collectEnumFields(node *ir.IrSchemaNode, fieldName string, schemas map[string]*ir.IrSchemaNode, visited map[string]bool, out map[string]map[string]bool) {
	if node == nil {
		return
	}
	if node.EnumValues != nil && isTrustedEnum(node.EnumValues.Provenance) && fieldName != "" {
		for _, v := range node.EnumValues.Value {
			if s, ok := v.(string); ok {
				addEnumMember(out, fieldName, s)
			}
		}
	}
	for _, p := range node.Properties {
		child := p.Schema
		collectEnumFields(&child, p.Name, schemas, visited, out)
	}
	if node.Items != nil {
		collectEnumFields(node.Items, fieldName, schemas, visited, out)
	}
	if node.Composition != nil {
		for i := range node.Composition.Members {
			collectEnumFields(&node.Composition.Members[i], fieldName, schemas, visited, out)
		}
	}
	if node.Ref != nil && !visited[*node.Ref] {
		visited[*node.Ref] = true
		if ref, ok := schemas[*node.Ref]; ok {
			collectEnumFields(ref, fieldName, schemas, visited, out)
		}
	}
}

func isTrustedEnum(provenance string) bool {
	return provenance == ir.ProvenanceExplicit || provenance == ir.ProvenanceDerived
}

func addEnumMember(out map[string]map[string]bool, field, value string) {
	field = strings.ToLower(field)
	set := out[field]
	if set == nil {
		set = map[string]bool{}
		out[field] = set
	}
	set[value] = true
}
