package importer

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

const (
	swagger2ConverterName    = "swagger2openapi"
	swagger2ConverterVersion = "7.0.8"
)

func isSwagger2Document(doc any) bool {
	m, ok := doc.(*OrdMap)
	if !ok {
		return false
	}
	s, ok := m.GetOr("swagger").(string)
	return ok && s == "2.0"
}

func convertSwagger2ToOpenAPI(parsed any, limits ParseLimits) (*OrdMap, error) {
	if !isSwagger2Document(parsed) {
		return nil, specErr(SpecConversionFailed, "not a Swagger 2.0 document")
	}
	c := &s2o{orig: parsed.(*OrdMap), limits: limits}
	out, err := c.convert()
	if err != nil {
		if spec, ok := err.(*SpecError); ok && spec.Code == SpecConversionFailed {
			return nil, err
		}
		return nil, specErr(SpecConversionFailed, fmt.Sprintf("Swagger 2.0 conversion failed: %v", err))
	}
	return out, nil
}

type s2o struct {
	orig           *OrdMap
	openapi        *OrdMap
	limits         ParseLimits
	componentNames map[string]string
}

func convFail(format string, args ...any) error {
	return specErr(SpecConversionFailed, fmt.Sprintf("Swagger 2.0 conversion failed: "+format, args...))
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && x == x
	default:
		return true
	}
}

var parameterTypeProperties = []string{
	"format", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
	"minLength", "maxLength", "multipleOf", "minItems", "maxItems",
	"uniqueItems", "minProperties", "maxProperties", "additionalProperties",
	"pattern", "enum", "default",
}

var httpMethodsLower = []string{"get", "post", "put", "delete", "patch", "head", "options", "trace"}

func isHTTPMethodLower(s string) bool {
	for _, m := range httpMethodsLower {
		if s == m {
			return true
		}
	}
	return false
}

var sanitiseBadChars = regexp.MustCompile(`[^A-Za-z0-9_\-\.]+|\s+`)

func sanitise(s string) string {
	s = strings.Replace(s, "[]", "Array", 1)
	components := strings.Split(s, "/")
	components[0] = sanitiseBadChars.ReplaceAllString(components[0], "_")
	return strings.Join(components, "/")
}

func sanitiseAll(s string) string {
	return sanitise(strings.Join(strings.Split(s, "/"), "_"))
}

func decodeURIComponent(s string) string {
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}

func (c *s2o) convert() (*OrdMap, error) {

	c.openapi = NewOrdMap()
	c.openapi.Set("openapi", "3.0.0")
	for _, k := range c.orig.Keys() {
		c.openapi.Set(k, deepClone(c.orig.GetOr(k)))
	}
	c.openapi.Delete("swagger")

	deleteNulls(c.openapi, "#")

	c.buildServers()
	c.openapi.Delete("host")
	c.openapi.Delete("basePath")
	if xs, ok := c.openapi.GetOr("x-servers").([]any); ok {
		c.openapi.Set("servers", xs)
		c.openapi.Delete("x-servers")
	}

	if err := c.fixInfo(); err != nil {
		return nil, err
	}
	if !c.openapi.Has("paths") {
		c.openapi.Set("paths", NewOrdMap())
	}

	if s, ok := c.openapi.GetOr("consumes").(string); ok {
		c.openapi.Set("consumes", []any{s})
	}
	if s, ok := c.openapi.GetOr("produces").(string); ok {
		c.openapi.Set("produces", []any{s})
	}

	components := NewOrdMap()
	c.openapi.Set("components", components)
	if v, ok := c.openapi.Get("x-callbacks"); ok {
		components.Set("callbacks", v)
		c.openapi.Delete("x-callbacks")
	}
	components.Set("examples", NewOrdMap())
	components.Set("headers", NewOrdMap())
	if v, ok := c.openapi.Get("x-links"); ok {
		components.Set("links", v)
		c.openapi.Delete("x-links")
	}
	components.Set("parameters", getOrNewMap(c.openapi, "parameters"))
	components.Set("responses", getOrNewMap(c.openapi, "responses"))
	components.Set("requestBodies", NewOrdMap())
	components.Set("securitySchemes", getOrNewMap(c.openapi, "securityDefinitions"))
	components.Set("schemas", getOrNewMap(c.openapi, "definitions"))
	c.openapi.Delete("definitions")
	c.openapi.Delete("responses")
	c.openapi.Delete("parameters")
	c.openapi.Delete("securityDefinitions")

	if err := c.main(); err != nil {
		return nil, err
	}
	return c.openapi, nil
}

func getOrNewMap(m *OrdMap, key string) *OrdMap {
	if sub, ok := m.GetOr(key).(*OrdMap); ok {
		return sub
	}
	return NewOrdMap()
}

func (c *s2o) components() *OrdMap { return getOrNewMap(c.openapi, "components") }

func deleteNulls(node any, path string) {
	switch m := node.(type) {
	case *OrdMap:
		for _, key := range append([]string(nil), m.Keys()...) {
			v, present := m.Get(key)
			if !present {
				continue
			}
			childPath := path + "/" + jpescape(key)
			if v == nil {
				if !strings.HasPrefix(key, "x-") && key != "default" && !strings.Contains(childPath, "/example") {
					m.Delete(key)
				}
				continue
			}
			deleteNulls(v, childPath)
		}
	case []any:
		for i, item := range m {
			deleteNulls(item, path+"/"+strconv.Itoa(i))
		}
	}
}

