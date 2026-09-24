package specdiff

import (
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

func TestDeriveLevelLaw(t *testing.T) {
	cases := []struct {
		effect Effect
		dir    Direction
		scope  Scope
		guards Guards
		want   Level
	}{
		{Narrows, Request, Guaranteed, Guards{}, Err},
		{Narrows, Request, Optional, Guards{}, Warn},
		{Widens, Request, Guaranteed, Guards{}, Info},
		{Widens, Request, Optional, Guards{}, Info},
		{Narrows, Response, Guaranteed, Guards{}, Err},
		{Narrows, Response, Optional, Guards{}, Warn},
		{Widens, Response, Guaranteed, Guards{}, Warn},
		{Widens, Response, Optional, Guards{}, Info},
		{Incomparable, Request, Guaranteed, Guards{}, Err},
		{Incomparable, Response, Guaranteed, Guards{}, Err},
		{Unchanged, Request, Guaranteed, Guards{}, Info},
		{Unchanged, Response, Optional, Guards{}, Info},
		{Narrows, Request, Guaranteed, Guards{DeprecatedHonored: true}, Warn},
		{Narrows, Response, Guaranteed, Guards{DeprecatedHonored: true}, Warn},
		{Narrows, Request, Optional, Guards{DeprecatedHonored: true}, Warn},
		{Narrows, Response, Guaranteed, Guards{Uncertain: true}, Warn},
		{Incomparable, Response, Guaranteed, Guards{Uncertain: true}, Warn},
		{Widens, Response, Guaranteed, Guards{Uncertain: true}, Warn},
		{Widens, Request, Guaranteed, Guards{Uncertain: true}, Info},
		{Widens, Request, Guaranteed, Guards{DeprecatedHonored: true}, Info},
		{Widens, Request, Guaranteed, Guards{Tolerated: true}, Info},
		{Widens, Response, Guaranteed, Guards{Tolerated: true}, Info},
		{Narrows, Response, Guaranteed, Guards{Tolerated: true}, Info},
		{Narrows, Response, Optional, Guards{Tolerated: true}, Info},
		{Narrows, Request, Guaranteed, Guards{Tolerated: true}, Err},
		{Narrows, Response, Guaranteed, Guards{Tolerated: true, Uncertain: true}, Info},
	}
	for _, c := range cases {
		if got := DeriveLevel(c.effect, c.dir, c.scope, c.guards); got != c.want {
			t.Errorf("DeriveLevel(%s, %s, %s, %+v) = %s, want %s", c.effect, c.dir, c.scope, c.guards, got, c.want)
		}
	}
	for _, e := range []Effect{Narrows, Widens, Incomparable, Unchanged} {
		for _, d := range []Direction{Request, Response} {
			for _, s := range []Scope{Guaranteed, Optional} {
				DeriveLevel(e, d, s, Guards{})
			}
		}
	}
}

func ep(method, template string, mut ...func(*ir.Endpoint)) ir.Endpoint {
	segs := strings.Split(template, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
			segs[i] = "{}"
		}
	}
	canonical := strings.Join(segs, "/")
	e := ir.Endpoint{
		ID:            method + " " + template,
		Method:        ir.Explicit(method, ""),
		PathTemplate:  ir.Explicit(template, ""),
		CanonicalPath: canonical,
		Deprecated:    ir.Explicit(false, ""),
	}
	for _, m := range mut {
		m(&e)
	}
	return e
}

func def(eps ...ir.Endpoint) *ir.ApiDefinition {
	return &ir.ApiDefinition{Endpoints: eps}
}

func strSchema() ir.IrSchemaNode {
	return ir.IrSchemaNode{Type: ir.Explicit("string", ""), Nullable: ir.Explicit(false, "")}
}

func objSchema(props ...ir.PropertySchema) ir.IrSchemaNode {
	return ir.IrSchemaNode{Type: ir.Explicit("object", ""), Nullable: ir.Explicit(false, ""), Properties: props}
}

func prop(name string, required bool, schema ir.IrSchemaNode) ir.PropertySchema {
	return ir.PropertySchema{Name: name, Required: ir.Explicit(required, ""), Schema: schema}
}

func with200(schema ir.IrSchemaNode) func(*ir.Endpoint) {
	return func(e *ir.Endpoint) {
		e.Responses = []ir.ResponseDef{{StatusCode: "200", Content: []ir.MediaType{{MediaType: "application/json", Schema: schema}}}}
	}
}

func find(t *testing.T, findings []Finding, id string) *Finding {
	t.Helper()
	for i := range findings {
		if findings[i].ID == id {
			return &findings[i]
		}
	}
	t.Fatalf("no finding %q in %v", id, ids(findings))
	return nil
}

