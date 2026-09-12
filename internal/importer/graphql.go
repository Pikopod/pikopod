// GraphQL (SDL or introspection JSON) → OpenAPI 3.0 → IR natively, so the
// existing sandbox machinery serves GraphQL unchanged. No auth is invented.
package importer

import (
	"fmt"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

const (
	graphqlConverterName    = "pikopod-graphql"
	graphqlConverterVersion = "1"

	graphqlMaxBytes  = 5 * 1024 * 1024
	graphqlMaxTypes  = 2000
	graphqlMaxDepth  = 32
	graphqlMaxTokens = 1_000_000
)

// NormalizeGraphQLSDL converts GraphQL SDL into the IR via the OpenAPI
// pipeline (one normalize path for every source kind).
func NormalizeGraphQLSDL(raw []byte) (*ir.ApiDefinition, error) {
	if len(raw) > graphqlMaxBytes {
		return nil, userFacing(specErr(SpecTooLarge, fmt.Sprintf("GraphQL SDL exceeds the %d-byte limit", graphqlMaxBytes)))
	}
	schema, err := parseGraphQLSDL(string(raw))
	if err != nil {
		return nil, userFacing(err)
	}
	return normalizeGraphQL(schema)
}

// NormalizeGraphQLIntrospection converts a GraphQL introspection result
// (`{"__schema": ...}` or `{"data": {"__schema": ...}}`) into the IR.
func NormalizeGraphQLIntrospection(raw []byte) (*ir.ApiDefinition, error) {
	if len(raw) > graphqlMaxBytes {
		return nil, userFacing(specErr(SpecTooLarge, fmt.Sprintf("GraphQL introspection JSON exceeds the %d-byte limit", graphqlMaxBytes)))
	}
	doc, err := parseJSONSafely(string(raw), DefaultParseLimits)
	if err != nil {
		return nil, userFacing(err)
	}
	root, ok := doc.(*OrdMap)
	if !ok {
		return nil, userFacing(specErr(SpecParseError, "GraphQL introspection JSON is not an object"))
	}
	schemaRaw := root.GetOr("__schema")
	if schemaRaw == nil {
		if data, ok := root.GetOr("data").(*OrdMap); ok {
			schemaRaw = data.GetOr("__schema")
		}
	}
	schemaMap, ok := schemaRaw.(*OrdMap)
	if !ok {
		return nil, userFacing(specErr(SpecParseError, "GraphQL introspection JSON is missing __schema"))
	}
	schema, specE := schemaFromIntrospection(schemaMap)
	if specE != nil {
		return nil, userFacing(specE)
	}
	return normalizeGraphQL(schema)
}

func normalizeGraphQL(schema *gqlSchema) (*ir.ApiDefinition, error) {
	oas, specE := graphqlToOpenAPI(schema)
	if specE != nil {
		return nil, userFacing(specE)
	}
	def, err := normalizeOpenAPIValue(oas, DefaultParseLimits, "ACTIVE")
	if err != nil {
		return nil, userFacing(err)
	}
	// The converter version participates in normalizerVersion and therefore in
	// the normalized hash, exactly like the Swagger 2.0 and Postman paths.
	def.SourceKind = "graphql"
	def.NormalizerVersion = ir.NormalizerVersion + "+" + graphqlConverterName + "@" + graphqlConverterVersion
	return def, nil
}

type gqlKind int

const (
	gqlObject gqlKind = iota
	gqlInterface
	gqlInput
	gqlEnum
	gqlUnion
	gqlScalar
)

// gqlTypeRef is a GraphQL type reference: a leaf named type or a list wrapper,
// either optionally non-null.
type gqlTypeRef struct {
	nonNull bool
	name    string      // leaf when elem == nil
	elem    *gqlTypeRef // list wrapper when non-nil
}

type gqlArg struct {
	name string
	typ  *gqlTypeRef
}

type gqlField struct {
	name        string
	description string
	args        []gqlArg
	typ         *gqlTypeRef
}

type gqlType struct {
	kind        gqlKind
	name        string
	description string
	fields      []gqlField // object | interface | input
	enumValues  []string   // enum
	members     []string   // union
}

type gqlSchema struct {
	description      string
	queryType        string
	mutationType     string
	subscriptionType string
	types            []*gqlType // declaration order
	byName           map[string]*gqlType
}

var gqlBuiltinScalars = map[string]string{
	"String":  "string",
	"ID":      "string",
	"Int":     "integer",
	"Float":   "number",
	"Boolean": "boolean",
}

// rootFields resolves a root operation type name to its fields, defaulting to
// the conventional Query/Mutation/Subscription names.
func (s *gqlSchema) rootFields(declared, conventional string) []gqlField {
	name := declared
	if name == "" {
		name = conventional
	}
	if t, ok := s.byName[name]; ok && t.kind == gqlObject {
		return t.fields
	}
	return nil
}

func gqlErr(line int, format string, args ...any) *SpecError {
	return &SpecError{Code: SpecParseError, Message: fmt.Sprintf("GraphQL SDL: %s (line %d)", fmt.Sprintf(format, args...), line)}
}

type gqlTokKind int

const (
	gqlTokEOF gqlTokKind = iota
	gqlTokName
	gqlTokPunct
	gqlTokString
	gqlTokNumber
)

type gqlToken struct {
	kind gqlTokKind
	val  string
	line int
}

func (k gqlTokKind) String() string {
	switch k {
	case gqlTokEOF:
		return "end of input"
	case gqlTokName:
		return "name"
	case gqlTokPunct:
		return "punctuator"
	case gqlTokString:
		return "string"
	case gqlTokNumber:
		return "number"
	}
	return "token"
}

func isGqlNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isGqlNameCont(c byte) bool {
	return isGqlNameStart(c) || (c >= '0' && c <= '9')
}

func lexGraphQL(src string) ([]gqlToken, *SpecError) {
	var toks []gqlToken
	line := 1
	i := 0
	n := len(src)
	if strings.HasPrefix(src, "\uFEFF") {
		i = 3
	}
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '\r':
			if i+1 < n && src[i+1] == '\n' {
				i++
			}
			line++
			i++
		case c == ' ' || c == '\t' || c == ',':
			i++
		case c == '#':
			for i < n && src[i] != '\n' && src[i] != '\r' {
				i++
			}
		case c == '"':
			tok, next, nextLine, err := lexGqlString(src, i, line)
			if err != nil {
				return nil, err
			}
			toks = append(toks, tok)
			i, line = next, nextLine
		case isGqlNameStart(c):
			j := i + 1
			for j < n && isGqlNameCont(src[j]) {
				j++
			}
			toks = append(toks, gqlToken{gqlTokName, src[i:j], line})
			i = j
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < n && (src[j] == '.' || src[j] == '+' || src[j] == '-' || src[j] == 'e' || src[j] == 'E' || (src[j] >= '0' && src[j] <= '9')) {
				j++
			}
			toks = append(toks, gqlToken{gqlTokNumber, src[i:j], line})
			i = j
		case strings.IndexByte("!$&():=@[]{|}", c) >= 0:
			toks = append(toks, gqlToken{gqlTokPunct, string(c), line})
			i++
		default:
			return nil, gqlErr(line, "unexpected character %q", string(c))
		}
		if len(toks) > graphqlMaxTokens {
			return nil, specErr(SpecTooLarge, fmt.Sprintf("GraphQL SDL exceeds the %d-token limit", graphqlMaxTokens))
		}
	}
	toks = append(toks, gqlToken{gqlTokEOF, "", line})
	return toks, nil
}

