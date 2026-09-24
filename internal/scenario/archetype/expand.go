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
			return nil, errfmt.New("scenario expansion failed", fmt.Sprintf("binding for role '%s' is missing", req.Role), "supply a binding for every role the archetype requires", "")
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
				return nil, errfmt.New("scenario expansion failed", fmt.Sprintf("webhook event '%s' is not in this API version", ref), "bind the role to an event the imported spec declares", "")
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
				return nil, errfmt.New("scenario expansion failed", fmt.Sprintf("operation '%s' is not in this API version", ref), "bind the role to an operation the imported spec declares", "")
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
		return "", errfmt.New("scenario expansion failed", fmt.Sprintf("unknown role token '%s'", token), "the archetype's step templates may only reference declared roles", "")
	}
	if r.isEvent {
		if hasField && field != "" {
			return "", errfmt.New("scenario expansion failed", fmt.Sprintf("webhook role '%s' has no field '%s'", role, field), "reference the event role bare: <<role>>", "")
		}
		return r.event, nil
	}
	if field == "method" {
		return r.method, nil
	}
	if field == "collectionPath" {
		return r.collectionPath, nil
	}
	return "", errfmt.New("scenario expansion failed", fmt.Sprintf("unknown operation field '%s' on role '%s'", field, role), "operation roles expose <<role.method>> and <<role.collectionPath>>", "")
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
	return &Expanded{
		Definition:       map[string]any{"steps": steps, "requiresFidelity": a.RequiresFidelity},
		RequiresFidelity: a.RequiresFidelity,
	}, nil
}