func ids(findings []Finding) []string {
	out := make([]string, len(findings))
	for i, f := range findings {
		out[i] = f.ID + "/" + string(f.Level)
	}
	return out
}

func assertAbsent(t *testing.T, findings []Finding, id string) {
	t.Helper()
	for _, f := range findings {
		if f.ID == id {
			t.Fatalf("unexpected finding %q: %+v", id, f)
		}
	}
}

func TestEndpointRemovedAndAdded(t *testing.T) {
	old := def(ep("GET", "/widgets"), ep("GET", "/legacy"))
	niw := def(ep("GET", "/widgets"), ep("POST", "/widgets"))
	fs := Diff(old, niw)
	if f := find(t, fs, "endpoint-removed"); f.Level != Err || f.Template != "/legacy" {
		t.Fatalf("endpoint-removed: %+v", f)
	}
	if f := find(t, fs, "endpoint-added"); f.Level != Info || f.Method != "POST" {
		t.Fatalf("endpoint-added: %+v", f)
	}
}

func TestDeprecatedRemovalIsHonoredSunset(t *testing.T) {
	old := def(ep("GET", "/legacy", func(e *ir.Endpoint) { e.Deprecated = ir.Explicit(true, "") }))
	fs := Diff(old, def())
	if f := find(t, fs, "endpoint-removed"); f.Level != Warn {
		t.Fatalf("deprecated removal should be WARN (honored sunset), got %s", f.Level)
	}
}

func TestPathParamRenameIsComparedNotReAdded(t *testing.T) {
	oldEp := ep("GET", "/widgets/{id}")
	oldEp.Parameters = []ir.Parameter{{Name: "id", Location: "path", Required: ir.Explicit(true, ""), Schema: strSchema()}}
	newEp := ep("GET", "/widgets/{widgetId}")
	intSchema := ir.IrSchemaNode{Type: ir.Explicit("integer", ""), Nullable: ir.Explicit(false, "")}
	newEp.Parameters = []ir.Parameter{{Name: "widgetId", Location: "path", Required: ir.Explicit(true, ""), Schema: intSchema}}

	fs := Diff(def(oldEp), def(newEp))
	assertAbsent(t, fs, "endpoint-removed")
	assertAbsent(t, fs, "endpoint-added")
	assertAbsent(t, fs, "param-removed")
	assertAbsent(t, fs, "param-added-required")
	if f := find(t, fs, "param-renamed"); f.Level != Info {
		t.Fatalf("param-renamed: %+v", f)
	}

	if f := find(t, fs, "request-type-changed"); f.Level != Err {
		t.Fatalf("renamed param type change should still be ERR: %+v", f)
	}
}

func TestNewRequiredParamIsErr(t *testing.T) {
	oldEp := ep("GET", "/widgets")
	newEp := ep("GET", "/widgets")
	newEp.Parameters = []ir.Parameter{{Name: "tenant", Location: "query", Required: ir.Explicit(true, ""), Schema: strSchema()}}
	fs := Diff(def(oldEp), def(newEp))
	if f := find(t, fs, "param-added-required"); f.Level != Err {
		t.Fatalf("new required param: %+v", f)
	}
}

func TestNewOptionalParamIsInfo(t *testing.T) {
	newEp := ep("GET", "/widgets")
	newEp.Parameters = []ir.Parameter{{Name: "filter", Location: "query", Required: ir.Explicit(false, ""), Schema: strSchema()}}
	fs := Diff(def(ep("GET", "/widgets")), def(newEp))
	if f := find(t, fs, "param-added-optional"); f.Level != Info {
		t.Fatalf("new optional param: %+v", f)
	}
}

func TestIsUncertainStillCoversLLMExtracted(t *testing.T) {
	newEp := ep("GET", "/widgets")
	newEp.Parameters = []ir.Parameter{{Name: "tenant", Location: "query",
		Required: ir.Prov[bool]{Value: true, Provenance: ir.ProvenanceLLMExtracted, Confidence: 0.6, Evidence: "extracted"}, Schema: strSchema()}}
	fs := Diff(def(ep("GET", "/widgets")), def(newEp))
	if f := find(t, fs, "param-added-required"); f.Level != Warn {
		t.Fatalf("inferred-required must not page as certain break: %+v", f)
	}
}

func TestTypeLattice(t *testing.T) {
	cases := []struct {
		from, to string
		dir      Direction
		effect   Effect
	}{
		{"integer", "number", Request, Widens},
		{"number", "integer", Request, Narrows},
		{"integer", "number", Response, Widens},
		{"number", "integer", Response, Narrows},
		{"string", "object", Response, Incomparable},
		{"boolean", "string", Request, Incomparable},
	}
	for _, c := range cases {
		eff, _, changed := typeEffect(c.from, c.to, c.dir)
		if !changed || eff != c.effect {
			t.Errorf("typeEffect(%s→%s, %s) = %s/%v, want %s", c.from, c.to, c.dir, eff, changed, c.effect)
		}
	}
	if _, _, changed := typeEffect("unknown", "string", Request); changed {
		t.Error("unknown must abstain, never guess")
	}
	if _, _, changed := typeEffect("string", "string", Response); changed {
		t.Error("equal types are not a change")
	}
}

