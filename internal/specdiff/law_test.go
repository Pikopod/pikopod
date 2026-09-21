package specdiff

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

var deviations = map[string]string{}

type pair struct {
	old, nu *ir.ApiDefinition
}

func withReq(schema ir.IrSchemaNode, required bool) func(*ir.Endpoint) {
	return func(e *ir.Endpoint) {
		e.RequestBody = &ir.RequestBody{Required: ir.Explicit(required, ""), Content: []ir.MediaType{{MediaType: "application/json", Schema: schema}}}
	}
}

func withResp(status string, schema ir.IrSchemaNode) func(*ir.Endpoint) {
	return func(e *ir.Endpoint) {
		e.Responses = append(e.Responses, ir.ResponseDef{StatusCode: status, Content: []ir.MediaType{{MediaType: "application/json", Schema: schema}}})
	}
}

func withParam(name, in string, required bool) func(*ir.Endpoint) {
	return func(e *ir.Endpoint) {
		e.Parameters = append(e.Parameters, ir.Parameter{Name: name, Location: in, Required: ir.Explicit(required, ""), Schema: strSchema()})
	}
}

func withSecurity(ids ...string) func(*ir.Endpoint) {
	return func(e *ir.Endpoint) {
		for _, id := range ids {
			e.Security = append(e.Security, ir.SecurityRequirement{SchemeID: id})
		}
	}
}

func typed(t string) ir.IrSchemaNode {
	return ir.IrSchemaNode{Type: ir.Explicit(t, ""), Nullable: ir.Explicit(false, "")}
}

func nullable(s ir.IrSchemaNode) ir.IrSchemaNode {
	s.Nullable = ir.Explicit(true, "")
	return s
}

func enum(vals ...any) ir.IrSchemaNode {
	s := strSchema()
	s.EnumValues = &ir.Prov[[]any]{Value: vals, Provenance: ir.ProvenanceExplicit, Confidence: 1}
	return s
}

func auth(name, kind, scheme string) ir.ApiDefinition {
	sc := scheme
	return ir.ApiDefinition{AuthSchemes: []ir.AuthScheme{{ID: "au_" + name, Name: name, Kind: ir.Explicit(kind, ""), Scheme: &ir.Prov[string]{Value: sc}}}}
}

func reqSide(body ir.IrSchemaNode) *ir.ApiDefinition {
	return def(ep("POST", "/w", withReq(body, true)))
}

func respSide(body ir.IrSchemaNode) *ir.ApiDefinition {
	return def(ep("GET", "/w", withResp("200", body)))
}

