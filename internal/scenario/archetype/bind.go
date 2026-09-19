// The deterministic archetype binder. A candidate resting SOLELY on INFERRED
// facts is rejected; zero candidates is a first-class result with a reason.
package archetype

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/scenario"
)

type Candidate struct {
	Bindings     map[string]string `json:"bindings"` // role → operationId | webhook event
	UsesInferred bool              `json:"usesInferred"`
}

// Binding is one archetype's applicability plus its ranked candidates.
// InferredOnly holds candidates refused solely because every fact they rest
// on is extracted, so a caller can offer them for the user to assert.
type Binding struct {
	ArchetypeID  string      `json:"archetypeId"`
	Applicable   bool        `json:"applicable"`
	Reason       string      `json:"reason,omitempty"`
	Candidates   []Candidate `json:"candidates"`
	InferredOnly []Candidate `json:"inferredOnly,omitempty"`
}

const (
	maxCandidates   = 25
	maxInferredOnly = 3
)

type opFact struct {
	ref            string
	method         string
	collectionPath string
	crud           string // "" = none
	hasSuccess     bool
	has4xx         bool
	has5xx         bool
	requiresAuth   bool
	hasEnumField   bool
	inferred       bool
}

type hookFact struct {
	ref      string
	inferred bool
}

var itemParamRe = regexp.MustCompile(`^\{.+\}$`)

func crudOf(endpoint *ir.Endpoint) string {
	method := strings.ToUpper(endpoint.Method.Value)
	var segs []string
	for _, s := range strings.Split(endpoint.PathTemplate.Value, "/") {
		if len(s) > 0 {
			segs = append(segs, s)
		}
	}
	isItem := len(segs) > 0 && itemParamRe.MatchString(segs[len(segs)-1])
	if !isItem {
		if method == "POST" {
			return "CREATE"
		}
		if method == "GET" {
			return "LIST"
		}
	} else {
		switch method {
		case "GET":
			return "READ"
		case "PUT", "PATCH":
			return "UPDATE"
		case "DELETE":
			return "DELETE"
		}
	}
	return ""
}

func statusHasClass(codes []string, cls byte) bool {
	for _, c := range codes {
		if len(c) > 0 && c[0] == cls {
			return true
		}
		if strings.ToUpper(c) == string(cls)+"XX" {
			return true
		}
	}
	return false
}

func schemaHasEnum(schema *ir.IrSchemaNode, named map[string]*ir.IrSchemaNode, depth int, seen map[string]bool) bool {
	if schema == nil || depth > 6 {
		return false
	}
	if schema.EnumValues != nil && len(schema.EnumValues.Value) > 0 {
		return true
	}
	if schema.Ref != nil {
		if seen[*schema.Ref] {
			return false
		}
		seen[*schema.Ref] = true
		return schemaHasEnum(named[*schema.Ref], named, depth+1, seen)
	}
	if schema.Items != nil && schemaHasEnum(schema.Items, named, depth+1, seen) {
		return true
	}
	for i := range schema.Properties {
		if schemaHasEnum(&schema.Properties[i].Schema, named, depth+1, seen) {
			return true
		}
	}
	if schema.Composition != nil {
		for i := range schema.Composition.Members {
			if schemaHasEnum(&schema.Composition.Members[i], named, depth+1, seen) {
				return true
			}
		}
	}
	return false
}