// lexGqlString lexes a `"..."` or `"""..."""` string starting at src[i] and
// returns the token plus the next index/line.
func lexGqlString(src string, i, line int) (gqlToken, int, int, *SpecError) {
	n := len(src)
	startLine := line
	if strings.HasPrefix(src[i:], `"""`) {
		j := i + 3
		for {
			if j >= n {
				return gqlToken{}, 0, 0, gqlErr(startLine, "unterminated block string")
			}
			if strings.HasPrefix(src[j:], `\"""`) {
				j += 4
				continue
			}
			if strings.HasPrefix(src[j:], `"""`) {
				break
			}
			if src[j] == '\n' {
				line++
			}
			j++
		}
		val := strings.TrimSpace(strings.ReplaceAll(src[i+3:j], `\"""`, `"""`))
		return gqlToken{gqlTokString, val, startLine}, j + 3, line, nil
	}
	var b strings.Builder
	j := i + 1
	for {
		if j >= n || src[j] == '\n' || src[j] == '\r' {
			return gqlToken{}, 0, 0, gqlErr(startLine, "unterminated string")
		}
		if src[j] == '"' {
			break
		}
		if src[j] == '\\' {
			if j+1 >= n {
				return gqlToken{}, 0, 0, gqlErr(startLine, "unterminated string")
			}
			switch esc := src[j+1]; esc {
			case '"', '\\', '/':
				b.WriteByte(esc)
				j += 2
			case 'n':
				b.WriteByte('\n')
				j += 2
			case 't':
				b.WriteByte('\t')
				j += 2
			case 'r', 'b', 'f':
				b.WriteByte(' ')
				j += 2
			case 'u':
				if j+6 > n {
					return gqlToken{}, 0, 0, gqlErr(startLine, "truncated \\u escape")
				}
				var r rune
				for _, h := range []byte(src[j+2 : j+6]) {
					switch {
					case h >= '0' && h <= '9':
						r = r*16 + rune(h-'0')
					case h >= 'a' && h <= 'f':
						r = r*16 + rune(h-'a'+10)
					case h >= 'A' && h <= 'F':
						r = r*16 + rune(h-'A'+10)
					default:
						return gqlToken{}, 0, 0, gqlErr(startLine, "invalid \\u escape")
					}
				}
				b.WriteRune(r)
				j += 6
			default:
				return gqlToken{}, 0, 0, gqlErr(startLine, "invalid escape \\%s", string(esc))
			}
			continue
		}
		b.WriteByte(src[j])
		j++
	}
	return gqlToken{gqlTokString, b.String(), startLine}, j + 1, line, nil
}