func (c *s2o) buildServers() {
	host, _ := c.orig.GetOr("host").(string)
	basePath, _ := c.orig.GetOr("basePath").(string)
	basePath = strings.TrimSuffix(basePath, "/")
	if host != "" {
		schemes := []any{""}
		if arr, ok := c.orig.GetOr("schemes").([]any); ok {
			schemes = arr
		}
		for _, sRaw := range schemes {
			s, _ := sRaw.(string)
			server := NewOrdMap()
			u := "//" + host + basePath
			if s != "" {
				u = s + ":" + u
			}
			server.Set("url", u)
			extractServerParameters(server)
			c.appendServer(server)
		}
	} else if bp, ok := c.orig.GetOr("basePath").(string); ok && truthy(bp) {
		server := NewOrdMap()
		server.Set("url", bp)
		extractServerParameters(server)
		c.appendServer(server)
	}
}

func (c *s2o) appendServer(server *OrdMap) {
	servers, _ := c.openapi.GetOr("servers").([]any)
	c.openapi.Set("servers", append(servers, server))
}

var serverVarRe = regexp.MustCompile(`\{(.+?)\}`)

func extractServerParameters(server *OrdMap) {
	u, ok := server.GetOr("url").(string)
	if !ok || u == "" {
		return
	}
	u = strings.ReplaceAll(u, "{{", "{")
	u = strings.ReplaceAll(u, "}}", "}")
	server.Set("url", u)
	for _, m := range serverVarRe.FindAllStringSubmatch(u, -1) {
		vars, ok := server.GetOr("variables").(*OrdMap)
		if !ok {
			vars = NewOrdMap()
			server.Set("variables", vars)
		}
		v := NewOrdMap()
		v.Set("default", "unknown")
		vars.Set(m[1], v)
	}
}

func (c *s2o) fixInfo() error {
	infoRaw, hasInfo := c.openapi.Get("info")
	if !hasInfo || infoRaw == nil {
		info := NewOrdMap()
		info.Set("version", "")
		info.Set("title", "")
		c.openapi.Set("info", info)
		infoRaw = info
	}
	info, ok := infoRaw.(*OrdMap)
	if !ok {
		return convFail("info must be an object")
	}
	if v, present := info.Get("title"); !present || v == nil {
		info.Set("title", "")
	}
	if v, present := info.Get("version"); !present || v == nil {
		info.Set("version", "")
	}
	switch v := info.GetOr("version").(type) {
	case string:
	case float64:
		info.Set("version", ir.FormatJSNumber(v))
	case bool:
		info.Set("version", fmt.Sprintf("%t", v))
	default:
		info.Set("version", fmt.Sprint(v))
	}
	if v, present := info.Get("logo"); present {
		info.Set("x-logo", v)
		info.Delete("logo")
	}
	if tos, present := info.Get("termsOfService"); present {
		if tos == nil {
			info.Set("termsOfService", "")
			tos = ""
		}
		if !jsURLValid(tos) {
			info.Delete("termsOfService")
		}
	}
	return nil
}

func jsURLValid(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return false
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss", "ftp":
		return u.Host != ""
	}
	return true
}

func (c *s2o) main() error {
	c.componentNames = map[string]string{}
	components := c.components()

	if sec, ok := c.openapi.GetOr("security").([]any); ok {
		processSecurity(sec)
	}

	securitySchemes := getOrNewMap(components, "securitySchemes")
	for _, s := range append([]string(nil), securitySchemes.Keys()...) {
		sname := sanitise(s)
		if s != sname {
			if securitySchemes.Has(sname) {
				return convFail("Duplicate sanitised securityScheme name %s", sname)
			}
			securitySchemes.Set(sname, securitySchemes.GetOr(s))
			securitySchemes.Delete(s)
		}
		if scheme, ok := securitySchemes.GetOr(sname).(*OrdMap); ok {
			c.processSecurityScheme(scheme)
		}
	}

	schemas := getOrNewMap(components, "schemas")
	for _, s := range append([]string(nil), schemas.Keys()...) {
		sname := sanitiseAll(s)
		suffix := ""
		if s != sname {
			n := 0
			for schemas.Has(sname + suffix) {
				n++
				if n == 1 {
					suffix = "2"
				} else {
					v, _ := strconv.Atoi(suffix)
					suffix = strconv.Itoa(v + 1)
				}
			}
			schemas.Set(sname+suffix, schemas.GetOr(s))
			schemas.Delete(s)
		}
		c.componentNames[s] = sname + suffix
		c.fixUpSchema(schemas.GetOr(sname + suffix))
	}

	if err := c.fixupRefs(c.openapi, func(v any) {}); err != nil {
		return err
	}

	parameters := getOrNewMap(components, "parameters")
	for _, p := range append([]string(nil), parameters.Keys()...) {
		sname := sanitise(p)
		if p != sname {
			if parameters.Has(sname) {
				return convFail("Duplicate sanitised parameter name %s", sname)
			}
			parameters.Set(sname, parameters.GetOr(p))
			parameters.Delete(p)
		}
		if _, err := c.processParameter(parameters.GetOr(sname), nil, nil, "", sname); err != nil {
			return err
		}
	}

	responses := getOrNewMap(components, "responses")
	for _, r := range append([]string(nil), responses.Keys()...) {
		sname := sanitise(r)
		if r != sname {
			if responses.Has(sname) {
				return convFail("Duplicate sanitised response name %s", sname)
			}
			responses.Set(sname, responses.GetOr(r))
			responses.Delete(r)
		}
		if err := c.processResponse(responses.GetOr(sname), nil); err != nil {
			return err
		}

	}

	if paths, ok := c.openapi.GetOr("paths").(*OrdMap); ok {
		if err := c.processPaths(paths); err != nil {
			return err
		}
	}

	for _, p := range append([]string(nil), parameters.Keys()...) {
		if param, ok := parameters.GetOr(p).(*OrdMap); ok && truthy(param.GetOr("x-s2o-delete")) {
			parameters.Delete(p)
		}
	}

	c.openapi.Delete("consumes")
	c.openapi.Delete("produces")
	c.openapi.Delete("schemes")

	components.Set("requestBodies", NewOrdMap())

	for _, key := range []string{"responses", "parameters", "examples", "requestBodies", "securitySchemes", "headers", "schemas"} {
		if m, ok := components.GetOr(key).(*OrdMap); ok && m.Len() == 0 {
			components.Delete(key)
		}
	}
	if components.Len() == 0 {
		c.openapi.Delete("components")
	}
	return nil
}

