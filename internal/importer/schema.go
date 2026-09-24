package importer

import (
	"fmt"
	"sort"

	"github.com/pikopod/pikopod/internal/ir"
)

var constraintKeys = []string{
	"minLength",
	"maxLength",
	"pattern",
	"minimum",
	"maximum",
	"exclusiveMinimum",
	"exclusiveMaximum",
	"multipleOf",
	"minItems",
	"maxItems",
	"uniqueItems",
	"minProperties",
	"maxProperties",
}

var compositionKinds = []string{"allOf", "oneOf", "anyOf"}

func normalizeSchema(raw any, parentID, role, pointer string, res *refResolver, limits ParseLimits, depth int) (ir.IrSchemaNode, error) {
	if depth > limits.MaxSchemaDepth {
		return ir.IrSchemaNode{}, specErr(SpecDepthExceeded, fmt.Sprintf("schema nesting exceeds %d", limits.MaxSchemaDepth))
	}
	id := ir.SchemaNodeID(parentID, role)
	node := emptySchemaNode(id, pointer)

	if refPtr, ok := res.isRef(raw); ok {
		refName := res.schemaRefName(refPtr)
		if refName == "" && res.isExternal(refPtr) {
			resolved, err := res.resolve(raw)
			if err != nil {
				return ir.IrSchemaNode{}, err
			}
			return normalizeSchema(resolved, parentID, role, pointer, res, limits, depth+1)
		}
		if refName == "" {

			return ir.IrSchemaNode{}, &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("unsupported schema $ref %q", refPtr), Pointer: refPtr}
		}
		node.Type = ir.Derived("unknown", pointer)
		refID := ir.NamedSchemaID(refName)
		node.Ref = &refID
		return node, nil
	}

	obj, ok := raw.(*OrdMap)
	if !ok {
		node.Type = ir.Derived("unknown", pointer)
		return node, nil
	}

	for _, kind := range compositionKinds {
		members, isArr := obj.GetOr(kind).([]any)
		if !isArr {
			continue
		}
		node.Type = ir.Derived("unknown", pointer)
		node.Nullable = nullableFrom(obj, pointer)
		comp := &ir.SchemaComposition{Kind: kind, Members: make([]ir.IrSchemaNode, 0, len(members))}
		for i, m := range members {
			member, err := normalizeSchema(m, id, fmt.Sprintf("%s:%d", kind, i), fmt.Sprintf("%s/%s/%d", pointer, kind, i), res, limits, depth+1)
			if err != nil {
				return ir.IrSchemaNode{}, err
			}
			comp.Members = append(comp.Members, member)
		}
		node.Composition = comp
		return node, nil
	}

	nodeType, nullable := normalizeType(obj, pointer)
	node.Type = nodeType
	node.Nullable = nullable

	for _, key := range constraintKeys {
		switch v := obj.GetOr(key).(type) {
		case string, float64, bool:
			node.Constraints = append(node.Constraints, ir.SchemaConstraint{
				Key:   key,
				Value: ir.Explicit[any](v, pointer+"/"+key),
			})
		}
	}

	if enumRaw, isArr := obj.GetOr("enum").([]any); isArr && len(enumRaw) > 0 {
		filtered := make([]any, 0, len(enumRaw))
		for _, v := range enumRaw {
			switch v.(type) {
			case nil, string, float64, bool:
				filtered = append(filtered, v)
			}
		}
		ev := ir.Explicit(filtered, pointer+"/enum")
		node.EnumValues = &ev
	}

	if properties, hasProps := obj.Get("properties"); hasProps && properties != nil {
		entries := objectEntries(properties)
		if entries != nil {
			requiredArr, _ := obj.GetOr("required").([]any)
			for _, e := range entries {
				name := e.key
				propSchema, err := normalizeSchema(e.value, id, "prop:"+name, pointer+"/properties/"+name, res, limits, depth+1)
				if err != nil {
					return ir.IrSchemaNode{}, err
				}
				node.Properties = append(node.Properties, ir.PropertySchema{
					Name:     name,
					Required: ir.Explicit(includesString(requiredArr, name), pointer+"/required"),
					Schema:   propSchema,
				})
			}
			sort.SliceStable(node.Properties, func(i, j int) bool {
				return ir.JSLess(node.Properties[i].Name, node.Properties[j].Name)
			})
		}
	}

	if itemsRaw, hasItems := obj.Get("items"); hasItems {
		items, err := normalizeSchema(itemsRaw, id, "items", pointer+"/items", res, limits, depth+1)
		if err != nil {
			return ir.IrSchemaNode{}, err
		}
		node.Items = &items
	}

	if format, ok := obj.GetOr("format").(string); ok {
		f := ir.Explicit(format, pointer+"/format")
		node.Format = &f
	}

	return node, nil
}

func normalizeType(obj *OrdMap, pointer string) (ir.Prov[string], ir.Prov[bool]) {
	rawType := obj.GetOr("type")

	if arr, isArr := rawType.([]any); isArr {
		var nonNull []any
		hasNull := false
		for _, t := range arr {
			if s, ok := t.(string); ok && s == "null" {
				hasNull = true
				continue
			}
			nonNull = append(nonNull, t)
		}
		if len(nonNull) == 1 {
			if s, ok := nonNull[0].(string); ok && isScalarType(s) {
				return ir.Explicit(s, pointer+"/type"), ir.Derived(hasNull, pointer+"/type")
			}
		}
		return ir.Derived("unknown", pointer+"/type"), ir.Derived(hasNull, pointer+"/type")
	}

	if s, ok := rawType.(string); ok && isScalarType(s) {
		return ir.Explicit(s, pointer+"/type"), nullableFrom(obj, pointer)
	}

	derivedType := "unknown"
	if obj.Has("properties") {
		derivedType = "object"
	} else if obj.Has("items") {
		derivedType = "array"
	}
	return ir.Derived(derivedType, pointer), nullableFrom(obj, pointer)
}

func nullableFrom(obj *OrdMap, pointer string) ir.Prov[bool] {

	if b, ok := obj.GetOr("nullable").(bool); ok && b {
		return ir.Explicit(true, pointer+"/nullable")
	}
	return ir.Derived(false, pointer)
}

func isScalarType(value string) bool {
	switch value {
	case "string", "number", "integer", "boolean", "object", "array", "null", "unknown":
		return true
	}
	return false
}

func emptySchemaNode(id, pointer string) ir.IrSchemaNode {
	return ir.IrSchemaNode{
		ID:            id,
		Type:          ir.Derived("unknown", pointer),
		Nullable:      ir.Derived(false, pointer),
		Constraints:   []ir.SchemaConstraint{},
		Properties:    []ir.PropertySchema{},
		SourcePointer: pointer,
	}
}

type entry struct {
	key   string
	value any
}

func objectEntries(value any) []entry {
	switch v := value.(type) {
	case *OrdMap:
		out := make([]entry, 0, v.Len())
		for _, k := range v.Keys() {
			out = append(out, entry{key: k, value: v.values[k]})
		}
		return out
	case []any:
		out := make([]entry, 0, len(v))
		for i, item := range v {
			out = append(out, entry{key: fmt.Sprintf("%d", i), value: item})
		}
		return out
	default:
		return nil
	}
}

func includesString(arr []any, name string) bool {
	for _, v := range arr {
		if s, ok := v.(string); ok && s == name {
			return true
		}
	}
	return false
}
