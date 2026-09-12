// Postman collection (v2.0/v2.1) → OpenAPI 3.0, natively: the converters we
// evaluated dropped the auth scheme and documented error responses.
package importer

import (
	"strconv"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

const (
	postmanConverterName    = "pikopod-postman"
	postmanConverterVersion = "1"
)

// NormalizePostman converts a Postman collection into the IR via the OpenAPI
// pipeline (one normalize path for every source kind).
func NormalizePostman(raw []byte) (*ir.ApiDefinition, error) {
	doc, err := parseJSONSafely(string(raw), DefaultParseLimits)
	if err != nil {
		return nil, userFacing(err)
	}
	root, ok := doc.(*OrdMap)
	if !ok {
		return nil, userFacing(&SpecError{Code: SpecParseError, Message: "Postman collection is not a JSON object"})
	}
	oas, err := convertPostmanToOpenAPI(root)
	if err != nil {
		return nil, userFacing(err)
	}
	def, err := normalizeOpenAPIValue(oas, DefaultParseLimits, "ACTIVE")
	if err != nil {
		return nil, userFacing(err)
	}
	def.NormalizerVersion = ir.NormalizerVersion + "+" + postmanConverterName + "@" + postmanConverterVersion
	return def, nil
}

type p2oState struct {
	paths       *OrdMap
	authScheme  *OrdMap // components.securitySchemes.<name>
	authName    string
	serverURL   string
	sawRequests int
}

func convertPostmanToOpenAPI(root *OrdMap) (*OrdMap, error) {
	st := &p2oState{paths: NewOrdMap()}

	info := NewOrdMap()
	title := "Imported Postman Collection"
	if im, ok := root.GetOr("info").(*OrdMap); ok {
		if n, ok := im.GetOr("name").(string); ok && n != "" {
			title = n
		}
	}
	info.Set("title", title)
	info.Set("version", "postman")

	// Collection-level auth applies to every operation unless overridden.
	if auth, ok := root.GetOr("auth").(*OrdMap); ok {
		st.applyAuth(auth)
	}

	items, _ := root.GetOr("item").([]any)
	if err := st.walkItems(items, nil); err != nil {
		return nil, err
	}
	if st.sawRequests == 0 {
		return nil, &SpecError{Code: SpecParseError, Message: "Postman collection contains no requests"}
	}

	oas := NewOrdMap()
	oas.Set("openapi", "3.0.0")
	oas.Set("info", info)
	if st.serverURL != "" {
		server := NewOrdMap()
		server.Set("url", st.serverURL)
		oas.Set("servers", []any{server})
	}
	oas.Set("paths", st.paths)
	if st.authScheme != nil {
		schemes := NewOrdMap()
		schemes.Set(st.authName, st.authScheme)
		components := NewOrdMap()
		components.Set("securitySchemes", schemes)
		oas.Set("components", components)
		req := NewOrdMap()
		req.Set(st.authName, []any{})
		oas.Set("security", []any{req})
	}
	return oas, nil
}

// applyAuth maps Postman auth blocks to a securityScheme; first one wins
// (collection-level, or the first authenticated request).
func (st *p2oState) applyAuth(auth *OrdMap) {
	if st.authScheme != nil {
		return
	}
	typ, _ := auth.GetOr("type").(string)
	scheme := NewOrdMap()
	switch typ {
	case "bearer":
		scheme.Set("type", "http")
		scheme.Set("scheme", "bearer")
		st.authName = "bearerAuth"
	case "basic":
		scheme.Set("type", "http")
		scheme.Set("scheme", "basic")
		st.authName = "basicAuth"
	case "apikey":
		scheme.Set("type", "apiKey")
		name, in := "X-Api-Key", "header"
		for _, kv := range authParams(auth, "apikey") {
			switch kv.key {
			case "key":
				if kv.value != "" {
					name = kv.value
				}
			case "in":
				if kv.value == "query" {
					in = "query"
				}
			}
		}
		scheme.Set("name", name)
		scheme.Set("in", in)
		st.authName = "apiKeyAuth"
	default:
		return // noauth / oauth flows: nothing enforceable to model
	}
	st.authScheme = scheme
}

type postmanKV struct{ key, value string }

// authParams reads auth.<type> in both shapes: v2.1 array of {key,value} and
// v2.0 object.
func authParams(auth *OrdMap, typ string) []postmanKV {
	var out []postmanKV
	switch block := auth.GetOr(typ).(type) {
	case []any:
		for _, e := range block {
			if em, ok := e.(*OrdMap); ok {
				k, _ := em.GetOr("key").(string)
				v, _ := em.GetOr("value").(string)
				out = append(out, postmanKV{k, v})
			}
		}
	case *OrdMap:
		for _, k := range block.Keys() {
			if v, ok := block.GetOr(k).(string); ok {
				out = append(out, postmanKV{k, v})
			}
		}
	}
	return out
}

func (st *p2oState) walkItems(items []any, folders []string) error {
	for _, it := range items {
		im, ok := it.(*OrdMap)
		if !ok {
			continue
		}
		name, _ := im.GetOr("name").(string)
		if sub, ok := im.GetOr("item").([]any); ok {
			if err := st.walkItems(sub, append(folders, name)); err != nil {
				return err
			}
			continue
		}
		if req, ok := im.GetOr("request").(*OrdMap); ok {
			st.addRequest(im, req, name, folders)
		}
	}
	return nil
}

func (st *p2oState) addRequest(item, req *OrdMap, name string, folders []string) {
	method, _ := req.GetOr("method").(string)
	method = strings.ToLower(method)
	switch method {
	case "get", "post", "put", "patch", "delete", "head", "options":
	default:
		return
	}
	path, pathParams, server := parsePostmanURL(req.GetOr("url"))
	if path == "" {
		return
	}
	if st.serverURL == "" && server != "" {
		st.serverURL = server
	}
	if auth, ok := req.GetOr("auth").(*OrdMap); ok {
		st.applyAuth(auth)
	}
	st.sawRequests++

	pathItem, _ := st.paths.GetOr(path).(*OrdMap)
	if pathItem == nil {
		pathItem = NewOrdMap()
		st.paths.Set(path, pathItem)
	}
	op, _ := pathItem.GetOr(method).(*OrdMap)
	fresh := op == nil
	if fresh {
		op = NewOrdMap()
		pathItem.Set(method, op)
	}

	if fresh {
		if name != "" {
			op.Set("summary", name)
		}
		if len(folders) > 0 {
			tags := make([]any, 0, len(folders))
			for _, f := range folders {
				if f != "" {
					tags = append(tags, f)
				}
			}
			if len(tags) > 0 {
				op.Set("tags", tags)
			}
		}

		var params []any
		for _, p := range pathParams {
			pm := NewOrdMap()
			pm.Set("name", p)
			pm.Set("in", "path")
			pm.Set("required", true)
			sch := NewOrdMap()
			sch.Set("type", "string")
			pm.Set("schema", sch)
			params = append(params, pm)
		}
		for _, q := range queryParamsOf(req.GetOr("url")) {
			pm := NewOrdMap()
			pm.Set("name", q)
			pm.Set("in", "query")
			sch := NewOrdMap()
			sch.Set("type", "string")
			pm.Set("schema", sch)
			params = append(params, pm)
		}
		if len(params) > 0 {
			op.Set("parameters", params)
		}

		if body := requestBodyOf(req); body != nil {
			op.Set("requestBody", body)
		}
	}

	// Responses merge across duplicate documentation of the same operation.
	responses, _ := op.GetOr("responses").(*OrdMap)
	if responses == nil {
		responses = NewOrdMap()
		op.Set("responses", responses)
	}
	addPostmanResponses(responses, item.GetOr("response"))
	if responses.Len() == 0 {
		// OpenAPI requires at least one response; an example-less request is
		// still an operation worth modelling.
		def := NewOrdMap()
		def.Set("description", "OK")
		responses.Set("200", def)
	}
}

// parsePostmanURL handles both URL shapes and returns the templated path,
// its parameters, and the server base (scheme://host) when present.
func parsePostmanURL(u any) (path string, params []string, server string) {
	var rawURL string
	switch v := u.(type) {
	case string:
		rawURL = v
	case *OrdMap:
		if r, ok := v.GetOr("raw").(string); ok {
			rawURL = r
		}
	default:
		return "", nil, ""
	}
	if rawURL == "" {
		return "", nil, ""
	}
	if i := strings.IndexAny(rawURL, "?#"); i >= 0 {
		rawURL = rawURL[:i]
	}
	// Split scheme://host from the path. `{{baseUrl}}/x` counts as a host.
	rest := rawURL
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		host := rest[:i]
		if !strings.Contains(host, "{{") && strings.Contains(rawURL, "://") {
			server = rawURL[:strings.Index(rawURL, "://")+3] + host
		}
		rest = rest[i:]
	} else {
		rest = "/"
	}

	segs := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	out := make([]string, 0, len(segs))
	seen := map[string]bool{}
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		var pname string
		switch {
		case strings.HasPrefix(seg, ":") && len(seg) > 1:
			pname = seg[1:]
		case strings.HasPrefix(seg, "{{") && strings.HasSuffix(seg, "}}"):
			pname = strings.Trim(seg, "{}")
		}
		if pname != "" {
			pname = sanitiseParamName(pname)
			// LOOP: a third occurrence (or two names sanitizing to empty) must
			// still get a unique name, or one template binds two positions.
			for pname == "" || seen[pname] {
				pname = pname + "_"
			}
			seen[pname] = true
			params = append(params, pname)
			out = append(out, "{"+pname+"}")
			continue
		}
		out = append(out, seg)
	}
	return "/" + strings.Join(out, "/"), params, server
}