func buildOpFacts(apiDef *ir.ApiDefinition) []opFact {
	named := map[string]*ir.IrSchemaNode{}
	for i := range apiDef.Schemas {
		named[apiDef.Schemas[i].ID] = &apiDef.Schemas[i].Schema
	}
	out := make([]opFact, 0, len(apiDef.Endpoints))
	for i := range apiDef.Endpoints {
		e := &apiDef.Endpoints[i]
		codes := make([]string, 0, len(e.Responses))
		var schemas []*ir.IrSchemaNode
		for j := range e.Responses {
			codes = append(codes, e.Responses[j].StatusCode)
			for k := range e.Responses[j].Content {
				schemas = append(schemas, &e.Responses[j].Content[k].Schema)
			}
		}
		if e.RequestBody != nil {
			var reqSchemas []*ir.IrSchemaNode
			for k := range e.RequestBody.Content {
				reqSchemas = append(reqSchemas, &e.RequestBody.Content[k].Schema)
			}
			schemas = append(reqSchemas, schemas...) // request schemas first
		}
		ref := e.ID
		if e.OperationID != nil {
			ref = e.OperationID.Value
		}
		hasEnum := false
		for _, s := range schemas {
			if schemaHasEnum(s, named, 0, map[string]bool{}) {
				hasEnum = true
				break
			}
		}
		out = append(out, opFact{
			ref:            ref,
			method:         strings.ToUpper(e.Method.Value),
			collectionPath: scenario.ResourceTypeOf(e),
			crud:           crudOf(e),
			hasSuccess:     statusHasClass(codes, '2'),
			has4xx:         statusHasClass(codes, '4'),
			has5xx:         statusHasClass(codes, '5'),
			requiresAuth:   len(e.Security) > 0,
			hasEnumField:   hasEnum,
			inferred:       e.Method.IsUncertain(),
		})
	}
	return out
}

func opMatches(fact *opFact, req *RoleRequirement, partial map[string]any) bool {
	m := &req.Match
	if m.Crud != "" && fact.crud != m.Crud {
		return false
	}
	if m.HasSuccessResponse && !fact.hasSuccess {
		return false
	}
	if m.HasErrorResponseClass == "4XX" && !fact.has4xx {
		return false
	}
	if m.HasErrorResponseClass == "5XX" && !fact.has5xx {
		return false
	}
	if m.RequiresAuth && !fact.requiresAuth {
		return false
	}
	if m.HasEnumField && !fact.hasEnumField {
		return false
	}
	if m.SameResourceAs != "" {
		other, has := partial[m.SameResourceAs]
		of, isOp := other.(*opFact)
		if !has || !isOp || of.collectionPath != fact.collectionPath {
			return false
		}
	}
	return true
}

// Bind returns zero or more candidate bindings, ranked deterministically.
func Bind(a *Archetype, apiDef *ir.ApiDefinition) Binding {
	return BindWith(a, apiDef, nil)
}

// BindWith treats each asserted role (from --bind) as an explicit fact: the
// named operation must exist and fit the role's shape, and it alone fills it.
func BindWith(a *Archetype, apiDef *ir.ApiDefinition, asserted map[string]string) Binding {
	ops := buildOpFacts(apiDef)
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].ref < ops[j].ref })
	var hooks []hookFact
	for i := range apiDef.Webhooks {
		hooks = append(hooks, hookFact{ref: apiDef.Webhooks[i].Event.Value, inferred: apiDef.Webhooks[i].Event.IsUncertain()})
	}
	sort.SliceStable(hooks, func(i, j int) bool { return hooks[i].ref < hooks[j].ref })

	var emptyRole *RoleRequirement
	var candidates, inferredOnly []Candidate
	assertErr := ""

	var backtrack func(roleIdx int, partial map[string]any)
	backtrack = func(roleIdx int, partial map[string]any) {
		if len(candidates) >= maxCandidates {
			return
		}
		if roleIdx == len(a.Requires) {
			usesInferred := false
			allInferred := len(partial) > 0
			bindings := map[string]string{}
			for role, f := range partial {
				inf := false
				switch t := f.(type) {
				case *opFact:
					inf = t.inferred
					bindings[role] = t.ref
				case hookFact:
					inf = t.inferred
					bindings[role] = t.ref
				}
				if inf {
					usesInferred = true
				}
				if !inf || asserted[role] != "" {
					allInferred = false
				}
			}
			if allInferred {
				if len(inferredOnly) < maxInferredOnly {
					inferredOnly = append(inferredOnly, Candidate{Bindings: bindings, UsesInferred: true})
				}
				return
			}
			candidates = append(candidates, Candidate{Bindings: bindings, UsesInferred: usesInferred})
			return
		}
		req := &a.Requires[roleIdx]
		var matches []any
		if ref, ok := asserted[req.Role]; ok {
			fact, why := assertedFact(ref, req, partial, ops, hooks)
			if fact == nil {
				assertErr = why
				return
			}
			matches = []any{fact}
		} else if req.Bind == "webhookEvent" {
			for _, h := range hooks {
				matches = append(matches, h)
			}
		} else {
			for i := range ops {
				if opMatches(&ops[i], req, partial) {
					matches = append(matches, &ops[i])
				}
			}
		}
		if len(matches) == 0 && emptyRole == nil {
			emptyRole = req
		}
		for _, fact := range matches {
			next := make(map[string]any, len(partial)+1)
			for k, v := range partial {
				next[k] = v
			}
			next[req.Role] = fact
			backtrack(roleIdx+1, next)
			if len(candidates) >= maxCandidates {
				return
			}
		}
	}
	backtrack(0, map[string]any{})

	// Deterministic ranking: by the concatenation of bound refs.
	key := func(c Candidate) string {
		parts := make([]string, 0, len(a.Requires))
		for _, r := range a.Requires {
			parts = append(parts, c.Bindings[r.Role])
		}
		return strings.Join(parts, "|")
	}
	sort.SliceStable(candidates, func(i, j int) bool { return key(candidates[i]) < key(candidates[j]) })

	if len(candidates) > 0 {
		return Binding{ArchetypeID: a.ID, Applicable: true, Candidates: candidates, InferredOnly: inferredOnly}
	}
	reason := "no candidate binding rests on explicit facts"
	switch {
	case assertErr != "":
		reason = assertErr
	case emptyRole != nil:
		reason = fmt.Sprintf("no %s matching %s for role '%s'", emptyRole.Bind, matchJSONString(&emptyRole.Match), emptyRole.Role)
	case len(inferredOnly) > 0:
		reason = "every candidate rests on extracted facts only; assert a role with --bind"
	}
	return Binding{ArchetypeID: a.ID, Applicable: false, Reason: reason, Candidates: []Candidate{}, InferredOnly: inferredOnly}
}