type gqlParser struct {
	toks   []gqlToken
	pos    int
	schema *gqlSchema
}

func parseGraphQLSDL(src string) (*gqlSchema, *SpecError) {
	toks, err := lexGraphQL(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{
		toks:   toks,
		schema: &gqlSchema{byName: map[string]*gqlType{}},
	}
	if err := p.parseDocument(); err != nil {
		return nil, err
	}
	return p.schema, nil
}

func (p *gqlParser) peek() *gqlToken { return &p.toks[p.pos] }

func (p *gqlParser) next() *gqlToken {
	t := &p.toks[p.pos]
	if t.kind != gqlTokEOF {
		p.pos++
	}
	return t
}

func (p *gqlParser) isPunct(s string) bool {
	t := p.peek()
	return t.kind == gqlTokPunct && t.val == s
}

func (p *gqlParser) acceptPunct(s string) bool {
	if p.isPunct(s) {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) expectPunct(s string) *SpecError {
	t := p.next()
	if t.kind != gqlTokPunct || t.val != s {
		return gqlErr(t.line, "expected %q, found %s %q", s, t.kind, t.val)
	}
	return nil
}

func (p *gqlParser) expectName() (string, int, *SpecError) {
	t := p.next()
	if t.kind != gqlTokName {
		return "", t.line, gqlErr(t.line, "expected a name, found %s %q", t.kind, t.val)
	}
	return t.val, t.line, nil
}

// acceptDescription consumes a leading string token (a description) if
// present.
func (p *gqlParser) acceptDescription() string {
	if p.peek().kind == gqlTokString {
		return p.next().val
	}
	return ""
}

func (p *gqlParser) parseDocument() *SpecError {
	for p.peek().kind != gqlTokEOF {
		desc := p.acceptDescription()
		if err := p.parseDefinition(desc, false); err != nil {
			return err
		}
	}
	if p.schema.rootFields(p.schema.queryType, "Query") == nil &&
		p.schema.rootFields(p.schema.mutationType, "Mutation") == nil {
		return specErr(SpecParseError, "GraphQL SDL: schema defines no Query or Mutation object type")
	}
	return nil
}

// parseDefinition parses one type-system definition; extend merges into an
// existing type instead of declaring a new one.
func (p *gqlParser) parseDefinition(desc string, extend bool) *SpecError {
	t := p.peek()
	if t.kind != gqlTokName {
		return gqlErr(t.line, "expected a type-system definition, found %s %q", t.kind, t.val)
	}
	switch t.val {
	case "schema":
		p.next()
		return p.parseSchemaDef()
	case "type":
		p.next()
		return p.parseCompositeDef(gqlObject, desc, extend)
	case "interface":
		p.next()
		return p.parseCompositeDef(gqlInterface, desc, extend)
	case "input":
		p.next()
		return p.parseCompositeDef(gqlInput, desc, extend)
	case "enum":
		p.next()
		return p.parseEnumDef(desc, extend)
	case "union":
		p.next()
		return p.parseUnionDef(desc, extend)
	case "scalar":
		p.next()
		return p.parseScalarDef(desc, extend)
	case "directive":
		p.next()
		return p.parseDirectiveDef()
	case "extend":
		if extend {
			return gqlErr(t.line, "unexpected nested extend")
		}
		p.next()
		return p.parseDefinition("", true)
	default:
		return gqlErr(t.line, "unknown definition keyword %q", t.val)
	}
}

// register adds a type or merges an extend into the existing one. An extend of
// an undefined type declares it: federated SDL ships extensions without bases.
func (p *gqlParser) register(t *gqlType, line int, extend bool) *SpecError {
	existing, exists := p.schema.byName[t.name]
	if !exists {
		if len(p.schema.types) >= graphqlMaxTypes {
			return specErr(SpecTooLarge, fmt.Sprintf("GraphQL schema exceeds the %d-type limit", graphqlMaxTypes))
		}
		p.schema.types = append(p.schema.types, t)
		p.schema.byName[t.name] = t
		return nil
	}
	if !extend {
		return gqlErr(line, "duplicate type definition %q", t.name)
	}
	if existing.kind != t.kind {
		return gqlErr(line, "extend of %q does not match its kind", t.name)
	}
	existing.fields = append(existing.fields, t.fields...)
	existing.enumValues = append(existing.enumValues, t.enumValues...)
	existing.members = append(existing.members, t.members...)
	return nil
}

func (p *gqlParser) parseSchemaDef() *SpecError {
	if err := p.skipDirectives(); err != nil {
		return err
	}
	if !p.isPunct("{") {
		return nil // `extend schema @dir` — directives only
	}
	if err := p.expectPunct("{"); err != nil {
		return err
	}
	for !p.isPunct("}") {
		op, line, err := p.expectName()
		if err != nil {
			return err
		}
		if e := p.expectPunct(":"); e != nil {
			return e
		}
		name, _, err := p.expectName()
		if err != nil {
			return err
		}
		switch op {
		case "query":
			p.schema.queryType = name
		case "mutation":
			p.schema.mutationType = name
		case "subscription":
			p.schema.subscriptionType = name
		default:
			return gqlErr(line, "unknown root operation %q", op)
		}
	}
	return p.expectPunct("}")
}

func (p *gqlParser) parseCompositeDef(kind gqlKind, desc string, extend bool) *SpecError {
	name, line, err := p.expectName()
	if err != nil {
		return err
	}
	if kind != gqlInput && p.peek().kind == gqlTokName && p.peek().val == "implements" {
		p.next()
		p.acceptPunct("&") // optional leading &
		for {
			if _, _, err := p.expectName(); err != nil {
				return err
			}
			if !p.acceptPunct("&") {
				break
			}
		}
	}
	if err := p.skipDirectives(); err != nil {
		return err
	}
	t := &gqlType{kind: kind, name: name, description: desc}
	if p.isPunct("{") {
		fields, err := p.parseFieldsBlock(kind)
		if err != nil {
			return err
		}
		t.fields = fields
	}
	return p.register(t, line, extend)
}

func (p *gqlParser) parseFieldsBlock(kind gqlKind) ([]gqlField, *SpecError) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var fields []gqlField
	for !p.isPunct("}") {
		fdesc := p.acceptDescription()
		fname, _, err := p.expectName()
		if err != nil {
			return nil, err
		}
		var args []gqlArg
		if kind != gqlInput && p.isPunct("(") {
			args, err = p.parseArgDefs()
			if err != nil {
				return nil, err
			}
		}
		if e := p.expectPunct(":"); e != nil {
			return nil, e
		}
		typ, err := p.parseTypeRef(0)
		if err != nil {
			return nil, err
		}
		if p.acceptPunct("=") {
			if err := p.skipValue(0); err != nil {
				return nil, err
			}
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		fields = append(fields, gqlField{name: fname, description: fdesc, args: args, typ: typ})
	}
	return fields, p.expectPunct("}")
}

func (p *gqlParser) parseArgDefs() ([]gqlArg, *SpecError) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var args []gqlArg
	for !p.isPunct(")") {
		p.acceptDescription()
		name, _, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if e := p.expectPunct(":"); e != nil {
			return nil, e
		}
		typ, err := p.parseTypeRef(0)
		if err != nil {
			return nil, err
		}
		if p.acceptPunct("=") {
			if err := p.skipValue(0); err != nil {
				return nil, err
			}
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		args = append(args, gqlArg{name: name, typ: typ})
	}
	return args, p.expectPunct(")")
}

func (p *gqlParser) parseTypeRef(depth int) (*gqlTypeRef, *SpecError) {
	if depth > graphqlMaxDepth {
		return nil, specErr(SpecDepthExceeded, fmt.Sprintf("GraphQL type nesting exceeds %d", graphqlMaxDepth))
	}
	var ref *gqlTypeRef
	if p.acceptPunct("[") {
		elem, err := p.parseTypeRef(depth + 1)
		if err != nil {
			return nil, err
		}
		if e := p.expectPunct("]"); e != nil {
			return nil, e
		}
		ref = &gqlTypeRef{elem: elem}
	} else {
		name, _, err := p.expectName()
		if err != nil {
			return nil, err
		}
		ref = &gqlTypeRef{name: name}
	}
	if p.acceptPunct("!") {
		ref.nonNull = true
	}
	return ref, nil
}

// skipValue consumes one GraphQL value literal (default values); the content
// is validated for shape and discarded.
func (p *gqlParser) skipValue(depth int) *SpecError {
	if depth > graphqlMaxDepth {
		return specErr(SpecDepthExceeded, fmt.Sprintf("GraphQL value nesting exceeds %d", graphqlMaxDepth))
	}
	t := p.next()
	switch t.kind {
	case gqlTokNumber, gqlTokString, gqlTokName: // numbers, strings, enum/bool/null names
		return nil
	case gqlTokPunct:
		switch t.val {
		case "$": // variables cannot appear in SDL defaults, but tolerate the shape
			_, _, err := p.expectName()
			return err
		case "[":
			for !p.isPunct("]") {
				if p.peek().kind == gqlTokEOF {
					return gqlErr(t.line, "unterminated list value")
				}
				if err := p.skipValue(depth + 1); err != nil {
					return err
				}
			}
			return p.expectPunct("]")
		case "{":
			for !p.isPunct("}") {
				if _, _, err := p.expectName(); err != nil {
					return err
				}
				if e := p.expectPunct(":"); e != nil {
					return e
				}
				if err := p.skipValue(depth + 1); err != nil {
					return err
				}
			}
			return p.expectPunct("}")
		}
	}
	return gqlErr(t.line, "expected a value, found %s %q", t.kind, t.val)
}

// skipDirectives consumes zero or more directive applications (@name(args)).
func (p *gqlParser) skipDirectives() *SpecError {
	for p.isPunct("@") {
		p.next()
		if _, _, err := p.expectName(); err != nil {
			return err
		}
		if p.isPunct("(") {
			p.next()
			for !p.isPunct(")") {
				if _, _, err := p.expectName(); err != nil {
					return err
				}
				if e := p.expectPunct(":"); e != nil {
					return e
				}
				if err := p.skipValue(0); err != nil {
					return err
				}
			}
			if e := p.expectPunct(")"); e != nil {
				return e
			}
		}
	}
	return nil
}

func (p *gqlParser) parseEnumDef(desc string, extend bool) *SpecError {
	name, line, err := p.expectName()
	if err != nil {
		return err
	}
	if e := p.skipDirectives(); e != nil {
		return e
	}
	t := &gqlType{kind: gqlEnum, name: name, description: desc}
	if p.isPunct("{") {
		p.next()
		for !p.isPunct("}") {
			p.acceptDescription()
			v, _, err := p.expectName()
			if err != nil {
				return err
			}
			if e := p.skipDirectives(); e != nil {
				return e
			}
			t.enumValues = append(t.enumValues, v)
		}
		if e := p.expectPunct("}"); e != nil {
			return e
		}
	}
	return p.register(t, line, extend)
}

func (p *gqlParser) parseUnionDef(desc string, extend bool) *SpecError {
	name, line, err := p.expectName()
	if err != nil {
		return err
	}
	if e := p.skipDirectives(); e != nil {
		return e
	}
	t := &gqlType{kind: gqlUnion, name: name, description: desc}
	if p.acceptPunct("=") {
		p.acceptPunct("|")
		for {
			m, _, err := p.expectName()
			if err != nil {
				return err
			}
			t.members = append(t.members, m)
			if !p.acceptPunct("|") {
				break
			}
		}
	}
	return p.register(t, line, extend)
}

func (p *gqlParser) parseScalarDef(desc string, extend bool) *SpecError {
	name, line, err := p.expectName()
	if err != nil {
		return err
	}
	if e := p.skipDirectives(); e != nil {
		return e
	}
	if _, builtin := gqlBuiltinScalars[name]; builtin {
		return nil // redeclaring a builtin scalar is a no-op
	}
	return p.register(&gqlType{kind: gqlScalar, name: name, description: desc}, line, extend)
}

// parseDirectiveDef consumes `directive @name(args) repeatable? on LOC|LOC`.
// Directive definitions are not modelled.
func (p *gqlParser) parseDirectiveDef() *SpecError {
	if err := p.expectPunct("@"); err != nil {
		return err
	}
	if _, _, err := p.expectName(); err != nil {
		return err
	}
	if p.isPunct("(") {
		if _, err := p.parseArgDefs(); err != nil {
			return err
		}
	}
	if p.peek().kind == gqlTokName && p.peek().val == "repeatable" {
		p.next()
	}
	kw, line, err := p.expectName()
	if err != nil {
		return err
	}
	if kw != "on" {
		return gqlErr(line, "expected 'on' in directive definition, found %q", kw)
	}
	p.acceptPunct("|")
	for {
		if _, _, err := p.expectName(); err != nil {
			return err
		}
		if !p.acceptPunct("|") {
			return nil
		}
	}
}

func schemaFromIntrospection(m *OrdMap) (*gqlSchema, *SpecError) {
	s := &gqlSchema{byName: map[string]*gqlType{}}
	if d, ok := m.GetOr("description").(string); ok {
		s.description = d
	}
	rootName := func(key string) string {
		if rm, ok := m.GetOr(key).(*OrdMap); ok {
			if n, ok := rm.GetOr("name").(string); ok {
				return n
			}
		}
		return ""
	}
	s.queryType = rootName("queryType")
	s.mutationType = rootName("mutationType")
	s.subscriptionType = rootName("subscriptionType")

	typesRaw, ok := m.GetOr("types").([]any)
	if !ok {
		return nil, specErr(SpecParseError, "GraphQL introspection __schema.types is missing")
	}
	for _, tRaw := range typesRaw {
		tm, ok := tRaw.(*OrdMap)
		if !ok {
			continue
		}
		name, _ := tm.GetOr("name").(string)
		if name == "" || strings.HasPrefix(name, "__") {
			continue
		}
		kind, _ := tm.GetOr("kind").(string)
		t := &gqlType{name: name}
		if d, ok := tm.GetOr("description").(string); ok {
			t.description = d
		}
		var err *SpecError
		switch kind {
		case "OBJECT", "INTERFACE":
			t.kind = gqlObject
			if kind == "INTERFACE" {
				t.kind = gqlInterface
			}
			t.fields, err = introspectionFields(tm.GetOr("fields"), true)
		case "INPUT_OBJECT":
			t.kind = gqlInput
			t.fields, err = introspectionFields(tm.GetOr("inputFields"), false)
		case "ENUM":
			t.kind = gqlEnum
			if values, ok := tm.GetOr("enumValues").([]any); ok {
				for _, v := range values {
					if vm, ok := v.(*OrdMap); ok {
						if vn, ok := vm.GetOr("name").(string); ok {
							t.enumValues = append(t.enumValues, vn)
						}
					}
				}
			}
		case "UNION":
			t.kind = gqlUnion
			if members, ok := tm.GetOr("possibleTypes").([]any); ok {
				for _, mem := range members {
					if mm, ok := mem.(*OrdMap); ok {
						if mn, ok := mm.GetOr("name").(string); ok {
							t.members = append(t.members, mn)
						}
					}
				}
			}
		case "SCALAR":
			if _, builtin := gqlBuiltinScalars[name]; builtin {
				continue
			}
			t.kind = gqlScalar
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(s.types) >= graphqlMaxTypes {
			return nil, specErr(SpecTooLarge, fmt.Sprintf("GraphQL schema exceeds the %d-type limit", graphqlMaxTypes))
		}
		if _, dup := s.byName[name]; dup {
			return nil, specErr(SpecParseError, fmt.Sprintf("GraphQL introspection declares type %q twice", name))
		}
		s.types = append(s.types, t)
		s.byName[name] = t
	}
	if s.rootFields(s.queryType, "Query") == nil && s.rootFields(s.mutationType, "Mutation") == nil {
		return nil, specErr(SpecParseError, "GraphQL introspection defines no Query or Mutation object type")
	}
	return s, nil
}

// introspectionFields maps __Field[] (withArgs) or __InputValue[].
func introspectionFields(raw any, withArgs bool) ([]gqlField, *SpecError) {
	list, ok := raw.([]any)
	if !ok {
		return nil, nil
	}
	var out []gqlField
	for _, fRaw := range list {
		fm, ok := fRaw.(*OrdMap)
		if !ok {
			continue
		}
		name, _ := fm.GetOr("name").(string)
		if name == "" {
			continue
		}
		f := gqlField{name: name}
		if d, ok := fm.GetOr("description").(string); ok {
			f.description = d
		}
		typ, err := introspectionTypeRef(fm.GetOr("type"), 0)
		if err != nil {
			return nil, err
		}
		f.typ = typ
		if withArgs {
			if args, ok := fm.GetOr("args").([]any); ok {
				for _, aRaw := range args {
					am, ok := aRaw.(*OrdMap)
					if !ok {
						continue
					}
					an, _ := am.GetOr("name").(string)
					if an == "" {
						continue
					}
					at, err := introspectionTypeRef(am.GetOr("type"), 0)
					if err != nil {
						return nil, err
					}
					f.args = append(f.args, gqlArg{name: an, typ: at})
				}
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func introspectionTypeRef(raw any, depth int) (*gqlTypeRef, *SpecError) {
	if depth > graphqlMaxDepth {
		return nil, specErr(SpecDepthExceeded, fmt.Sprintf("GraphQL type nesting exceeds %d", graphqlMaxDepth))
	}
	tm, ok := raw.(*OrdMap)
	if !ok {
		return nil, specErr(SpecParseError, "GraphQL introspection has a malformed type reference")
	}
	kind, _ := tm.GetOr("kind").(string)
	switch kind {
	case "NON_NULL":
		inner, err := introspectionTypeRef(tm.GetOr("ofType"), depth+1)
		if err != nil {
			return nil, err
		}
		inner.nonNull = true
		return inner, nil
	case "LIST":
		elem, err := introspectionTypeRef(tm.GetOr("ofType"), depth+1)
		if err != nil {
			return nil, err
		}
		return &gqlTypeRef{elem: elem}, nil
	default:
		name, _ := tm.GetOr("name").(string)
		if name == "" {
			return nil, specErr(SpecParseError, "GraphQL introspection has a type reference without a name")
		}
		return &gqlTypeRef{name: name}, nil
	}
}

func graphqlToOpenAPI(s *gqlSchema) (*OrdMap, *SpecError) {
	oas := NewOrdMap()
	oas.Set("openapi", "3.0.0")

	info := NewOrdMap()
	info.Set("title", "GraphQL API")
	info.Set("version", "graphql")
	if s.description != "" {
		info.Set("description", s.description)
	}
	oas.Set("info", info)

	paths := NewOrdMap()
	paths.Set("/graphql", gqlCatchAllPathItem())
	for _, f := range s.rootFields(s.queryType, "Query") {
		item := NewOrdMap()
		item.Set("get", gqlQueryOperation(s, &f))
		paths.Set("/graphql/query/"+f.name, item)
	}
	for _, f := range s.rootFields(s.mutationType, "Mutation") {
		item := NewOrdMap()
		item.Set("post", gqlMutationOperation(s, &f))
		paths.Set("/graphql/mutation/"+f.name, item)
	}
	oas.Set("paths", paths)

	if subs := s.rootFields(s.subscriptionType, "Subscription"); len(subs) > 0 {
		hooks := NewOrdMap()
		for _, f := range subs {
			hooks.Set(f.name, gqlSubscriptionWebhook(s, &f))
		}
		oas.Set("webhooks", hooks)
	}

	schemas := NewOrdMap()
	for _, t := range s.types {
		schemas.Set(t.name, gqlComponentSchema(s, t))
	}
	if schemas.Len() > 0 {
		components := NewOrdMap()
		components.Set("schemas", schemas)
		oas.Set("components", components)
	}
	// No security schemes on purpose: GraphQL SDL carries no auth information
	// and pikopod never invents an auth scheme it cannot justify.
	return oas, nil
}

func gqlJSONResponse(schema *OrdMap) *OrdMap {
	media := NewOrdMap()
	media.Set("schema", schema)
	content := NewOrdMap()
	content.Set("application/json", media)
	resp := NewOrdMap()
	resp.Set("description", "OK")
	resp.Set("content", content)
	responses := NewOrdMap()
	responses.Set("200", resp)
	return responses
}

func gqlCatchAllPathItem() *OrdMap {
	op := NewOrdMap()
	op.Set("operationId", "graphql.execute")
	op.Set("summary", "GraphQL endpoint (catch-all)")
	op.Set("description", "Accepts a raw GraphQL request ({query, variables}). "+
		"The sandbox models each top-level Query field as GET /graphql/query/{field} "+
		"and each Mutation field as POST /graphql/mutation/{field}; those per-field "+
		"operations are the modelled surface. This catch-all returns a generically "+
		"shaped {data} response so real GraphQL clients get an answer, not a 404 — "+
		"it does not execute GraphQL documents.")
	op.Set("tags", []any{"graphql"})

	query := NewOrdMap()
	query.Set("type", "string")
	query.Set("description", "GraphQL query document")
	variables := NewOrdMap()
	variables.Set("type", "object")
	props := NewOrdMap()
	props.Set("query", query)
	props.Set("variables", variables)
	schema := NewOrdMap()
	schema.Set("type", "object")
	schema.Set("properties", props)
	schema.Set("required", []any{"query"})
	media := NewOrdMap()
	media.Set("schema", schema)
	content := NewOrdMap()
	content.Set("application/json", media)
	body := NewOrdMap()
	body.Set("required", true)
	body.Set("content", content)
	op.Set("requestBody", body)

	data := NewOrdMap()
	data.Set("type", "object")
	respProps := NewOrdMap()
	respProps.Set("data", data)
	respSchema := NewOrdMap()
	respSchema.Set("type", "object")
	respSchema.Set("properties", respProps)
	op.Set("responses", gqlJSONResponse(respSchema))

	item := NewOrdMap()
	item.Set("post", op)
	return item
}

func gqlQueryOperation(s *gqlSchema, f *gqlField) *OrdMap {
	op := NewOrdMap()
	op.Set("operationId", "query."+f.name)
	if f.description != "" {
		op.Set("summary", f.description)
	} else {
		op.Set("summary", f.name)
	}
	op.Set("tags", []any{"query"})

	var params []any
	var skipped []string
	for _, arg := range f.args {
		schema, ok := gqlParamSchema(s, arg.typ)
		if !ok {
			skipped = append(skipped, arg.name+": "+gqlTypeRefString(arg.typ))
			continue
		}
		pm := NewOrdMap()
		pm.Set("name", arg.name)
		pm.Set("in", "query")
		if arg.typ.nonNull {
			pm.Set("required", true)
		}
		pm.Set("schema", schema)
		params = append(params, pm)
	}
	if len(params) > 0 {
		op.Set("parameters", params)
	}

	desc := f.description
	if len(skipped) > 0 {
		note := "GraphQL arguments not modelled as query parameters (non-scalar): " + strings.Join(skipped, ", ") + "."
		if desc != "" {
			desc += "\n\n" + note
		} else {
			desc = note
		}
	}
	if desc != "" {
		op.Set("description", desc)
	}

	op.Set("responses", gqlJSONResponse(gqlTypeRefSchema(s, f.typ)))
	return op
}

func gqlMutationOperation(s *gqlSchema, f *gqlField) *OrdMap {
	op := NewOrdMap()
	op.Set("operationId", "mutation."+f.name)
	if f.description != "" {
		op.Set("summary", f.description)
		op.Set("description", f.description)
	} else {
		op.Set("summary", f.name)
	}
	op.Set("tags", []any{"mutation"})

	if len(f.args) > 0 {
		var schema *OrdMap
		bodyRequired := false
		if input, ok := gqlSingleInputObjectArg(s, f.args); ok {
			// `createUser(input: CreateUserInput!)` → the body IS the input
			// type; its required list comes from the input's non-null fields.
			schema = gqlRefSchema(input)
			bodyRequired = f.args[0].typ.nonNull
		} else {
			props := NewOrdMap()
			var required []any
			for _, arg := range f.args {
				props.Set(arg.name, gqlTypeRefSchema(s, arg.typ))
				if arg.typ.nonNull {
					required = append(required, arg.name)
				}
			}
			schema = NewOrdMap()
			schema.Set("type", "object")
			schema.Set("properties", props)
			if len(required) > 0 {
				schema.Set("required", required)
				bodyRequired = true
			}
		}
		media := NewOrdMap()
		media.Set("schema", schema)
		content := NewOrdMap()
		content.Set("application/json", media)
		body := NewOrdMap()
		if bodyRequired {
			body.Set("required", true)
		}
		body.Set("content", content)
		op.Set("requestBody", body)
	}

	op.Set("responses", gqlJSONResponse(gqlTypeRefSchema(s, f.typ)))
	return op
}

func gqlSubscriptionWebhook(s *gqlSchema, f *gqlField) *OrdMap {
	media := NewOrdMap()
	media.Set("schema", gqlTypeRefSchema(s, f.typ))
	content := NewOrdMap()
	content.Set("application/json", media)
	body := NewOrdMap()
	body.Set("content", content)
	op := NewOrdMap()
	if f.description != "" {
		op.Set("description", f.description)
	}
	op.Set("requestBody", body)
	resp := NewOrdMap()
	resp.Set("description", "OK")
	responses := NewOrdMap()
	responses.Set("200", resp)
	op.Set("responses", responses)
	item := NewOrdMap()
	item.Set("post", op)
	return item
}

// gqlSingleInputObjectArg reports the input-object type name when the field
// has exactly one argument and it is an input object.
func gqlSingleInputObjectArg(s *gqlSchema, args []gqlArg) (string, bool) {
	if len(args) != 1 || args[0].typ.elem != nil {
		return "", false
	}
	t, ok := s.byName[args[0].typ.name]
	if !ok || t.kind != gqlInput {
		return "", false
	}
	return t.name, true
}

func gqlRefSchema(name string) *OrdMap {
	ref := NewOrdMap()
	ref.Set("$ref", "#/components/schemas/"+name)
	return ref
}

// gqlTypeRefSchema maps a GraphQL type reference to an OAS schema. Named types
// always become $refs, so recursive types terminate by construction.
func gqlTypeRefSchema(s *gqlSchema, ref *gqlTypeRef) *OrdMap {
	if ref == nil {
		obj := NewOrdMap()
		obj.Set("type", "object")
		return obj
	}
	if ref.elem != nil {
		arr := NewOrdMap()
		arr.Set("type", "array")
		arr.Set("items", gqlTypeRefSchema(s, ref.elem))
		return arr
	}
	if builtin, ok := gqlBuiltinScalars[ref.name]; ok {
		sch := NewOrdMap()
		sch.Set("type", builtin)
		return sch
	}
	if _, ok := s.byName[ref.name]; ok {
		return gqlRefSchema(ref.name)
	}
	// Reference to an undefined type: tolerate as an opaque string carrying
	// the name as its format (same shape custom scalars get).
	sch := NewOrdMap()
	sch.Set("type", "string")
	sch.Set("format", ref.name)
	return sch
}

// gqlParamSchema returns a query-parameter schema when the arg type is
// parameter-able: a scalar, enum, or custom scalar — not a list or object-ish.
func gqlParamSchema(s *gqlSchema, ref *gqlTypeRef) (*OrdMap, bool) {
	if ref == nil || ref.elem != nil {
		return nil, false
	}
	if builtin, ok := gqlBuiltinScalars[ref.name]; ok {
		sch := NewOrdMap()
		sch.Set("type", builtin)
		return sch, true
	}
	if t, ok := s.byName[ref.name]; ok {
		if t.kind == gqlEnum || t.kind == gqlScalar {
			return gqlRefSchema(ref.name), true
		}
		return nil, false
	}
	// Undefined type: opaque string, same tolerance as gqlTypeRefSchema.
	sch := NewOrdMap()
	sch.Set("type", "string")
	sch.Set("format", ref.name)
	return sch, true
}

// gqlTypeRefString reconstructs the SDL spelling of a type reference for
// human-readable notes ("[User!]!").
func gqlTypeRefString(ref *gqlTypeRef) string {
	if ref == nil {
		return ""
	}
	var out string
	if ref.elem != nil {
		out = "[" + gqlTypeRefString(ref.elem) + "]"
	} else {
		out = ref.name
	}
	if ref.nonNull {
		out += "!"
	}
	return out
}

// gqlComponentSchema builds the components/schemas entry for a named type.
// Only INPUT types get a required list; non-null is ignored for responses.
func gqlComponentSchema(s *gqlSchema, t *gqlType) *OrdMap {
	sch := NewOrdMap()
	switch t.kind {
	case gqlObject, gqlInterface, gqlInput:
		sch.Set("type", "object")
		if t.description != "" {
			sch.Set("description", t.description)
		}
		props := NewOrdMap()
		var required []any
		for _, f := range t.fields {
			props.Set(f.name, gqlTypeRefSchema(s, f.typ))
			if t.kind == gqlInput && f.typ.nonNull {
				required = append(required, f.name)
			}
		}
		if props.Len() > 0 {
			sch.Set("properties", props)
		}
		if len(required) > 0 {
			sch.Set("required", required)
		}
	case gqlEnum:
		sch.Set("type", "string")
		if t.description != "" {
			sch.Set("description", t.description)
		}
		if len(t.enumValues) > 0 {
			values := make([]any, 0, len(t.enumValues))
			for _, v := range t.enumValues {
				values = append(values, v)
			}
			sch.Set("enum", values)
		}
	case gqlUnion:
		if t.description != "" {
			sch.Set("description", t.description)
		}
		var members []any
		for _, m := range t.members {
			if _, ok := s.byName[m]; ok {
				members = append(members, gqlRefSchema(m))
			}
		}
		if len(members) > 0 {
			sch.Set("oneOf", members)
		} else {
			sch.Set("type", "object")
		}
	case gqlScalar:
		sch.Set("type", "string")
		sch.Set("format", t.name)
		if t.description != "" {
			sch.Set("description", t.description)
		}
	}
	return sch
}