func processSecurity(security []any) {
	for _, sRaw := range security {
		s, ok := sRaw.(*OrdMap)
		if !ok {
			continue
		}
		for _, k := range append([]string(nil), s.Keys()...) {
			sname := sanitise(k)
			if k != sname {
				s.Set(sname, s.GetOr(k))
				s.Delete(k)
			}
		}
	}
}

func (c *s2o) processSecurityScheme(scheme *OrdMap) {
	if t, _ := scheme.GetOr("type").(string); t == "basic" {
		scheme.Set("type", "http")
		scheme.Set("scheme", "basic")
	}
	if t, _ := scheme.GetOr("type").(string); t == "oauth2" {
		flow := NewOrdMap()
		flowName, hasFlowName := scheme.GetOr("flow").(string)
		if !hasFlowName {
			flowName = "undefined"
		}
		switch flowName {
		case "application":
			flowName = "clientCredentials"
		case "accessCode":
			flowName = "authorizationCode"
		}
		if au, present := scheme.Get("authorizationUrl"); present {
			if s, ok := au.(string); ok {
				flow.Set("authorizationUrl", urlBeforeQuery(s))
			}
		}
		if tu, ok := scheme.GetOr("tokenUrl").(string); ok {
			flow.Set("tokenUrl", urlBeforeQuery(tu))
		}
		if scopes, ok := scheme.Get("scopes"); ok && truthy(scopes) {
			flow.Set("scopes", scopes)
		} else {
			flow.Set("scopes", NewOrdMap())
		}
		flows := NewOrdMap()
		flows.Set(flowName, flow)
		scheme.Set("flows", flows)
		scheme.Delete("flow")
		scheme.Delete("authorizationUrl")
		scheme.Delete("tokenUrl")
		scheme.Delete("scopes")
		if scheme.Has("name") {
			scheme.Delete("name")
		}
	}
}

func urlBeforeQuery(s string) string {
	out := strings.TrimSpace(strings.SplitN(s, "?", 2)[0])
	if out == "" {
		return "/"
	}
	return out
}

func (c *s2o) fixUpSchema(schema any) {
	c.walkSchema(schema, NewOrdMap(), map[*OrdMap]bool{})
}