func fixtures() map[string]pair {
	return map[string]pair{
		"endpoint-removed":                {def(ep("GET", "/w"), ep("GET", "/g")), def(ep("GET", "/w"))},
		"endpoint-added":                  {def(ep("GET", "/w")), def(ep("GET", "/w"), ep("GET", "/g"))},
		"endpoint-deprecated":             {def(ep("GET", "/w")), def(ep("GET", "/w", func(e *ir.Endpoint) { e.Deprecated = ir.Explicit(true, "") }))},
		"param-removed":                   {def(ep("GET", "/w", withParam("q", "query", false))), def(ep("GET", "/w"))},
		"param-renamed":                   {def(ep("GET", "/w/{id}", withParam("id", "path", true))), def(ep("GET", "/w/{wid}", withParam("wid", "path", true)))},
		"param-became-required":           {def(ep("GET", "/w", withParam("q", "query", false))), def(ep("GET", "/w", withParam("q", "query", true)))},
		"param-became-optional":           {def(ep("GET", "/w", withParam("q", "query", true))), def(ep("GET", "/w", withParam("q", "query", false)))},
		"param-added-required":            {def(ep("GET", "/w")), def(ep("GET", "/w", withParam("q", "query", true)))},
		"param-added-optional":            {def(ep("GET", "/w")), def(ep("GET", "/w", withParam("q", "query", false)))},
		"request-body-added-required":     {def(ep("POST", "/w")), def(ep("POST", "/w", withReq(objSchema(), true)))},
		"request-body-added-optional":     {def(ep("POST", "/w")), def(ep("POST", "/w", withReq(objSchema(), false)))},
		"request-body-removed/required":   {def(ep("POST", "/w", withReq(objSchema(), true))), def(ep("POST", "/w"))},
		"request-body-removed/optional":   {def(ep("POST", "/w", withReq(objSchema(), false))), def(ep("POST", "/w"))},
		"request-body-became-required":    {def(ep("POST", "/w", withReq(objSchema(), false))), def(ep("POST", "/w", withReq(objSchema(), true)))},
		"request-body-became-optional":    {def(ep("POST", "/w", withReq(objSchema(), true))), def(ep("POST", "/w", withReq(objSchema(), false)))},
		"response-status-removed/success": {def(ep("GET", "/w", withResp("200", objSchema()), withResp("404", objSchema()))), def(ep("GET", "/w", withResp("404", objSchema())))},
		"response-status-removed/error":   {def(ep("GET", "/w", withResp("200", objSchema()), withResp("404", objSchema()))), def(ep("GET", "/w", withResp("200", objSchema())))},
		"response-status-added":           {def(ep("GET", "/w", withResp("200", objSchema()))), def(ep("GET", "/w", withResp("200", objSchema()), withResp("404", objSchema())))},
		"request-media-type-removed": {def(ep("POST", "/w", func(e *ir.Endpoint) {
			e.RequestBody = &ir.RequestBody{Required: ir.Explicit(true, ""), Content: []ir.MediaType{{MediaType: "application/json", Schema: objSchema()}, {MediaType: "text/xml", Schema: objSchema()}}}
		})), reqSide(objSchema())},
		"response-media-type-removed": {def(ep("GET", "/w", func(e *ir.Endpoint) {
			e.Responses = []ir.ResponseDef{{StatusCode: "200", Content: []ir.MediaType{{MediaType: "application/json", Schema: objSchema()}, {MediaType: "text/xml", Schema: objSchema()}}}}
		})), respSide(objSchema())},
		"request-media-type-added": {reqSide(objSchema()), def(ep("POST", "/w", func(e *ir.Endpoint) {
			e.RequestBody = &ir.RequestBody{Required: ir.Explicit(true, ""), Content: []ir.MediaType{{MediaType: "application/json", Schema: objSchema()}, {MediaType: "text/xml", Schema: objSchema()}}}
		}))},
		"response-media-type-added": {respSide(objSchema()), def(ep("GET", "/w", func(e *ir.Endpoint) {
			e.Responses = []ir.ResponseDef{{StatusCode: "200", Content: []ir.MediaType{{MediaType: "application/json", Schema: objSchema()}, {MediaType: "text/xml", Schema: objSchema()}}}}
		}))},
		"endpoint-security-added":            {def(ep("GET", "/w")), def(ep("GET", "/w", withSecurity("au_key")))},
		"endpoint-security-scheme-removed":   {def(ep("GET", "/w", withSecurity("au_key", "au_bearer"))), def(ep("GET", "/w", withSecurity("au_key")))},
		"endpoint-security-removed":          {def(ep("GET", "/w", withSecurity("au_key"))), def(ep("GET", "/w"))},
		"auth-scheme-removed":                {ptr(auth("k", "apiKey", "")), &ir.ApiDefinition{}},
		"auth-scheme-changed":                {ptr(auth("k", "http", "bearer")), ptr(auth("k", "http", "basic"))},
		"auth-scheme-added":                  {&ir.ApiDefinition{}, ptr(auth("k", "apiKey", ""))},
		"schema-restructured":                {respSide(objSchema()), respSide(oneOf(objSchema(), strSchema()))},
		"request-variant-removed":            {reqSide(oneOf(objSchema(), strSchema())), reqSide(oneOf(objSchema()))},
		"response-variant-removed":           {respSide(oneOf(objSchema(), strSchema())), respSide(oneOf(objSchema()))},
		"request-variant-added":              {reqSide(oneOf(objSchema())), reqSide(oneOf(objSchema(), strSchema()))},
		"response-variant-added":             {respSide(oneOf(objSchema())), respSide(oneOf(objSchema(), strSchema()))},
		"request-type-changed/incomparable":  {reqSide(objSchema(prop("a", true, typed("string")))), reqSide(objSchema(prop("a", true, typed("boolean"))))},
		"request-type-changed/narrows":       {reqSide(objSchema(prop("a", true, typed("number")))), reqSide(objSchema(prop("a", true, typed("integer"))))},
		"request-type-changed/widens":        {reqSide(objSchema(prop("a", true, typed("integer")))), reqSide(objSchema(prop("a", true, typed("number"))))},
		"response-type-changed/incomparable": {respSide(objSchema(prop("a", true, typed("string")))), respSide(objSchema(prop("a", true, typed("boolean"))))},
		"response-type-changed/narrows":      {respSide(objSchema(prop("a", true, typed("number")))), respSide(objSchema(prop("a", true, typed("integer"))))},
		"response-type-changed/widens":       {respSide(objSchema(prop("a", true, typed("integer")))), respSide(objSchema(prop("a", true, typed("number"))))},
		"request-nullable-added":             {reqSide(objSchema(prop("a", true, strSchema()))), reqSide(objSchema(prop("a", true, nullable(strSchema()))))},
		"response-nullable-added":            {respSide(objSchema(prop("a", true, strSchema()))), respSide(objSchema(prop("a", true, nullable(strSchema()))))},
		"request-nullable-removed":           {reqSide(objSchema(prop("a", true, nullable(strSchema())))), reqSide(objSchema(prop("a", true, strSchema())))},
		"response-nullable-removed":          {respSide(objSchema(prop("a", true, nullable(strSchema())))), respSide(objSchema(prop("a", true, strSchema())))},
		"request-enum-closed":                {reqSide(objSchema(prop("a", true, strSchema()))), reqSide(objSchema(prop("a", true, enum("x"))))},
		"response-enum-closed":               {respSide(objSchema(prop("a", true, strSchema()))), respSide(objSchema(prop("a", true, enum("x"))))},
		"request-enum-opened":                {reqSide(objSchema(prop("a", true, enum("x")))), reqSide(objSchema(prop("a", true, strSchema())))},
		"response-enum-opened":               {respSide(objSchema(prop("a", true, enum("x")))), respSide(objSchema(prop("a", true, strSchema())))},
		"request-enum-value-removed":         {reqSide(objSchema(prop("a", true, enum("x", "y")))), reqSide(objSchema(prop("a", true, enum("x"))))},
		"response-enum-value-removed":        {respSide(objSchema(prop("a", true, enum("x", "y")))), respSide(objSchema(prop("a", true, enum("x"))))},
		"request-enum-value-added":           {reqSide(objSchema(prop("a", true, enum("x")))), reqSide(objSchema(prop("a", true, enum("x", "y"))))},
		"response-enum-value-added":          {respSide(objSchema(prop("a", true, enum("x")))), respSide(objSchema(prop("a", true, enum("x", "y"))))},
		"request-property-removed":           {reqSide(objSchema(prop("a", false, strSchema()))), reqSide(objSchema())},
		"response-required-property-removed": {respSide(objSchema(prop("a", true, strSchema()))), respSide(objSchema())},
		"response-property-removed":          {respSide(objSchema(prop("a", false, strSchema()))), respSide(objSchema())},
		"request-property-became-required":   {reqSide(objSchema(prop("a", false, strSchema()))), reqSide(objSchema(prop("a", true, strSchema())))},
		"response-property-became-required":  {respSide(objSchema(prop("a", false, strSchema()))), respSide(objSchema(prop("a", true, strSchema())))},
		"request-property-became-optional":   {reqSide(objSchema(prop("a", true, strSchema()))), reqSide(objSchema(prop("a", false, strSchema())))},
		"response-property-became-optional":  {respSide(objSchema(prop("a", true, strSchema()))), respSide(objSchema(prop("a", false, strSchema())))},
		"request-property-added-required":    {reqSide(objSchema()), reqSide(objSchema(prop("a", true, strSchema())))},
		"request-property-added-optional":    {reqSide(objSchema()), reqSide(objSchema(prop("a", false, strSchema())))},
		"response-property-added":            {respSide(objSchema()), respSide(objSchema(prop("a", false, strSchema())))},
	}
}