func TestResponseEnumAsymmetry(t *testing.T) {
	mkDef := func(vals []any) *ir.ApiDefinition {
		s := strSchema()
		s.EnumValues = &ir.Prov[[]any]{Value: vals, Provenance: ir.ProvenanceExplicit}
		return def(ep("GET", "/status", with200(objSchema(prop("state", true, s)))))
	}

	fs := Diff(mkDef([]any{"active", "failed"}), mkDef([]any{"active", "failed", "on_hold"}))
	if f := find(t, fs, "response-enum-value-added"); f.Level != Warn {
		t.Fatalf("response enum add: %+v", f)
	}

	fs = Diff(mkDef([]any{"active", "failed"}), mkDef([]any{"active"}))
	if f := find(t, fs, "response-enum-value-removed"); f.Level != Info {
		t.Fatalf("response enum remove: %+v", f)
	}
}

func TestRequestEnumNarrowedIsErr(t *testing.T) {
	mkDef := func(vals []any) *ir.ApiDefinition {
		s := strSchema()
		s.EnumValues = &ir.Prov[[]any]{Value: vals, Provenance: ir.ProvenanceExplicit}
		e := ep("POST", "/orders")
		e.RequestBody = &ir.RequestBody{Required: ir.Explicit(true, ""),
			Content: []ir.MediaType{{MediaType: "application/json", Schema: objSchema(prop("mode", true, s))}}}
		return def(e)
	}
	fs := Diff(mkDef([]any{"card", "transfer"}), mkDef([]any{"card"}))
	if f := find(t, fs, "request-enum-value-removed"); f.Level != Err {
		t.Fatalf("request enum narrow: %+v", f)
	}
}

func TestResponsePropertyLifecycle(t *testing.T) {
	oldDef := def(ep("GET", "/w", with200(objSchema(
		prop("id", true, strSchema()),
		prop("note", false, strSchema()),
	))))
	newDef := def(ep("GET", "/w", with200(objSchema(
		prop("note", false, strSchema()),
		prop("extra", false, strSchema()),
	))))
	fs := Diff(oldDef, newDef)
	if f := find(t, fs, "response-required-property-removed"); f.Level != Warn {
		t.Fatalf("guaranteed property removed widens the response set, WARN by law: %+v", f)
	}

	if f := find(t, fs, "response-property-added"); f.Level != Info {
		t.Fatalf("additive property: %+v", f)
	}
}

func TestOptionalResponsePropertyRemovedIsWarn(t *testing.T) {
	oldDef := def(ep("GET", "/w", with200(objSchema(prop("note", false, strSchema())))))
	newDef := def(ep("GET", "/w", with200(objSchema())))
	fs := Diff(oldDef, newDef)
	if f := find(t, fs, "response-property-removed"); f.Level != Warn {
		t.Fatalf("optional removal is guarded to WARN: %+v", f)
	}
}

func TestResponseNullableAddedIsWarn(t *testing.T) {
	nullable := strSchema()
	nullable.Nullable = ir.Explicit(true, "")
	oldDef := def(ep("GET", "/w", with200(objSchema(prop("ref", true, strSchema())))))
	newDef := def(ep("GET", "/w", with200(objSchema(prop("ref", true, nullable)))))
	fs := Diff(oldDef, newDef)
	if f := find(t, fs, "response-nullable-added"); f.Level != Warn {
		t.Fatalf("nullable added: %+v", f)
	}
}

func TestStatusRemoval(t *testing.T) {
	mkEp := func(statuses ...string) ir.Endpoint {
		e := ep("GET", "/w")
		for _, s := range statuses {
			e.Responses = append(e.Responses, ir.ResponseDef{StatusCode: s})
		}
		return e
	}
	fs := Diff(def(mkEp("200", "404")), def(mkEp("404")))
	if f := find(t, fs, "response-status-removed"); f.Level != Err {
		t.Fatalf("success status removed: %+v", f)
	}
	fs = Diff(def(mkEp("200", "404")), def(mkEp("200")))
	if f := find(t, fs, "response-status-removed"); f.Level != Info {
		t.Fatalf("error status removed is tolerated shrink: %+v", f)
	}
	fs = Diff(def(mkEp("200")), def(mkEp("200", "429")))
	if f := find(t, fs, "response-status-added"); f.Level != Warn {
		t.Fatalf("status added: %+v", f)
	}
}

