package sandbox

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pikopod/pikopod/internal/ir"
)

type synthContext struct {
	prng           *Prng
	virtualClockMs int64
	namedSchemas   map[string]*ir.IrSchemaNode
}

func makeContext(seed string, virtualClockMs int64, namedSchemas map[string]*ir.IrSchemaNode) *synthContext {
	return &synthContext{prng: NewPrng(seed), virtualClockMs: virtualClockMs, namedSchemas: namedSchemas}
}

const (
	synthMaxDepth      = 6
	synthMaxArrayItems = 4
)

func numericConstraint(schema *ir.IrSchemaNode, key string) *float64 {
	for _, c := range schema.Constraints {
		if c.Key == key {
			switch v := c.Value.Value.(type) {
			case float64:
				return &v
			case int:
				f := float64(v)
				return &f
			case int64:
				f := float64(v)
				return &f
			case json.Number:
				if f, err := v.Float64(); err == nil {
					return &f
				}
			}
			return nil
		}
	}
	return nil
}

func isoFrom(virtualClockMs int64, dateOnly bool) string {
	iso := time.UnixMilli(virtualClockMs).UTC().Format("2006-01-02T15:04:05.000Z")
	if dateOnly {
		return iso[:10]
	}
	return iso
}

func pickAny(p *Prng, items []any) any {
	return items[p.Int(0, len(items)-1)]
}

var (
	lnameEmail   = regexp.MustCompile(`email`)
	lnameURL     = regexp.MustCompile(`(^|_)(url|uri|link)$`)
	lnameTimeish = regexp.MustCompile(`(_at|date|time)$`)
)

func synthString(schema *ir.IrSchemaNode, ctx *synthContext, fieldName string) string {
	format := ""
	if schema.Format != nil {
		format = schema.Format.Value
	}
	p := ctx.prng
	switch format {
	case "uuid":
		return p.Hex(8) + "-" + p.Hex(4) + "-4" + p.Hex(3) + "-" + p.Pick([]string{"8", "9", "a", "b"}) + p.Hex(3) + "-" + p.Hex(12)
	case "email":
		return p.Word() + "." + p.Word() + "@" + p.Word() + "." + p.Pick([]string{"com", "io", "test"})
	case "date-time":
		return isoFrom(ctx.virtualClockMs, false)
	case "date":
		return isoFrom(ctx.virtualClockMs, true)
	case "uri", "url":
		return "https://" + p.Word() + "." + p.Pick([]string{"com", "io", "test"}) + "/" + p.Word()
	case "hostname":
		return p.Word() + "." + p.Pick([]string{"com", "io", "test"})
	case "ipv4":
		return strconv.Itoa(p.Int(1, 254)) + "." + strconv.Itoa(p.Int(0, 255)) + "." + strconv.Itoa(p.Int(0, 255)) + "." + strconv.Itoa(p.Int(1, 254))
	}

	lname := strings.ToLower(fieldName)
	if lnameEmail.MatchString(lname) {
		return p.Word() + "@" + p.Word() + ".test"
	}
	if lnameURL.MatchString(lname) {
		return "https://" + p.Word() + ".test/" + p.Word()
	}
	if lnameTimeish.MatchString(lname) {
		return isoFrom(ctx.virtualClockMs, false)
	}

	min := numericConstraint(schema, "minLength")
	max := numericConstraint(schema, "maxLength")
	s := p.Word()
	if min != nil && float64(len(s)) < *min {
		s = s + p.Token(int(*min)-len(s))
	}
	if max != nil && float64(len(s)) > *max {
		end := int(math.Max(0, *max))
		s = s[:end]
	}
	return s
}

func synthNumber(schema *ir.IrSchemaNode, ctx *synthContext, integer bool) any {
	min := numericConstraint(schema, "minimum")
	max := numericConstraint(schema, "maximum")
	lo := 0.0
	if min != nil {
		lo = *min
	}
	hi := 1000.0
	if max != nil {
		hi = *max
	} else if min != nil {
		hi = *min + 1000
	}
	if integer {
		return ctx.prng.Int(int(math.Ceil(lo)), int(math.Floor(hi)))
	}

	return math.Floor((lo+ctx.prng.Next()*(hi-lo))*100+0.5) / 100
}

