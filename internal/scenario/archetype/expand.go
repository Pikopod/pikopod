package archetype

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pikopod/pikopod/internal/errfmt"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/scenario"
)

type resolvedRole struct {
	method         string
	collectionPath string

	event   string
	isEvent bool
}

var tokenRe = regexp.MustCompile(`<<\s*([^>]+?)\s*>>`)

func resolveBindings(a *Archetype, bindings map[string]string, apiDef *ir.ApiDefinition) (map[string]resolvedRole, error) {
	resolved := map[string]resolvedRole{}
	for _, req := range a.Requires {
		ref, has := bindings[req.Role]
		if !has || ref == "" {
			return nil, errfmt.New("scenario expansion failed", fmt.Sprintf("binding for role '%s' is missing", req.Role), "supply a binding for every role the archetype requires", "scenarios/README.md#archetypes-start-here")
		}
		if req.Bind == "webhookEvent" {
			found := false
			for i := range apiDef.Webhooks {
				if apiDef.Webhooks[i].Event.Value == ref {
					found = true
					break
				}
			}
			if !found {
				return nil, errfmt.New("scenario expansion failed", fmt.Sprintf("webhook event '%s' is not in this API version", ref), "bind the role to an event the imported spec declares", "scenarios/README.md#archetypes-start-here")
			}
			resolved[req.Role] = resolvedRole{event: ref, isEvent: true}
		} else {
			var ep *ir.Endpoint
			for i := range apiDef.Endpoints {
				e := &apiDef.Endpoints[i]
				id := e.ID
				if e.OperationID != nil {
					id = e.OperationID.Value
				}
				if id == ref {
					ep = e
					break
				}
			}
			if ep == nil {
				return nil, errfmt.New("scenario expansion failed", fmt.Sprintf("operation '%s' is not in this API version", ref), "bind the role to an operation the imported spec declares", "scenarios/README.md#archetypes-start-here")
			}
			resolved[req.Role] = resolvedRole{method: strings.ToUpper(ep.Method.Value), collectionPath: scenario.ResourceTypeOf(ep)}
		}
	}
	return resolved, nil
}

func tokenValue(token string, resolved map[string]resolvedRole) (string, error) {
	role, field, hasField := strings.Cut(token, ".")
	r, has := resolved[role]
	if !has {
		return "", errfmt.New("scenario expansion failed", fmt.Sprintf("unknown role token '%s'", token), "the archetype's step templates may only reference declared roles", "scenarios/README.md#archetypes-start-here")
	}
	if r.isEvent {
		if hasField && field != "" {
			return "", errfmt.New("scenario expansion failed", fmt.Sprintf("webhook role '%s' has no field '%s'", role, field), "reference the event role bare: <<role>>", "scenarios/README.md#archetypes-start-here")
		}
		return r.event, nil
	}
	if field == "method" {
		return r.method, nil
	}
	if field == "collectionPath" {
		return r.collectionPath, nil
	}
	return "", errfmt.New("scenario expansion failed", fmt.Sprintf("unknown operation field '%s' on role '%s'", field, role), "operation roles expose <<role.method>> and <<role.collectionPath>>", "scenarios/README.md#archetypes-start-here")
}

