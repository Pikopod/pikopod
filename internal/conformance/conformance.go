package conformance

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/pathtmpl"
	"github.com/pikopod/pikopod/internal/proxy"
	"github.com/pikopod/pikopod/internal/volatile"
)

type Violation struct {
	Method      string `json:"method"`
	Template    string `json:"template"`
	Status      int    `json:"status"`
	Pointer     string `json:"pointer,omitempty"`
	Code        string `json:"code"`
	Severity    string `json:"severity"`
	Message     string `json:"message"`
	Occurrences int    `json:"occurrences"`

	Observed string `json:"observed,omitempty"`
}

type Report struct {
	Records      int
	Skipped      int
	Unverifiable int
	Violations   []Violation
}

const maxWalkDepth = 12

type checker struct {
	named    map[string]*ir.IrSchemaNode
	seen     map[string]*Violation
	order    []string
	report   *Report
	redacted map[string]string
}

func Check(def *ir.ApiDefinition, records []*proxy.Record) *Report {
	c := &checker{named: map[string]*ir.IrSchemaNode{}, seen: map[string]*Violation{}, report: &Report{}}
	for i := range def.Schemas {
		c.named[def.Schemas[i].ID] = &def.Schemas[i].Schema
	}
	for _, rec := range records {
		c.checkRecord(def, rec)
	}
	for _, k := range c.order {
		c.report.Violations = append(c.report.Violations, *c.seen[k])
	}
	return c.report
}

func (c *checker) checkRecord(def *ir.ApiDefinition, rec *proxy.Record) {
	if rec.RespKind != "json" {
		c.report.Skipped++
		return
	}
	template := pathtmpl.Templatize(stripQuery(rec.Path))
	endpoint := matchEndpoint(def, rec.Method, template)
	if endpoint == nil {
		c.report.Skipped++
		return
	}
	c.report.Records++
	c.redacted = map[string]string{}
	for _, r := range rec.Redacted {
		if r.Section == "resp_body" {
			c.redacted[r.Pointer] = r.Mode
		}
	}

	resp := declaredResponse(endpoint, rec.Status)
	if resp == nil {
		severity, code := "warning", "status_undeclared"
		if rec.Status >= 200 && rec.Status < 300 {

			severity = "error"
		}
		c.add(endpoint, rec.Status, "", code, severity,
			fmt.Sprintf("status %d is not declared for this operation", rec.Status))
		return
	}
	schema := jsonSchemaOf(resp)
	if schema == nil {
		return
	}
	c.walk(endpoint, rec.Status, schema, rec.RespBody, "", 0)
}

func (c *checker) walk(endpoint *ir.Endpoint, status int, node *ir.IrSchemaNode, value any, pointer string, depth int) {
	if node == nil || depth > maxWalkDepth {
		return
	}
	if node.Ref != nil {
		if target, ok := c.named[*node.Ref]; ok {
			c.walk(endpoint, status, target, value, pointer, depth+1)
		}
		return
	}
	if node.Composition != nil {

		node = ir.FlattenAllOf(node, c.named)
	}
	if node.Composition != nil {

		c.checkVariants(endpoint, status, node, value, pointer, depth)
		return
	}

	if value == nil {
		if !node.Nullable.Value && node.Type.Value != "" {
			if c.suppressed(pointer) {
				return
			}
			c.addObserved(endpoint, status, pointer, "type", "error",
				fmt.Sprintf("documented %s, got null (not nullable)", node.Type.Value), "null")
		}
		return
	}

	if want := scalarClass(string(node.Type.Value)); want != "" {
		got := jsonClass(value)
		if got != "" && got != want {
			if c.suppressed(pointer) {
				return
			}
			c.addObserved(endpoint, status, pointer, "type", "error",
				fmt.Sprintf("documented %s, got %s", want, got), got)
			return
		}
	}

	if s, isStr := value.(string); isStr && node.EnumValues != nil &&
		!volatile.IsResponseField(lastPointerSegment(pointer)) {
		allowed := stringEnum(node.EnumValues.Value)
		if len(allowed) > 0 && !slices.Contains(allowed, s) {
			if c.suppressed(pointer) {
				return
			}
			c.addObserved(endpoint, status, pointer, "enum", "error",
				fmt.Sprintf("value %q not in documented set [%s]", s, strings.Join(allowed, ",")), s)
		}
	}

	switch v := value.(type) {
	case map[string]any:
		for i := range node.Properties {
			p := &node.Properties[i]
			childPtr := pointer + "/" + p.Name
			child, present := v[p.Name]
			if !present {
				if p.Required.Value {
					if mode, redacted := c.redacted[childPtr]; redacted && mode == "DROP" {

						c.report.Unverifiable++
						continue
					}
					c.add(endpoint, status, childPtr, "required", "error",
						"documented required, absent in the response")
				}
				continue
			}
			c.walk(endpoint, status, &p.Schema, child, childPtr, depth+1)
		}
	case []any:
		if node.Items != nil {
			for i := range v {

				c.walk(endpoint, status, node.Items, v[i], pointer+"/*", depth+1)
			}
		}
	}
}