func synthesize(schema *ir.IrSchemaNode, ctx *synthContext, depth int, fieldName string) any {
	if depth > synthMaxDepth {
		return nil
	}

	if schema.Ref != nil {
		if target, ok := ctx.namedSchemas[*schema.Ref]; ok {
			return synthesize(target, ctx, depth+1, fieldName)
		}
		return nil
	}
	if schema.Composition != nil && len(schema.Composition.Members) > 0 {
		return synthesize(&schema.Composition.Members[0], ctx, depth+1, fieldName)
	}
	if schema.EnumValues != nil && len(schema.EnumValues.Value) > 0 {
		return pickAny(ctx.prng, schema.EnumValues.Value)
	}

	switch schema.Type.Value {
	case "object":
		out := NewJSONObject()
		for i := range schema.Properties {
			prop := &schema.Properties[i]
			out.Set(prop.Name, synthesize(&prop.Schema, ctx, depth+1, prop.Name))
		}
		return out
	case "array":
		if schema.Items == nil {
			return []any{}
		}
		min := 1.0
		if m := numericConstraint(schema, "minItems"); m != nil {
			min = *m
		}
		max := 3.0
		if m := numericConstraint(schema, "maxItems"); m != nil {
			max = *m
		}
		count := ctx.prng.Int(int(min), int(math.Max(min, max)))
		if count < 0 {
			count = 0
		}
		if count > synthMaxArrayItems {
			count = synthMaxArrayItems
		}
		out := make([]any, count)
		for i := 0; i < count; i++ {
			out[i] = synthesize(schema.Items, ctx, depth+1, fieldName)
		}
		return out
	case "string":
		return synthString(schema, ctx, fieldName)
	case "integer":
		return synthNumber(schema, ctx, true)
	case "number":
		return synthNumber(schema, ctx, false)
	case "boolean":
		return ctx.prng.Bool()
	case "null":
		return nil
	default:

		return ctx.prng.Word()
	}
}

func derefSchema(schema *ir.IrSchemaNode, ctx *synthContext, depth int) *ir.IrSchemaNode {
	if schema == nil || depth > 10 {
		return schema
	}
	if schema.Ref != nil {
		return derefSchema(ctx.namedSchemas[*schema.Ref], ctx, depth+1)
	}
	return schema
}

func completeResource(responseSchema *ir.IrSchemaNode, provided *JSONObject, ctx *synthContext, declaredOnly bool) *JSONObject {
	resolved := derefSchema(responseSchema, ctx, 0)
	if resolved == nil || resolved.Type.Value != "object" {
		if declaredOnly {
			return NewJSONObject()
		}
		return provided.Clone()
	}
	base, _ := synthesize(resolved, ctx, 0, "").(*JSONObject)
	if base == nil {
		base = NewJSONObject()
	}
	if !declaredOnly {
		out := base.Clone()
		for _, k := range provided.Keys() {
			v, _ := provided.Get(k)
			out.Set(k, v)
		}
		return out
	}
	declared := map[string]bool{}
	for _, p := range resolved.Properties {
		declared[p.Name] = true
	}
	out := base.Clone()
	for _, k := range provided.Keys() {
		if declared[k] {
			v, _ := provided.Get(k)
			out.Set(k, v)
		}
	}
	return out
}

var countLike = regexp.MustCompile(`(?i)count|total|size`)

func shapeListBody(responseSchema *ir.IrSchemaNode, items []any, ctx *synthContext) any {
	resolved := derefSchema(responseSchema, ctx, 0)
	if resolved == nil || resolved.Type.Value != "object" {
		return items
	}

	var arrayProp *ir.PropertySchema
	for i := range resolved.Properties {
		if resolved.Properties[i].Schema.Type.Value == "array" {
			arrayProp = &resolved.Properties[i]
			break
		}
	}
	if arrayProp == nil {
		return items
	}

	out := NewJSONObject()
	for i := range resolved.Properties {
		prop := &resolved.Properties[i]
		switch {
		case prop.Name == arrayProp.Name:
			out.Set(prop.Name, items)
		case prop.Schema.Type.Value == "integer" && countLike.MatchString(prop.Name):
			out.Set(prop.Name, len(items))
		default:
			out.Set(prop.Name, synthesize(&prop.Schema, ctx, 0, prop.Name))
		}
	}
	return out
}