func substitute(value any, resolved map[string]resolvedRole) (any, error) {
	switch v := value.(type) {
	case string:
		var firstErr error
		out := tokenRe.ReplaceAllStringFunc(v, func(m string) string {
			token := strings.TrimSpace(tokenRe.FindStringSubmatch(m)[1])
			val, err := tokenValue(token, resolved)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			return val
		})
		if firstErr != nil {
			return nil, firstErr
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			r, err := substitute(item, resolved)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			r, err := substitute(item, resolved)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	default:
		return value, nil
	}
}

type Expanded struct {
	Definition       map[string]any `json:"definition"`
	RequiresFidelity string         `json:"requiresFidelity"`
}

func Expand(a *Archetype, bindings map[string]string, apiDef *ir.ApiDefinition) (*Expanded, error) {
	resolved, err := resolveBindings(a, bindings, apiDef)
	if err != nil {
		return nil, err
	}
	steps := make([]any, len(a.Expands))
	for i, s := range a.Expands {
		sub, err := substitute(map[string]any(s), resolved)
		if err != nil {
			return nil, err
		}
		steps[i] = sub
	}
	if !a.RawBodies {
		fillRequiredBodies(steps, apiDef)
	}
	return &Expanded{
		Definition:       map[string]any{"steps": steps, "requiresFidelity": a.RequiresFidelity},
		RequiresFidelity: a.RequiresFidelity,
	}, nil
}

func fillRequiredBodies(steps []any, apiDef *ir.ApiDefinition) {
	named := map[string]*ir.IrSchemaNode{}
	for i := range apiDef.Schemas {
		named[apiDef.Schemas[i].ID] = &apiDef.Schemas[i].Schema
	}
	for _, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok || step["type"] != "REQUEST" {
			continue
		}
		cfg, ok := step["config"].(map[string]any)
		if !ok {
			continue
		}
		method, _ := cfg["method"].(string)
		path, _ := cfg["path"].(string)
		endpoint := endpointForStep(apiDef, method, path)
		if endpoint == nil || endpoint.RequestBody == nil {
			continue
		}
		body, isObject := cfg["body"].(map[string]any)
		if !isObject {
			continue
		}
		var schema *ir.IrSchemaNode
		for i := range endpoint.RequestBody.Content {
			if strings.Contains(strings.ToLower(endpoint.RequestBody.Content[i].MediaType), "json") {
				schema = &endpoint.RequestBody.Content[i].Schema
				break
			}
		}
		if schema == nil {
			continue
		}
		for k, v := range minimalRequired(schema, named, 0) {
			if _, has := body[k]; !has {
				body[k] = v
			}
		}
		cfg["body"] = body
	}
}

func endpointForStep(apiDef *ir.ApiDefinition, method, path string) *ir.Endpoint {
	want := strings.Split(strings.Trim(path, "/"), "/")
	var fallback *ir.Endpoint
	for i := range apiDef.Endpoints {
		ep := &apiDef.Endpoints[i]
		if !strings.EqualFold(ep.Method.Value, method) {
			continue
		}
		if ep.PathTemplate.Value == path {
			return ep
		}
		have := strings.Split(strings.Trim(ep.PathTemplate.Value, "/"), "/")
		if len(have) != len(want) {
			continue
		}
		match := true
		for j := range have {
			param := strings.HasPrefix(have[j], "{") || strings.HasPrefix(want[j], "{{")
			if !param && have[j] != want[j] {
				match = false
				break
			}
		}
		if match && fallback == nil {
			fallback = ep
		}
	}
	return fallback
}

func minimalRequired(schema *ir.IrSchemaNode, named map[string]*ir.IrSchemaNode, depth int) map[string]any {
	out := map[string]any{}
	if schema == nil || depth > 6 {
		return out
	}
	for hops := 0; schema.Ref != nil && hops < 10; hops++ {
		target, ok := named[*schema.Ref]
		if !ok {
			return out
		}
		schema = target
	}
	if schema.Composition != nil && len(schema.Composition.Members) > 0 {
		return minimalRequired(&schema.Composition.Members[0], named, depth+1)
	}
	for i := range schema.Properties {
		p := &schema.Properties[i]
		if !p.Required.Value {
			continue
		}
		out[p.Name] = minimalValue(&p.Schema, p.Name, named, depth+1)
	}
	return out
}

func minimalValue(schema *ir.IrSchemaNode, name string, named map[string]*ir.IrSchemaNode, depth int) any {
	for hops := 0; schema != nil && schema.Ref != nil && hops < 10; hops++ {
		target, ok := named[*schema.Ref]
		if !ok {
			return name
		}
		schema = target
	}
	if schema == nil || depth > 6 {
		return name
	}
	if schema.EnumValues != nil && len(schema.EnumValues.Value) > 0 {
		return schema.EnumValues.Value[0]
	}
	format := ""
	if schema.Format != nil {
		format = schema.Format.Value
	}
	switch schema.Type.Value {
	case "object":
		return minimalRequired(schema, named, depth)
	case "array":
		return []any{}
	case "integer", "number":
		return 1
	case "boolean":
		return true
	case "string":
		switch format {
		case "email":
			return "user@example.test"
		case "date-time":
			return "2025-01-01T00:00:00.000Z"
		case "date":
			return "2025-01-01"
		case "uri", "url":
			return "https://example.test/"
		case "uuid":
			return "00000000-0000-4000-8000-000000000000"
		}
		return name
	}
	return name
}
