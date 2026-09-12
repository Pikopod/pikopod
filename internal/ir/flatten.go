// allOf is an INTERSECTION, so it flattens to one plain node; oneOf/anyOf are
// choices and stay composed, because choice handling belongs to the consumer.
package ir

import "fmt"

// maxFlattenDepth bounds ref-chain and nesting resolution (specs can cycle).
const maxFlattenDepth = 32

// FlattenAllOf merges an allOf (recursively, through refs) into one node; a
// type conflict or a non-allOf node returns the original untouched.
func FlattenAllOf(n *IrSchemaNode, table map[string]*IrSchemaNode) *IrSchemaNode {
	return flattenAllOf(n, table, 0)
}

func flattenAllOf(n *IrSchemaNode, table map[string]*IrSchemaNode, depth int) *IrSchemaNode {
	if n == nil || depth > maxFlattenDepth {
		return n
	}
	for hops := 0; n.Ref != nil && hops < maxFlattenDepth; hops++ {
		target, ok := table[*n.Ref]
		if !ok {
			return n
		}
		n = target
	}
	if n.Composition == nil || n.Composition.Kind != "allOf" {
		return n
	}

	merged := &IrSchemaNode{ID: n.ID, Nullable: Prov[bool]{Value: true}, SourcePointer: n.SourcePointer}
	nullableDeclared := false
	props := map[string]*PropertySchema{}
	var propOrder []string
	original := n

	for mi := range n.Composition.Members {
		m := flattenAllOf(&n.Composition.Members[mi], table, depth+1)
		if m == nil {
			continue
		}
		if m.Composition != nil {
			return original // a non-allOf member: not mergeable, keep composed
		}
		if m.Type.Value != "" && m.Type.Value != "unknown" {
			if merged.Type.Value != "" && merged.Type.Value != m.Type.Value {
				return original // conflicting member types: unsatisfiable as declared
			}
			merged.Type = m.Type
		}
		// Nullable intersects: null passes only if every declaring member
		// allows it.
		if !m.Nullable.Value {
			merged.Nullable = m.Nullable
		}
		nullableDeclared = true
		if m.Format != nil && merged.Format == nil {
			merged.Format = m.Format
		}
		if m.Items != nil && merged.Items == nil {
			merged.Items = m.Items
		}
		if m.EnumValues != nil {
			if merged.EnumValues == nil {
				merged.EnumValues = m.EnumValues
			} else {
				merged.EnumValues = intersectEnums(merged.EnumValues, m.EnumValues)
			}
		}
		merged.Constraints = append(merged.Constraints, m.Constraints...)
		for pi := range m.Properties {
			p := m.Properties[pi]
			if existing, ok := props[p.Name]; ok {
				// Later member's schema wins; required-ness is sticky (OR).
				if existing.Required.Value {
					p.Required = existing.Required
				}
				*existing = p
				continue
			}
			cp := p
			props[p.Name] = &cp
			propOrder = append(propOrder, p.Name)
		}
	}
	if !nullableDeclared {
		merged.Nullable = Prov[bool]{}
	}
	for _, name := range propOrder {
		merged.Properties = append(merged.Properties, *props[name])
	}
	return merged
}

func intersectEnums(a, b *Prov[[]any]) *Prov[[]any] {
	inB := map[string]bool{}
	for _, v := range b.Value {
		inB[canonEnum(v)] = true
	}
	out := Prov[[]any]{Provenance: a.Provenance}
	for _, v := range a.Value {
		if inB[canonEnum(v)] {
			out.Value = append(out.Value, v)
		}
	}
	return &out
}

func canonEnum(v any) string {
	if s, ok := v.(string); ok {
		return "s:" + s
	}
	return "v:" + fmt.Sprint(v)
}