func ptr(d ir.ApiDefinition) *ir.ApiDefinition { return &d }

func key(c Check) string {
	if c.Variant == "" {
		return c.ID
	}
	return c.ID + "/" + c.Variant
}

func only(t *testing.T, fs []Finding, id string) Finding {
	t.Helper()
	var hits []Finding
	for _, f := range fs {
		if f.ID == id {
			hits = append(hits, f)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("fixture must trigger exactly one %s, got %v", id, ids(fs))
	}
	return hits[0]
}

func TestSeverityLaw(t *testing.T) {
	fx := fixtures()
	for _, c := range Checks {
		k := key(c)
		if why, ok := deviations[c.ID]; ok {
			if !checkIndex[c.ID] {
				t.Fatalf("deviation %s is not in the inventory", c.ID)
			}
			t.Logf("deviation %s: %s", c.ID, why)
			continue
		}
		p, ok := fx[k]
		if !ok {
			t.Errorf("%s: no fixture", k)
			continue
		}
		f := only(t, Diff(p.old, p.nu), c.ID)
		want := DeriveLevel(c.Effect, c.Direction, c.Scope, Guards{Tolerated: c.Tolerated})
		if f.Level != want {
			t.Errorf("%s: emitted %s, law says %s for %s × %s × %s (tolerated=%v)", k, f.Level, want, c.Effect, c.Direction, c.Scope, c.Tolerated)
		}
	}
	for k := range fx {
		found := false
		for _, c := range Checks {
			if key(c) == k {
				found = true
			}
		}
		if !found {
			t.Errorf("fixture %s has no inventory entry", k)
		}
	}
}

func TestDeviationsEmpty(t *testing.T) {
	if len(deviations) != 0 {
		t.Fatalf("deviations must ship empty: %v", deviations)
	}
}

func TestContradictionFixed(t *testing.T) {
	fx := fixtures()
	a := only(t, Diff(fx["response-required-property-removed"].old, fx["response-required-property-removed"].nu), "response-required-property-removed")
	b := only(t, Diff(fx["response-property-became-optional"].old, fx["response-property-became-optional"].nu), "response-property-became-optional")
	if a.Level != b.Level {
		t.Fatalf("same withdrawn guarantee, different levels: %s=%s %s=%s", a.ID, a.Level, b.ID, b.Level)
	}
}

func maxLevel(fs []Finding) int {
	m := -1
	for _, f := range fs {
		if r := f.Level.Rank(); r > m {
			m = r
		}
	}
	return m
}

func TestMonotonicity(t *testing.T) {
	a := def(ep("GET", "/w", withResp("200", objSchema(prop("id", true, strSchema()), prop("status", true, enum("ok"))))), ep("GET", "/g"))
	b := def(ep("GET", "/w", withResp("200", objSchema(prop("id", true, strSchema()), prop("status", true, enum("ok"))))))
	c := def(ep("GET", "/w", withParam("tenant", "query", true), withResp("200", objSchema(prop("id", true, strSchema()), prop("status", true, enum("ok"))))))
	if maxLevel(Diff(a, b)) > maxLevel(Diff(a, c)) {
		t.Fatalf("more changes lowered the verdict: %v vs %v", ids(Diff(a, b)), ids(Diff(a, c)))
	}
	d := def(ep("GET", "/w", withParam("tenant", "query", true), withResp("200", objSchema(prop("id", true, strSchema()), prop("status", true, enum("ok")), prop("extra", false, strSchema())))))
	if maxLevel(Diff(a, d)) < maxLevel(Diff(a, c)) {
		t.Fatalf("an added INFO change lowered an existing ERR: %v", ids(Diff(a, d)))
	}
	for _, p := range fixtures() {
		base := maxLevel(Diff(p.old, p.nu))
		more := *p.nu
		more.Endpoints = append(append([]ir.Endpoint{}, more.Endpoints...), ep("GET", "/unrelated-added"))
		if maxLevel(Diff(p.old, &more)) < base {
			t.Fatalf("adding an unrelated endpoint lowered the verdict for %v", ids(Diff(p.old, p.nu)))
		}
	}
}

var inverses = map[string]string{
	"endpoint-removed":                   "endpoint-added",
	"param-became-required":              "param-became-optional",
	"param-added-optional":               "param-removed",
	"request-body-added-optional":        "request-body-removed/optional",
	"request-body-became-required":       "request-body-became-optional",
	"response-status-added":              "response-status-removed/error",
	"request-media-type-added":           "request-media-type-removed",
	"response-media-type-added":          "response-media-type-removed",
	"endpoint-security-added":            "endpoint-security-removed",
	"auth-scheme-added":                  "auth-scheme-removed",
	"auth-scheme-changed":                "auth-scheme-changed",
	"schema-restructured":                "schema-restructured",
	"request-variant-added":              "request-variant-removed",
	"response-variant-added":             "response-variant-removed",
	"request-type-changed/widens":        "request-type-changed/narrows",
	"response-type-changed/widens":       "response-type-changed/narrows",
	"request-type-changed/incomparable":  "request-type-changed/incomparable",
	"response-type-changed/incomparable": "response-type-changed/incomparable",
	"request-nullable-added":             "request-nullable-removed",
	"response-nullable-added":            "response-nullable-removed",
	"request-enum-closed":                "request-enum-opened",
	"response-enum-closed":               "response-enum-opened",
	"request-enum-value-added":           "request-enum-value-removed",
	"response-enum-value-added":          "response-enum-value-removed",
	"request-property-added-optional":    "request-property-removed",
	"request-property-became-required":   "request-property-became-optional",
	"response-property-became-required":  "response-property-became-optional",
	"response-property-added":            "response-property-removed",
	"param-renamed":                      "param-renamed",
}

var asymmetries = map[string]string{
	"param-added-required":               "reversing removes a required param, emitted as param-removed: by convention the server ignores what clients keep sending, a narrowing on their side rather than a widening of what is accepted",
	"request-property-added-required":    "same convention as param-added-required, for body properties",
	"request-body-added-required":        "reversing is request-body-removed/required, a guaranteed narrowing both ways: a body that appears is demanded, a body that vanishes is refused",
	"request-body-removed/required":      "see request-body-added-required",
	"response-status-removed/success":    "reversing adds a success status, emitted as response-status-added; the removal is guaranteed, the addition is a widening of documented outputs",
	"response-required-property-removed": "reversing adds a required property, emitted as response-property-added with tolerance; both widen the response set, the removal withdraws a guarantee the addition does not create for old consumers",
	"endpoint-deprecated":                "an announcement has no inverse check; undeprecating is not a contract change",
	"endpoint-security-scheme-removed":   "adding a second accepted scheme to an endpoint already requiring one is not reported today",
}

func flip(e Effect) Effect {
	switch e {
	case Narrows:
		return Widens
	case Widens:
		return Narrows
	}
	return e
}

func TestSymmetry(t *testing.T) {
	fx := fixtures()
	byKey := map[string]Check{}
	for _, c := range Checks {
		byKey[key(c)] = c
	}
	both := map[string]string{}
	for a, b := range inverses {
		both[a], both[b] = b, a
	}
	var gaps []string
	for _, c := range Checks {
		k := key(c)
		if _, documented := asymmetries[k]; documented {
			continue
		}
		invKey, ok := both[k]
		if !ok {
			gaps = append(gaps, k)
			continue
		}
		inv, ok := byKey[invKey]
		if !ok {
			t.Fatalf("%s names inverse %s, which is not in the inventory", k, invKey)
		}
		p := fx[k]
		rev := Diff(p.nu, p.old)
		found := false
		for _, f := range rev {
			if f.ID == inv.ID && f.Level == DeriveLevel(inv.Effect, inv.Direction, inv.Scope, Guards{Tolerated: inv.Tolerated}) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s reversed must emit %s at the law's level, got %v", k, invKey, ids(rev))
			continue
		}
		if flip(c.Effect) != inv.Effect || c.Direction != inv.Direction {
			t.Errorf("%s (%s × %s) reversed is %s (%s × %s); effects must flip on the same side", k, c.Effect, c.Direction, invKey, inv.Effect, inv.Direction)
		}
	}
	sort.Strings(gaps)
	if len(gaps) > 0 {
		t.Fatalf("checks with no inverse in the inventory and no documented asymmetry:\n  %s", strings.Join(gaps, "\n  "))
	}
}

var checkIDRe = regexp.MustCompile(`^(endpoint|param|request|response|auth|schema)-[a-z-]+$`)

func emittedIDs(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range []string{"specdiff.go", "schema.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if fn, ok := call.Fun.(*ast.Ident); ok && len(call.Args) > 0 {
					if lit, ok := call.Args[len(call.Args)-1].(*ast.BasicLit); ok && fn.Name == "dirCheckID" {
						suffix, _ := strconv.Unquote(lit.Value)
						out["request-"+suffix], out["response-"+suffix] = true, true
					}
					if fn.Name == "typeCheckID" {
						out["request-type-changed"], out["response-type-changed"] = true, true
					}
				}
			}
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, _ := strconv.Unquote(lit.Value)
				if checkIDRe.MatchString(s) {
					out[s] = true
				}
			}
			return true
		})
	}
	return out
}

func TestEveryCheckIsInInventory(t *testing.T) {
	emitted := emittedIDs(t)
	for id := range emitted {
		if !checkIndex[id] {
			t.Errorf("emit site uses %s, which is not in the inventory", id)
		}
	}
	for id := range checkIndex {
		if !emitted[id] {
			t.Errorf("inventory lists %s, which no emit site produces", id)
		}
	}
	if len(emitted) < 50 {
		t.Fatalf("the scan found only %d ids; it is not seeing the emit sites", len(emitted))
	}
}

func TestJoinSwitchIDsExist(t *testing.T) {
	for _, rel := range []string{filepath.Join("..", "agent", "join.go"), filepath.Join("..", "specwatch", "journal.go")} {
		src, err := os.ReadFile(rel)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), rel, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, _ := strconv.Unquote(lit.Value)
				if checkIDRe.MatchString(s) {
					seen++
					if !checkIndex[s] {
						t.Errorf("%s switches on %q, which is not a check id", rel, s)
					}
				}
			}
			return true
		})
		if seen == 0 {
			t.Fatalf("%s: no check ids found; the scan is broken", rel)
		}
	}
}