func TestRequestBodyBecameRequired(t *testing.T) {
	mkEp := func(required bool) ir.Endpoint {
		e := ep("POST", "/w")
		e.RequestBody = &ir.RequestBody{Required: ir.Explicit(required, "")}
		return e
	}
	fs := Diff(def(mkEp(false)), def(mkEp(true)))
	if f := find(t, fs, "request-body-became-required"); f.Level != Err {
		t.Fatalf("body became required: %+v", f)
	}
}

func TestAuthSchemeChanges(t *testing.T) {
	mkDef := func(kind, scheme string) *ir.ApiDefinition {
		d := def(ep("GET", "/w"))
		s := ir.Explicit(scheme, "")
		d.AuthSchemes = []ir.AuthScheme{{ID: "main", Name: "main", Kind: ir.Explicit(kind, ""), Scheme: &s}}
		return d
	}
	fs := Diff(mkDef("http", "bearer"), mkDef("http", "basic"))
	if f := find(t, fs, "auth-scheme-changed"); f.Level != Err {
		t.Fatalf("auth changed: %+v", f)
	}
	fs = Diff(mkDef("http", "bearer"), def(ep("GET", "/w")))
	if f := find(t, fs, "auth-scheme-removed"); f.Level != Err {
		t.Fatalf("auth removed: %+v", f)
	}
}

func TestEndpointGainedAuth(t *testing.T) {
	newEp := ep("GET", "/w")
	newEp.Security = []ir.SecurityRequirement{{SchemeID: "main"}}
	fs := Diff(def(ep("GET", "/w")), def(newEp))
	if f := find(t, fs, "endpoint-security-added"); f.Level != Err {
		t.Fatalf("gained auth: %+v", f)
	}
}

func TestRefResolutionAndCycleSafety(t *testing.T) {
	refName := "sch_widget"
	mkDef := func(propName string) *ir.ApiDefinition {
		refNode := ir.IrSchemaNode{Ref: &refName, Type: ir.Explicit("object", ""), Nullable: ir.Explicit(false, "")}
		d := def(ep("GET", "/w", with200(refNode)))
		d.Schemas = []ir.NamedSchema{{ID: refName, Name: "Widget",
			Schema: objSchema(prop(propName, true, strSchema()))}}
		return d
	}
	fs := Diff(mkDef("id"), mkDef("uid"))
	find(t, fs, "response-required-property-removed")
	find(t, fs, "response-property-added")

	self := "sch_self"
	selfDef := def(ep("GET", "/w", with200(ir.IrSchemaNode{Ref: &self})))
	selfDef.Schemas = []ir.NamedSchema{{ID: self, Name: "Self", Schema: ir.IrSchemaNode{Ref: &self}}}
	_ = Diff(selfDef, selfDef)
}

func TestNoFindingsOnIdenticalSpecs(t *testing.T) {
	d := def(ep("GET", "/widgets/{id}", with200(objSchema(prop("id", true, strSchema())))))
	if fs := Diff(d, d); len(fs) != 0 {
		t.Fatalf("identical specs produced findings: %v", ids(fs))
	}
}

func TestFingerprintPinned(t *testing.T) {
	f := Finding{ID: "endpoint-removed", Method: "GET", Template: "/legacy"}
	if got := f.Fingerprint(); got != "fp_710dbf2fd875" {
		t.Fatalf("fingerprint formula changed: %s — this re-alerts every persisted declared drift", got)
	}
}

func TestFingerprintInjectivity(t *testing.T) {
	a := Finding{ID: "x", Method: "GET", Template: "/w", Args: []string{"a:b", "c"}}
	b := Finding{ID: "x", Method: "GET", Template: "/w", Args: []string{"a", "b:c"}}
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("arg boundaries must not shift")
	}
	if a.Fingerprint() != a.Fingerprint() {
		t.Fatal("not deterministic")
	}
}

func TestBreakingFloor(t *testing.T) {
	fs := []Finding{{Level: Warn}, {Level: Info}}
	if Breaking(fs, Err) {
		t.Fatal("no ERR present")
	}
	if !Breaking(fs, Warn) {
		t.Fatal("WARN floor should trip")
	}
}

func TestDeterministicOrder(t *testing.T) {
	old := def(ep("GET", "/b"), ep("GET", "/a"), ep("POST", "/a"))
	fs1 := Diff(old, def())
	fs2 := Diff(old, def())
	if len(fs1) != 3 {
		t.Fatalf("want 3 removals, got %v", ids(fs1))
	}
	for i := range fs1 {
		if fs1[i].Fingerprint() != fs2[i].Fingerprint() {
			t.Fatal("order not deterministic")
		}
	}
	if fs1[0].Template != "/a" || fs1[0].Method != "GET" || fs1[1].Method != "POST" {
		t.Fatalf("wrong order: %v", ids(fs1))
	}
}