func (c *s2o) walkSchema(schema any, parent *OrdMap, seen map[*OrdMap]bool) {
	m, isMap := schema.(*OrdMap)
	if !isMap {
		return
	}
	if m.Has("$ref") {

		return
	}
	c.fixUpSubSchemaExtensions(m)
	c.fixUpSubSchema(m, parent)
	if seen[m] {
		return
	}
	seen[m] = true

	if items, present := m.Get("items"); present && items != nil {
		c.walkSchema(items, m, seen)
	}
	if ai := m.GetOr("additionalItems"); truthy(ai) {
		if _, ok := ai.(*OrdMap); ok {
			c.walkSchema(ai, m, seen)
		}
	}
	if ap := m.GetOr("additionalProperties"); truthy(ap) {
		if _, ok := ap.(*OrdMap); ok {
			c.walkSchema(ap, m, seen)
		}
	}
	for _, key := range []string{"properties", "patternProperties"} {
		if props := m.GetOr(key); truthy(props) {
			for _, e := range objectEntries(props) {
				c.walkSchema(e.value, m, seen)
			}
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if members := m.GetOr(key); truthy(members) {
			for _, e := range objectEntries(members) {
				c.walkSchema(e.value, m, seen)
			}
		}
	}
	if not := m.GetOr("not"); truthy(not) {
		c.walkSchema(not, m, seen)
	}
}

func (c *s2o) fixUpSubSchemaExtensions(schema *OrdMap) {
	if xr, ok := schema.GetOr("x-required").([]any); ok && truthy(schema.GetOr("x-required")) {
		required, _ := schema.GetOr("required").([]any)
		schema.Set("required", append(append([]any{}, required...), xr...))
		schema.Delete("x-required")
	}
	for _, pair := range [][2]string{{"x-anyOf", "anyOf"}, {"x-oneOf", "oneOf"}, {"x-not", "not"}} {
		if v := schema.GetOr(pair[0]); truthy(v) {
			schema.Set(pair[1], v)
			schema.Delete(pair[0])
		}
	}
	if b, ok := schema.GetOr("x-nullable").(bool); ok {
		schema.Set("nullable", b)
		schema.Delete("x-nullable")
	}
	if xd, ok := schema.GetOr("x-discriminator").(*OrdMap); ok {
		if _, ok := xd.GetOr("propertyName").(string); ok {
			schema.Set("discriminator", xd)
			schema.Delete("x-discriminator")
			if mapping, ok := xd.GetOr("mapping").(*OrdMap); ok {
				for _, entry := range mapping.Keys() {
					if ref, ok := mapping.GetOr(entry).(string); ok && strings.HasPrefix(ref, "#/definitions/") {
						mapping.Set(entry, strings.Replace(ref, "#/definitions/", "#/components/schemas/", 1))
					}
				}
			}
		}
	}
}

func (c *s2o) fixUpSubSchema(schema *OrdMap, parent *OrdMap) {
	if d, ok := schema.GetOr("discriminator").(string); ok {
		disc := NewOrdMap()
		disc.Set("propertyName", d)
		schema.Set("discriminator", disc)
	}
	if items, ok := schema.GetOr("items").([]any); ok {
		switch len(items) {
		case 0:
			schema.Set("items", NewOrdMap())
		case 1:
			schema.Set("items", items[0])
		default:
			anyOf := NewOrdMap()
			anyOf.Set("anyOf", items)
			schema.Set("items", anyOf)
		}
	}

	if types, ok := schema.GetOr("type").([]any); ok {

		if len(types) == 0 {
			schema.Delete("type")
		} else {
			oneOf, _ := schema.GetOr("oneOf").([]any)
			hadOneOf := schema.Has("oneOf")
			for _, t := range types {
				if s, ok := t.(string); ok && s == "null" {
					schema.Set("nullable", true)
					continue
				}
				if truthy(t) {
					member := NewOrdMap()
					member.Set("type", t)
					oneOf = append(oneOf, member)
				}
			}
			schema.Delete("type")
			if len(oneOf) == 0 {
				if hadOneOf {
					schema.Delete("oneOf")
				}
			} else if len(oneOf) < 2 {
				if member, ok := oneOf[0].(*OrdMap); ok {
					if t, present := member.Get("type"); present {
						schema.Set("type", t)
					}
				}
				schema.Delete("oneOf")
			} else {
				schema.Set("oneOf", oneOf)
			}
		}
	}
	if t, ok := schema.GetOr("type").([]any); ok && len(t) == 1 {
		schema.Set("type", t[0])
	}

	if t, ok := schema.GetOr("type").(string); ok && t == "null" {
		schema.Delete("type")
		schema.Set("nullable", true)
	}
	if t, ok := schema.GetOr("type").(string); ok && t == "array" && !truthy(schema.GetOr("items")) {
		schema.Set("items", NewOrdMap())
	}
	if t, ok := schema.GetOr("type").(string); ok && t == "file" {
		schema.Set("type", "string")
		schema.Set("format", "binary")
	}
	if req, ok := schema.GetOr("required").(bool); ok {
		if req && truthy(schema.GetOr("name")) && parent != nil {
			if !parent.Has("required") {
				parent.Set("required", []any{})
			}
			if pr, ok := parent.GetOr("required").([]any); ok {
				parent.Set("required", append(pr, schema.GetOr("name")))
			}
		}
		schema.Delete("required")
	}
	if xml, ok := schema.GetOr("xml").(*OrdMap); ok {
		if ns, ok := xml.GetOr("namespace").(string); ok && ns == "" {
			xml.Delete("namespace")
		}
	}
	if schema.Has("allowEmptyValue") {
		schema.Delete("allowEmptyValue")
	}
}

func (c *s2o) fixupRefs(node any, setSelf func(any)) error {
	switch m := node.(type) {
	case *OrdMap:
		if ref, ok := m.GetOr("$ref").(string); ok {
			if err := c.rewriteRef(m, ref, setSelf); err != nil {
				return err
			}

			return nil
		}
		for _, key := range append([]string(nil), m.Keys()...) {
			k := key
			if err := c.fixupRefs(m.GetOr(k), func(v any) { m.Set(k, v) }); err != nil {
				return err
			}
		}
	case []any:
		for i := range m {
			idx := i
			if err := c.fixupRefs(m[idx], func(v any) { m[idx] = v }); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *s2o) rewriteRef(m *OrdMap, ref string, setSelf func(any)) error {
	switch {
	case strings.HasPrefix(ref, "#/components/"):

	case ref == "#/consumes":
		setSelf(deepClone(c.openapi.GetOr("consumes")))
		return nil
	case ref == "#/produces":
		setSelf(deepClone(c.openapi.GetOr("produces")))
		return nil
	case strings.HasPrefix(ref, "#/definitions/"):
		keys := strings.Split(strings.Replace(ref, "#/definitions/", "", 1), "/")
		if newKey, ok := c.componentNames[decodeURIComponent(jpunescape(keys[0]))]; ok {
			keys[0] = newKey
		}
		m.Set("$ref", "#/components/schemas/"+strings.Join(keys, "/"))
	case strings.HasPrefix(ref, "#/parameters/"):
		m.Set("$ref", "#/components/parameters/"+sanitise(strings.Replace(ref, "#/parameters/", "", 1)))
	case strings.HasPrefix(ref, "#/responses/"):
		m.Set("$ref", "#/components/responses/"+sanitise(strings.Replace(ref, "#/responses/", "", 1)))
	case strings.HasPrefix(ref, "#"):

	default:

	}
	if m.Len() > 1 {
		stripped := NewOrdMap()
		stripped.Set("$ref", m.GetOr("$ref"))
		setSelf(stripped)
	}
	return nil
}

func (c *s2o) jptrGet(pointer string) (any, bool) {
	prop := pointer
	if strings.Contains(prop, "#") {
		parts := strings.SplitN(prop, "#", 2)
		if parts[0] != "" {
			return nil, false
		}
		prop = parts[1]
		prop = decodeURIComponent(strings.ReplaceAll(strings.TrimPrefix(prop, "/"), "+", " "))
	}
	prop = strings.TrimPrefix(prop, "/")
	var current any = c.openapi
	for _, raw := range strings.Split(prop, "/") {
		component := jpunescape(raw)
		switch cur := current.(type) {
		case []any:
			idx, err := strconv.Atoi(component)
			if err != nil || strconv.Itoa(idx) != component || idx < 0 || idx >= len(cur) {
				return nil, false
			}
			current = cur[idx]
		case *OrdMap:
			v, ok := cur.Get(component)
			if !ok {
				return nil, false
			}
			current = v
		default:
			return nil, false
		}
	}
	return current, true
}

func fixParamRef(param *OrdMap) {
	ref, _ := param.GetOr("$ref").(string)
	if idx := strings.Index(ref, "#/parameters/"); idx >= 0 {
		rest := ref[idx+len("#/parameters/"):]
		param.Set("$ref", ref[:idx]+"#/components/parameters/"+sanitise(rest))
	}

}

func attachRequestBody(op *OrdMap) *OrdMap {
	newOp := NewOrdMap()
	for _, key := range op.Keys() {
		newOp.Set(key, op.GetOr(key))
		if key == "parameters" {
			newOp.Set("requestBody", NewOrdMap())
		}
	}
	newOp.Set("requestBody", NewOrdMap())
	return newOp
}

func (c *s2o) consumesFor(op *OrdMap) []string {
	if op != nil {
		if s, ok := op.GetOr("consumes").(string); ok {
			op.Set("consumes", []any{s})
		}
	}
	if _, ok := c.openapi.GetOr("consumes").([]any); !ok {
		c.openapi.Delete("consumes")
	}
	var raw []any
	if op != nil {
		if arr, ok := op.GetOr("consumes").([]any); ok {
			raw = arr
		}
	}
	if raw == nil {
		if arr, ok := c.openapi.GetOr("consumes").([]any); ok {
			raw = arr
		}
	}
	return uniqueStrings(raw)
}

func uniqueStrings(raw []any) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range raw {
		if s, ok := v.(string); ok && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func (c *s2o) processParameter(paramAny any, op *OrdMap, path *OrdMap, method string, index string) (*OrdMap, error) {
	result := NewOrdMap()
	singularRequestBody := true
	originalType := ""

	consumes := c.consumesFor(op)

	param, paramIsMap := paramAny.(*OrdMap)

	if paramIsMap {
		if ref, ok := param.GetOr("$ref").(string); ok {

			fixParamRef(param)
			ref, _ = param.GetOr("$ref").(string)
			ptr := decodeURIComponent(strings.Replace(ref, "#/components/parameters/", "", 1))
			rbody := false
			target, hasTarget := getOrNewMap(c.components(), "parameters").Get(ptr)
			targetDeleted := false
			if tm, ok := target.(*OrdMap); ok && truthy(tm.GetOr("x-s2o-delete")) {
				targetDeleted = true
			}
			if (!hasTarget || !truthy(target) || targetDeleted) && strings.HasPrefix(ref, "#/") {

				param.Set("x-s2o-delete", true)
				rbody = true
			}
			if rbody {
				newParam, found := c.jptrGet(ref)
				if !found && strings.HasPrefix(ref, "#/") {

				} else if found && newParam != nil {
					if np, ok := newParam.(*OrdMap); ok {
						param = np
					}
				}
			}
		}
	}

	if param != nil && (truthy(param.GetOr("name")) || truthy(param.GetOr("in"))) {
		if b, ok := param.GetOr("x-deprecated").(bool); ok {
			param.Set("deprecated", b)
			param.Delete("x-deprecated")
		}
		if xe, present := param.Get("x-example"); present {
			param.Set("example", xe)
			param.Delete("x-example")
		}

		in, _ := param.GetOr("in").(string)
		if in != "body" && !truthy(param.GetOr("type")) {
			param.Set("type", "string")
		}
		if tm, ok := param.GetOr("type").(*OrdMap); ok {
			if tref, ok := tm.GetOr("$ref").(string); ok {
				if resolved, found := c.jptrGet(tref); found {
					param.Set("type", resolved)
				}
			}
		}
		if t, _ := param.GetOr("type").(string); t == "file" {
			param.Set("x-s2o-originalType", t)
			originalType = t
		}
		if dm, ok := param.GetOr("description").(*OrdMap); ok {
			if dref, ok := dm.GetOr("$ref").(string); ok {
				if resolved, found := c.jptrGet(dref); found {
					param.Set("description", resolved)
				}
			}
		}
		if d, present := param.Get("description"); present && d == nil {
			param.Delete("description")
		}

		paramType, _ := param.GetOr("type").(string)
		oldCollectionFormat, _ := param.GetOr("collectionFormat").(string)
		if paramType == "array" && oldCollectionFormat == "" {
			oldCollectionFormat = "csv"
		}
		if oldCollectionFormat != "" {
			if paramType != "array" {
				param.Delete("collectionFormat")
			}
			if oldCollectionFormat == "csv" && (in == "query" || in == "cookie") {
				param.Set("style", "form")
				param.Set("explode", false)
			}
			if oldCollectionFormat == "csv" && (in == "path" || in == "header") {
				param.Set("style", "simple")
			}
			if oldCollectionFormat == "ssv" && in == "query" {
				param.Set("style", "spaceDelimited")
			}
			if oldCollectionFormat == "pipes" && in == "query" {
				param.Set("style", "pipeDelimited")
			}
			if oldCollectionFormat == "multi" {
				param.Set("explode", true)
			}
			if oldCollectionFormat == "tsv" {
				param.Set("x-collectionFormat", "tsv")
			}
			param.Delete("collectionFormat")
		}

		if truthy(param.GetOr("type")) && paramType != "body" && in != "formData" {
			if truthy(param.GetOr("items")) && truthy(param.GetOr("schema")) {

			} else {
				schema, ok := param.GetOr("schema").(*OrdMap)
				if !ok {
					schema = NewOrdMap()
					param.Set("schema", schema)
				}
				schema.Set("type", param.GetOr("type"))
				if items, present := param.Get("items"); present && truthy(items) {
					schema.Set("items", items)
					param.Delete("items")
					stripNestedCollectionFormats(items)
				}
				for _, prop := range parameterTypeProperties {
					if v, present := param.Get(prop); present {
						schema.Set(prop, v)
					}
					param.Delete(prop)
				}
			}
		}

		if schema := param.GetOr("schema"); truthy(schema) {
			c.fixUpSchema(schema)
		}
		if truthy(param.GetOr("x-ms-skip-url-encoding")) && in == "query" {
			param.Set("allowReserved", true)
			param.Delete("x-ms-skip-url-encoding")
		}
	}

	in := ""
	if param != nil {
		in, _ = param.GetOr("in").(string)
	}
	paramType := ""
	if param != nil {
		paramType, _ = param.GetOr("type").(string)
	}

	if param != nil && in == "formData" {

		singularRequestBody = false
		content := NewOrdMap()
		result.Set("content", content)
		contentType := "application/x-www-form-urlencoded"
		if len(consumes) > 0 && slices.Contains(consumes, "multipart/form-data") {
			contentType = "multipart/form-data"
		}
		ct := NewOrdMap()
		content.Set(contentType, ct)
		if pschema := param.GetOr("schema"); truthy(pschema) {
			ct.Set("schema", pschema)
		} else {
			schema := NewOrdMap()
			ct.Set("schema", schema)
			schema.Set("type", "object")
			properties := NewOrdMap()
			schema.Set("properties", properties)
			name, _ := param.GetOr("name").(string)
			target := NewOrdMap()
			properties.Set(name, target)
			if d := param.GetOr("description"); truthy(d) {
				target.Set("description", d)
			}
			if e := param.GetOr("example"); truthy(e) {
				target.Set("example", e)
			}
			if t := param.GetOr("type"); truthy(t) {
				target.Set("type", t)
			}
			for _, prop := range parameterTypeProperties {
				if v, present := param.Get(prop); present {
					target.Set(prop, v)
				}
			}
			if r, ok := param.GetOr("required").(bool); ok && r {
				required, _ := schema.GetOr("required").([]any)
				schema.Set("required", append(required, any(name)))
				result.Set("required", true)
			}
			if d, present := param.Get("default"); present {
				target.Set("default", d)
			}
			if allOf := param.GetOr("allOf"); truthy(allOf) {
				target.Set("allOf", allOf)
			}
			if paramType == "array" {
				if items := param.GetOr("items"); truthy(items) {
					target.Set("items", items)
					if im, ok := items.(*OrdMap); ok {
						im.Delete("collectionFormat")
					}
				}
			}
			if originalType == "file" || param.GetOr("x-s2o-originalType") == "file" {
				target.Set("type", "string")
				target.Set("format", "binary")
			}
			copyExtensions(param, target)
		}
	} else if param != nil && paramType == "file" {

		if r := param.GetOr("required"); truthy(r) {
			result.Set("required", r)
		}
		content := NewOrdMap()
		result.Set("content", content)
		ct := NewOrdMap()
		content.Set("application/octet-stream", ct)
		schema := NewOrdMap()
		ct.Set("schema", schema)
		schema.Set("type", "string")
		schema.Set("format", "binary")
		copyExtensions(param, result)
	}
	if param != nil && in == "body" {
		content := NewOrdMap()
		result.Set("content", content)
		if d := param.GetOr("description"); truthy(d) {
			result.Set("description", d)
		}
		if r := param.GetOr("required"); truthy(r) {
			result.Set("required", r)
		}
		if len(consumes) == 0 {
			consumes = []string{"application/json"}
		}
		for _, mimetype := range consumes {
			ct := NewOrdMap()
			content.Set(mimetype, ct)
			var schemaClone any = NewOrdMap()
			if s := param.GetOr("schema"); truthy(s) {
				schemaClone = deepClone(s)
			}
			ct.Set("schema", schemaClone)
			c.fixUpSchema(schemaClone)
		}
		copyExtensions(param, result)
	}

	if result.Len() > 0 {
		if param != nil {
			param.Set("x-s2o-delete", true)
		}

		if op != nil {
			if truthy(op.GetOr("requestBody")) && singularRequestBody {
				if rb, ok := op.GetOr("requestBody").(*OrdMap); ok {
					rb.Set("x-s2o-overloaded", true)
				}

			} else {
				if !truthy(op.GetOr("requestBody")) {
					op = attachRequestBody(op)
					if path != nil && method != "" {
						path.Set(method, op)
					}
				}
				opRB, _ := op.GetOr("requestBody").(*OrdMap)
				resultContent, _ := result.GetOr("content").(*OrdMap)
				if merged := mergeFormContent(opRB, resultContent, "multipart/form-data"); !merged {
					if merged := mergeFormContent(opRB, resultContent, "application/x-www-form-urlencoded"); !merged {
						for _, k := range result.Keys() {
							opRB.Set(k, result.GetOr(k))
						}
					}
				}
			}
		}
	}

	if param != nil && !truthy(param.GetOr("x-s2o-delete")) {
		param.Delete("type")
		for _, prop := range parameterTypeProperties {
			param.Delete(prop)
		}
		if in == "path" {
			if r, ok := param.GetOr("required").(bool); !ok || !r {
				param.Set("required", true)
			}
		}
	}

	return op, nil
}

func mergeFormContent(opRB *OrdMap, resultContent *OrdMap, contentType string) bool {
	if opRB == nil || resultContent == nil {
		return false
	}
	opContent, ok := opRB.GetOr("content").(*OrdMap)
	if !ok {
		return false
	}
	opCT, ok := opContent.GetOr(contentType).(*OrdMap)
	if !ok {
		return false
	}
	opSchema, ok := opCT.GetOr("schema").(*OrdMap)
	if !ok {
		return false
	}
	opProps, ok := opSchema.GetOr("properties").(*OrdMap)
	if !ok {
		return false
	}
	resCT, ok := resultContent.GetOr(contentType).(*OrdMap)
	if !ok {
		return false
	}
	resSchema, ok := resCT.GetOr("schema").(*OrdMap)
	if !ok {
		return false
	}
	resProps, ok := resSchema.GetOr("properties").(*OrdMap)
	if !ok {
		return false
	}
	for _, k := range resProps.Keys() {
		opProps.Set(k, resProps.GetOr(k))
	}
	opReq, _ := opSchema.GetOr("required").([]any)
	resReq, _ := resSchema.GetOr("required").([]any)
	combined := append(append([]any{}, opReq...), resReq...)
	if len(combined) == 0 {
		opSchema.Delete("required")
	} else {
		opSchema.Set("required", combined)
	}
	return true
}

func stripNestedCollectionFormats(node any) {
	switch m := node.(type) {
	case *OrdMap:
		for _, key := range append([]string(nil), m.Keys()...) {
			if key == "collectionFormat" {
				if _, ok := m.GetOr(key).(string); ok {
					m.Delete(key)
					continue
				}
			}
			stripNestedCollectionFormats(m.GetOr(key))
		}
	case []any:
		for _, item := range m {
			stripNestedCollectionFormats(item)
		}
	}
}

func copyExtensions(src, tgt *OrdMap) {
	for _, prop := range src.Keys() {
		if strings.HasPrefix(prop, "x-") && !strings.HasPrefix(prop, "x-s2o") {
			tgt.Set(prop, src.GetOr(prop))
		}
	}
}

func (c *s2o) processResponse(responseAny any, op *OrdMap) error {
	response, ok := responseAny.(*OrdMap)
	if !ok {
		return nil
	}
	if ref, ok := response.GetOr("$ref").(string); ok {
		if strings.Contains(ref, "#/definitions/") {

		} else if strings.HasPrefix(ref, "#/responses/") {
			response.Set("$ref", "#/components/responses/"+sanitise(decodeURIComponent(strings.Replace(ref, "#/responses/", "", 1))))
		}
		return nil
	}

	if d, present := response.Get("description"); !present || d == nil || d == "" {

		response.Set("description", "")
	}

	if response.Has("schema") {
		c.fixUpSchema(response.GetOr("schema"))
		if sm, ok := response.GetOr("schema").(*OrdMap); ok {
			if sref, ok := sm.GetOr("$ref").(string); ok && strings.HasPrefix(sref, "#/responses/") {
				sm.Set("$ref", "#/components/responses/"+sanitise(decodeURIComponent(strings.Replace(sref, "#/responses/", "", 1))))
			}
		}
		if op != nil {
			if s, ok := op.GetOr("produces").(string); ok {
				op.Set("produces", []any{s})
			}
		}
		if p, present := c.openapi.Get("produces"); present && truthy(p) {
			if _, ok := p.([]any); !ok {
				c.openapi.Delete("produces")
			}
		}
		var producesRaw []any
		if op != nil {
			if arr, ok := op.GetOr("produces").([]any); ok {
				producesRaw = arr
			}
		}
		if producesRaw == nil {
			if arr, ok := c.openapi.GetOr("produces").([]any); ok {
				producesRaw = arr
			}
		}
		produces := uniqueStrings(producesRaw)
		if len(produces) == 0 {
			produces = []string{"*/*"}
		}

		content := NewOrdMap()
		response.Set("content", content)
		examples, _ := response.GetOr("examples").(*OrdMap)
		for _, mimetype := range produces {
			ct := NewOrdMap()
			content.Set(mimetype, ct)
			ct.Set("schema", deepClone(response.GetOr("schema")))
			if examples != nil {
				if ex, present := examples.Get(mimetype); present && truthy(ex) {
					wrapper := NewOrdMap()
					wrapper.Set("value", ex)
					exs := NewOrdMap()
					exs.Set("response", wrapper)
					ct.Set("examples", exs)
					examples.Delete(mimetype)
				}
			}
			if sm, ok := ct.GetOr("schema").(*OrdMap); ok {
				if t, ok := sm.GetOr("type").(string); ok && t == "file" {
					fileSchema := NewOrdMap()
					fileSchema.Set("type", "string")
					fileSchema.Set("format", "binary")
					ct.Set("schema", fileSchema)
				}
			}
		}
		response.Delete("schema")
	}

	if examples, ok := response.GetOr("examples").(*OrdMap); ok {
		for _, mimetype := range examples.Keys() {
			content, ok := response.GetOr("content").(*OrdMap)
			if !ok {
				content = NewOrdMap()
				response.Set("content", content)
			}
			ct, ok := content.GetOr(mimetype).(*OrdMap)
			if !ok {
				ct = NewOrdMap()
				content.Set(mimetype, ct)
			}
			wrapper := NewOrdMap()
			wrapper.Set("value", examples.GetOr(mimetype))
			exs := NewOrdMap()
			exs.Set("response", wrapper)
			ct.Set("examples", exs)
		}
	}
	response.Delete("examples")

	return nil
}

func (c *s2o) processPaths(container *OrdMap) error {
	for _, p := range append([]string(nil), container.Keys()...) {
		path, ok := container.GetOr(p).(*OrdMap)
		if !ok {
			continue
		}
		if v, ok := path.GetOr("x-trace").(*OrdMap); ok {
			path.Set("trace", v)
			path.Delete("x-trace")
		}
		if v, ok := path.GetOr("x-summary").(string); ok {
			path.Set("summary", v)
			path.Delete("x-summary")
		}
		if v, ok := path.GetOr("x-description").(string); ok {
			path.Set("description", v)
			path.Delete("x-description")
		}
		if v, ok := path.GetOr("x-servers").([]any); ok {
			path.Set("servers", v)
			path.Delete("x-servers")
		}
		for _, method := range append([]string(nil), path.Keys()...) {

			if !isHTTPMethodLower(method) {
				continue
			}
			op, opIsMap := path.GetOr(method).(*OrdMap)

			if opIsMap {
				if opParams, ok := op.GetOr("parameters").([]any); ok {
					if pathParamsRaw := path.GetOr("parameters"); truthy(pathParamsRaw) {
						pathParams, ok := pathParamsRaw.([]any)
						if !ok {
							return convFail("path parameters must be an array")
						}
						for _, pparamRaw := range pathParams {
							local := pparamRaw
							if lm, ok := local.(*OrdMap); ok {
								if _, ok := lm.GetOr("$ref").(string); ok {
									fixParamRef(lm)
									ref, _ := lm.GetOr("$ref").(string)
									resolved, found := c.jptrGet(ref)
									if !found {
										return convFail("cannot resolve path parameter reference %q", ref)
									}
									local = resolved
								}
							}
							localMap, _ := local.(*OrdMap)
							if localMap == nil {
								return convFail("path parameter did not resolve to an object")
							}
							match := false
							for _, e := range opParams {
								em, ok := e.(*OrdMap)
								if !ok {
									continue
								}
								if jsEqual(em.GetOr("name"), localMap.GetOr("name")) && jsEqual(em.GetOr("in"), localMap.GetOr("in")) {
									match = true
									break
								}
							}
							localIn, _ := localMap.GetOr("in").(string)
							localType, _ := localMap.GetOr("type").(string)
							if !match && (localIn == "formData" || localIn == "body" || localType == "file") {
								var err error
								op, err = c.processParameter(localMap, op, path, method, p)
								if err != nil {
									return err
								}
							}
						}
					}
					for _, paramRaw := range opParams {
						var err error
						op, err = c.processParameter(paramRaw, op, path, method, method+":"+p)
						if err != nil {
							return err
						}
					}
					if cur, ok := op.GetOr("parameters").([]any); ok {
						op.Set("parameters", filterKeepParameters(cur))
					}
				}

				if sec, ok := op.GetOr("security").([]any); ok && truthy(op.GetOr("security")) {
					processSecurity(sec)
				}

				if !truthy(op.GetOr("responses")) {
					defaultResp := NewOrdMap()
					defaultResp.Set("description", "Default response")
					responses := NewOrdMap()
					responses.Set("default", defaultResp)
					op.Set("responses", responses)
				}
				if responses, ok := op.GetOr("responses").(*OrdMap); ok {
					for _, r := range append([]string(nil), responses.Keys()...) {
						if err := c.processResponse(responses.GetOr(r), op); err != nil {
							return err
						}
					}
				}

				op.Delete("consumes")
				op.Delete("produces")
				op.Delete("schemes")

				if params, ok := op.GetOr("parameters").([]any); ok && len(params) == 0 {
					op.Delete("parameters")
				}

			}
		}
		if pathParams := path.GetOr("parameters"); truthy(pathParams) {
			for _, e := range objectEntries(pathParams) {
				if _, err := c.processParameter(e.value, nil, path, "", p); err != nil {
					return err
				}
			}
			if arr, ok := pathParams.([]any); ok {
				path.Set("parameters", filterKeepParameters(arr))
			}
		}
	}
	return nil
}

func jsEqual(a, b any) bool {
	switch a.(type) {
	case nil, bool, float64, string, int:
		switch b.(type) {
		case nil, bool, float64, string, int:
			return a == b
		}
	}
	return false
}

func filterKeepParameters(params []any) []any {
	out := []any{}
	for _, v := range params {
		if !truthy(v) {
			continue
		}
		if m, ok := v.(*OrdMap); ok && truthy(m.GetOr("x-s2o-delete")) {
			continue
		}
		out = append(out, v)
	}
	return out
}