func (c *checker) checkVariants(endpoint *ir.Endpoint, status int, node *ir.IrSchemaNode, value any, pointer string, depth int) {
	members := node.Composition.Members
	for i := range members {
		scratch := &checker{named: c.named, seen: map[string]*Violation{}, report: &Report{}, redacted: c.redacted}
		scratch.walk(endpoint, status, &members[i], value, pointer, depth+1)
		if len(scratch.seen) == 0 {
			return
		}
	}
	c.add(endpoint, status, pointer, "composition", "error",
		fmt.Sprintf("matches none of the %d documented %s variants", len(members), node.Composition.Kind))
}

func (c *checker) suppressed(pointer string) bool {
	if _, hit := c.redacted[pointer]; hit {
		c.report.Unverifiable++
		return true
	}
	return false
}

func (c *checker) add(endpoint *ir.Endpoint, status int, pointer, code, severity, message string) {
	c.addObserved(endpoint, status, pointer, code, severity, message, "")
}

func (c *checker) addObserved(endpoint *ir.Endpoint, status int, pointer, code, severity, message, observed string) {
	key := strings.Join([]string{endpoint.Method.Value, endpoint.PathTemplate.Value,
		fmt.Sprint(status), pointer, code}, "|")
	if v, ok := c.seen[key]; ok {
		v.Occurrences++
		return
	}
	c.seen[key] = &Violation{
		Method: strings.ToUpper(endpoint.Method.Value), Template: endpoint.PathTemplate.Value,
		Status: status, Pointer: pointer, Code: code, Severity: severity,
		Message: message, Occurrences: 1, Observed: observed,
	}
	c.order = append(c.order, key)
}

func matchEndpoint(def *ir.ApiDefinition, method, template string) *ir.Endpoint {
	segs := strings.Split(strings.Trim(template, "/"), "/")
	for i := range def.Endpoints {
		e := &def.Endpoints[i]
		if !strings.EqualFold(e.Method.Value, method) {
			continue
		}
		specSegs := strings.Split(strings.Trim(e.PathTemplate.Value, "/"), "/")
		if len(specSegs) != len(segs) {
			continue
		}
		match := true
		for j := range specSegs {
			if strings.Contains(specSegs[j], "{") || strings.Contains(segs[j], "{") {
				continue
			}
			if specSegs[j] != segs[j] {
				match = false
				break
			}
		}
		if match {
			return e
		}
	}
	return nil
}

func declaredResponse(endpoint *ir.Endpoint, status int) *ir.ResponseDef {
	code := fmt.Sprint(status)
	rangeCode := code[:1] + "XX"
	for i := range endpoint.Responses {
		if endpoint.Responses[i].StatusCode == code {
			return &endpoint.Responses[i]
		}
	}
	for i := range endpoint.Responses {
		if strings.EqualFold(endpoint.Responses[i].StatusCode, rangeCode) {
			return &endpoint.Responses[i]
		}
	}
	for i := range endpoint.Responses {
		if strings.EqualFold(endpoint.Responses[i].StatusCode, "default") {
			return &endpoint.Responses[i]
		}
	}
	return nil
}

func jsonSchemaOf(resp *ir.ResponseDef) *ir.IrSchemaNode {
	for i := range resp.Content {
		if strings.Contains(strings.ToLower(resp.Content[i].MediaType), "json") {
			return &resp.Content[i].Schema
		}
	}
	return nil
}

func scalarClass(t string) string {
	switch t {
	case "integer", "number":
		return "number"
	case "string", "boolean", "object", "array":
		return t
	default:
		return ""
	}
}

func jsonClass(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64, json.Number:
		return "number"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return ""
	}
}

func stringEnum(vals []any) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		s, ok := v.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func lastPointerSegment(pointer string) string {
	if i := strings.LastIndexByte(pointer, '/'); i >= 0 {
		return pointer[i+1:]
	}
	return pointer
}

func stripQuery(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}
