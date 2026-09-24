package sandbox

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

type responseShape struct {
	outer *ir.IrSchemaNode
	inner *ir.IrSchemaNode
	slot  string
}

func shapeFor(schema *ir.IrSchemaNode, keys []string, ctx *synthContext) responseShape {
	resolved := derefSchema(schema, ctx, 0)
	shape := responseShape{outer: schema, inner: schema}
	if resolved == nil || resolved.Type.Value != "object" || len(keys) == 0 {
		return shape
	}
	want := map[string]bool{}
	for _, k := range keys {
		want[k] = true
	}
	best := overlap(resolved, want)
	for i := range resolved.Properties {
		p := &resolved.Properties[i]
		inner := derefSchema(&p.Schema, ctx, 0)
		if inner == nil || inner.Type.Value != "object" {
			continue
		}
		if score := overlap(inner, want); score > best {
			best, shape.slot, shape.inner = score, p.Name, &p.Schema
		}
	}
	return shape
}

func overlap(schema *ir.IrSchemaNode, want map[string]bool) int {
	n := 0
	for i := range schema.Properties {
		if want[schema.Properties[i].Name] {
			n++
		}
	}
	return n
}

func (s responseShape) wrap(resource any, ctx *synthContext) any {
	if s.slot == "" {
		return resource
	}
	outer, _ := synthesize(derefSchema(s.outer, ctx, 0), ctx, 0, "").(*JSONObject)
	if outer == nil {
		return resource
	}
	outer.Set(s.slot, resource)
	return outer
}

func resourceKeys(provided *JSONObject, requestSchema *ir.IrSchemaNode, ctx *synthContext) []string {
	var keys []string
	if provided != nil {
		keys = append(keys, provided.Keys()...)
	}
	if req := derefSchema(requestSchema, ctx, 0); req != nil {
		for i := range req.Properties {
			keys = append(keys, req.Properties[i].Name)
		}
	}
	return keys
}

func (e *Engine) wrapStored(endpoint *ir.Endpoint, status int, raw json.RawMessage, seedParts ...string) any {
	ctx := e.synthCtx(append([]string{"wrap"}, seedParts...)...)
	var keys []string
	if obj, ok := parseJSONValueOK(raw); ok {
		keys = obj.Keys()
	}
	shape := shapeFor(successSchema(endpoint, status), keys, ctx)
	if shape.slot != "" {
		e.tracef("response", "resource wrapped in %q per the %d response schema", shape.slot, status)
	}
	return shape.wrap(raw, ctx)
}

func parseJSONValueOK(raw json.RawMessage) (*JSONObject, bool) {
	v, err := parseJSONValue(string(raw))
	if err != nil {
		return nil, false
	}
	obj, ok := v.(*JSONObject)
	return obj, ok
}

func successResponse(endpoint *ir.Endpoint, status int) *ir.ResponseDef {
	code := strconv.Itoa(status)
	for i := range endpoint.Responses {
		if endpoint.Responses[i].StatusCode == code {
			return &endpoint.Responses[i]
		}
	}
	for i := range endpoint.Responses {
		if s := statusIsSuccess(endpoint.Responses[i].StatusCode); s != nil && *s == status {
			return &endpoint.Responses[i]
		}
	}
	return nil
}

func (e *Engine) declaredExample(forNodeID string) (any, bool) {
	var best *ir.Example
	for i := range e.def.Examples {
		ex := &e.def.Examples[i]
		if ex.ForNodeID != forNodeID || ex.MediaType == nil || !strings.Contains(strings.ToLower(*ex.MediaType), "json") {
			continue
		}
		if best == nil || ex.ID < best.ID {
			best = ex
		}
	}
	if best == nil {
		return nil, false
	}
	return best.Value.Value, true
}