// assertedFact finds the fact a --bind names and checks it can play the role;
// the user vouches for what the docs left out, not for the operation's shape.
func assertedFact(ref string, req *RoleRequirement, partial map[string]any, ops []opFact, hooks []hookFact) (any, string) {
	if req.Bind == "webhookEvent" {
		for _, h := range hooks {
			if h.ref == ref {
				return h, ""
			}
		}
		return nil, fmt.Sprintf("--bind %s=%s: no declared webhook event %q", req.Role, ref, ref)
	}
	var fact *opFact
	for i := range ops {
		if ops[i].ref == ref {
			fact = &ops[i]
			break
		}
	}
	if fact == nil {
		return nil, fmt.Sprintf("--bind %s=%s: no operation with that id (operationId or ep_… from `scenario list`)", req.Role, ref)
	}
	m := &req.Match
	if m.Crud != "" && fact.crud != m.Crud {
		crud := fact.crud
		if crud == "" {
			crud = "not a CRUD operation"
		}
		return nil, fmt.Sprintf("--bind %s=%s: %s %s is %s; the role needs a %s operation", req.Role, ref, fact.method, fact.collectionPath, crud, m.Crud)
	}
	if m.SameResourceAs != "" {
		if of, ok := partial[m.SameResourceAs].(*opFact); ok && of.collectionPath != fact.collectionPath {
			return nil, fmt.Sprintf("--bind %s=%s: %s is not on the same resource as %s (%s)", req.Role, ref, fact.collectionPath, m.SameResourceAs, of.collectionPath)
		}
	}
	return fact, ""
}

// matchJSONString serializes a match byte-stably: only the keys the archetype
// set, in RoleMatch's declaration order — never the literal's insertion order.
func matchJSONString(m *RoleMatch) string {
	var parts []string
	add := func(k string, v any) {
		raw, _ := json.Marshal(v)
		parts = append(parts, fmt.Sprintf("%q:%s", k, string(raw)))
	}
	if m.Crud != "" {
		add("crud", m.Crud)
	}
	if m.HasSuccessResponse {
		add("hasSuccessResponse", true)
	}
	if m.HasErrorResponseClass != "" {
		add("hasErrorResponseClass", m.HasErrorResponseClass)
	}
	if m.RequiresAuth {
		add("requiresAuth", true)
	}
	if m.HasEnumField {
		add("hasEnumField", true)
	}
	if m.SameResourceAs != "" {
		add("sameResourceAs", m.SameResourceAs)
	}
	return "{" + strings.Join(parts, ",") + "}"
}