func sanitiseParamName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func queryParamsOf(u any) []string {
	um, ok := u.(*OrdMap)
	if !ok {
		return nil
	}
	qs, ok := um.GetOr("query").([]any)
	if !ok {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, q := range qs {
		qm, ok := q.(*OrdMap)
		if !ok {
			continue
		}
		if k, ok := qm.GetOr("key").(string); ok && k != "" && !strings.Contains(k, "{{") && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// requestBodyOf turns a raw JSON example body into a requestBody with an
// inferred schema. Malformed examples are tolerated (no body, no crash).
func requestBodyOf(req *OrdMap) *OrdMap {
	bm, ok := req.GetOr("body").(*OrdMap)
	if !ok {
		return nil
	}
	raw, ok := bm.GetOr("raw").(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	schema := inferSchemaFromExample(raw)
	if schema == nil {
		return nil
	}
	// Request example fields become REQUIRED at the top level: an example is the
	// only evidence Postman offers, and providers reject creates missing them.
	markTopLevelRequired(schema)
	media := NewOrdMap()
	media.Set("schema", schema)
	content := NewOrdMap()
	content.Set("application/json", media)
	body := NewOrdMap()
	body.Set("content", content)
	return body
}

func addPostmanResponses(responses *OrdMap, rs any) {
	list, ok := rs.([]any)
	if !ok {
		return
	}
	for _, r := range list {
		rm, ok := r.(*OrdMap)
		if !ok {
			continue
		}
		code := ""
		switch c := rm.GetOr("code").(type) {
		case float64:
			code = itoaCode(int(c))
		case int:
			code = itoaCode(c)
		case string:
			code = c
		}
		if len(code) != 3 {
			continue
		}
		entry, _ := responses.GetOr(code).(*OrdMap)
		if entry == nil {
			entry = NewOrdMap()
			desc, _ := rm.GetOr("name").(string)
			if desc == "" {
				desc = "response"
			}
			entry.Set("description", desc)
			responses.Set(code, entry)
		}
		if entry.Has("content") {
			continue // first documented body per status wins
		}
		if raw, ok := rm.GetOr("body").(string); ok && strings.TrimSpace(raw) != "" {
			if schema := inferSchemaFromExample(raw); schema != nil {
				media := NewOrdMap()
				media.Set("schema", schema)
				content := NewOrdMap()
				content.Set("application/json", media)
				entry.Set("content", content)
			}
		}
	}
}

func itoaCode(c int) string {
	if c < 100 || c > 599 {
		return ""
	}
	return strconv.Itoa(c)
}

// markTopLevelRequired lists every top-level property as required (request
// bodies only; responses keep no requiredness claims).
func markTopLevelRequired(schema *OrdMap) {
	props, ok := schema.GetOr("properties").(*OrdMap)
	if !ok || props.Len() == 0 {
		return
	}
	required := make([]any, 0, props.Len())
	for _, k := range props.Keys() {
		required = append(required, k)
	}
	schema.Set("required", required)
}

// inferSchemaFromExample derives a schema from a JSON example — shapes only, no
// requiredness (an example cannot prove it); nil when the example is invalid.
func inferSchemaFromExample(raw string) *OrdMap {
	doc, err := parseJSONSafely(raw, detectLimits)
	if err != nil {
		return nil
	}
	return schemaOfExample(doc, 0)
}

const maxInferDepth = 12

func schemaOfExample(v any, depth int) *OrdMap {
	s := NewOrdMap()
	if depth > maxInferDepth {
		s.Set("type", "object")
		return s
	}
	switch t := v.(type) {
	case *OrdMap:
		s.Set("type", "object")
		props := NewOrdMap()
		for _, k := range t.Keys() {
			props.Set(k, schemaOfExample(t.GetOr(k), depth+1))
		}
		if props.Len() > 0 {
			s.Set("properties", props)
		}
	case []any:
		s.Set("type", "array")
		if len(t) > 0 {
			s.Set("items", schemaOfExample(t[0], depth+1))
		} else {
			s.Set("items", NewOrdMap())
		}
	case string:
		s.Set("type", "string")
	case bool:
		s.Set("type", "boolean")
	case float64:
		if t == float64(int64(t)) {
			s.Set("type", "integer")
		} else {
			s.Set("type", "number")
		}
	case nil:
		s.Set("nullable", true)
	default:
		s.Set("type", "string")
	}
	return s
}
