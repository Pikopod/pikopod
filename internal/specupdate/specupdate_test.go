package specupdate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pikopod/pikopod/internal/conformance"
	"github.com/pikopod/pikopod/internal/contract"
)

const yamlSpec = `openapi: 3.0.0
info:
  title: T # the provider's own comment
  version: "1"
paths:
  /tx/{id}:
    get:
      # operation comment that must survive
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Tx'
        "404":
          description: missing
components:
  schemas:
    Tx:
      type: object
      properties:
        status:
          type: string
          enum: [active, failed]
        ref:
          type: string
`

func change(kind ChangeKind, mut ...func(*Change)) Change {
	c := Change{Kind: kind, Method: "GET", Template: "/tx/{id}", Status: 200,
		Reason: "test", Evidence: Evidence{Occurrences: 7}}
	for _, m := range mut {
		m(&c)
	}
	return c
}

func TestEnumUnionThroughRef(t *testing.T) {
	res, err := Apply([]byte(yamlSpec), []Change{
		change(EnumUnion, func(c *Change) { c.Pointer, c.Value = "/status", "on_hold" }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 || len(res.Skipped) != 0 {
		t.Fatalf("applied=%d skipped=%d", len(res.Applied), len(res.Skipped))
	}

	op := res.Applied[0].Ops[0]
	if op.Path != "/components/schemas/Tx/properties/status/enum/-" || op.Value != "on_hold" {
		t.Fatalf("op: %+v", op)
	}
	out := string(res.Out)
	if !strings.Contains(out, "on_hold") || !strings.Contains(out, "x-pikopod-observed") {
		t.Fatalf("output missing the union/annotation:\n%s", out)
	}

	for _, comment := range []string{"the provider's own comment", "operation comment that must survive"} {
		if !strings.Contains(out, comment) {
			t.Fatalf("comment lost: %q\n%s", comment, out)
		}
	}
}

func TestAdditiveOnlyNeverNarrows(t *testing.T) {
	narrowing := []Change{
		change(Retype, func(c *Change) { c.Pointer, c.Value = "/status", "number" }),
		change(MakeOptional, func(c *Change) { c.Pointer = "/ref" }),
		change(AddEndpoint, func(c *Change) { c.Template = "/new" }),
	}
	res, err := Apply([]byte(yamlSpec), narrowing)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 || len(res.Suggestions) != 3 {
		t.Fatalf("narrowings must NEVER apply: applied=%d suggestions=%d", len(res.Applied), len(res.Suggestions))
	}

	clean, _ := Apply([]byte(yamlSpec), nil)
	if string(res.Out) != string(clean.Out) {
		t.Fatal("suggestions must not touch the document")
	}
}

func TestAddStatusAndAlreadyDeclaredSkip(t *testing.T) {
	res, err := Apply([]byte(yamlSpec), []Change{
		change(AddStatus, func(c *Change) { c.Status = 429 }),
		change(AddStatus, func(c *Change) { c.Status = 404 }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 || len(res.Skipped) != 1 {
		t.Fatalf("applied=%d skipped=%d", len(res.Applied), len(res.Skipped))
	}
	if !strings.Contains(string(res.Out), `"429"`) {
		t.Fatalf("429 not added:\n%s", res.Out)
	}
}

func TestNullableWrap(t *testing.T) {
	res, err := Apply([]byte(yamlSpec), []Change{
		change(NullableWrap, func(c *Change) { c.Pointer = "/ref" }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("%+v", res.Skipped)
	}
	if !strings.Contains(string(res.Out), "nullable: true") {
		t.Fatalf("no nullable:\n%s", res.Out)
	}

	res2, err := Apply(res.Out, []Change{change(NullableWrap, func(c *Change) { c.Pointer = "/ref" })})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Applied) != 0 {
		t.Fatal("fixpoint violated: nullable applied twice")
	}
}

func TestAddPropertyWithProvenance(t *testing.T) {
	res, err := Apply([]byte(yamlSpec), []Change{
		change(AddProperty, func(c *Change) {
			c.Pointer, c.Value, c.PropType = "", "fee", "number"
			c.Evidence = Evidence{Presence: 0.99, Since: "2026-09-01T00:00:00Z"}
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("skipped: %+v", res.Skipped)
	}
	out := string(res.Out)
	for _, want := range []string{"fee:", "type: number", "presence: 0.99", "since:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}

	res2, _ := Apply([]byte(yamlSpec), []Change{
		change(AddProperty, func(c *Change) { c.Pointer, c.Value = "", "status" }),
	})
	if len(res2.Applied) != 0 {
		t.Fatal("must not overwrite an existing property")
	}
}

func TestEnumUnionRefusesWhenNoEnum(t *testing.T) {
	res, _ := Apply([]byte(yamlSpec), []Change{
		change(EnumUnion, func(c *Change) { c.Pointer, c.Value = "/ref", "x" }),
	})
	if len(res.Applied) != 0 || len(res.Skipped) != 1 {
		t.Fatal("no documented enum — nothing to union; must skip, not invent")
	}
}

func TestJSONSourceOrderedReEmit(t *testing.T) {
	jsonSpec := `{
  "openapi": "3.0.0",
  "info": {"title": "T", "version": "1"},
  "paths": {
    "/w": {
      "get": {
        "responses": {
          "200": {
            "content": {
              "application/json": {
                "schema": {"type": "object", "properties": {"state": {"type": "string", "enum": ["a", "b"]}}}
              }
            }
          }
        }
      }
    }
  }
}`
	res, err := Apply([]byte(jsonSpec), []Change{
		change(EnumUnion, func(c *Change) { c.Template, c.Pointer, c.Value = "/w", "/state", "c" }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.JSON {
		t.Fatal("JSON source must re-emit JSON")
	}
	var parsed map[string]any
	if err := json.Unmarshal(res.Out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, res.Out)
	}
	out := string(res.Out)

	if !(strings.Index(out, `"openapi"`) < strings.Index(out, `"info"`) &&
		strings.Index(out, `"info"`) < strings.Index(out, `"paths"`)) {
		t.Fatalf("key order lost:\n%s", out)
	}
	if !strings.Contains(out, `"c"`) {
		t.Fatalf("union missing:\n%s", out)
	}
}

func TestSwagger2SchemaShape(t *testing.T) {
	sw2 := `swagger: "2.0"
info: {title: T, version: "1"}
paths:
  /w:
    get:
      responses:
        "200":
          description: ok
          schema:
            type: object
            properties:
              state: {type: string, enum: [a]}
`
	res, err := Apply([]byte(sw2), []Change{
		change(EnumUnion, func(c *Change) { c.Template, c.Pointer, c.Value = "/w", "/state", "b" }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 1 {
		t.Fatalf("swagger2 response.schema shape: %+v", res.Skipped)
	}
}

func TestDeriveChangesFromConformance(t *testing.T) {
	rep := &conformance.Report{Violations: []conformance.Violation{
		{Method: "GET", Template: "/tx/{id}", Status: 418, Code: "status_undeclared", Occurrences: 5},
		{Method: "GET", Template: "/tx/{id}", Status: 200, Pointer: "/status", Code: "enum", Observed: "on_hold", Occurrences: 3},
		{Method: "GET", Template: "/tx/{id}", Status: 200, Pointer: "/ref", Code: "type", Observed: "null", Occurrences: 2},
		{Method: "GET", Template: "/tx/{id}", Status: 200, Pointer: "/amt", Code: "type", Observed: "string", Occurrences: 9},
		{Method: "GET", Template: "/tx/{id}", Status: 200, Pointer: "/req", Code: "required", Occurrences: 4},
	}}
	changes := DeriveChanges(rep, nil)
	kinds := map[ChangeKind]int{}
	for _, c := range changes {
		kinds[c.Kind]++
	}
	want := map[ChangeKind]int{AddStatus: 1, EnumUnion: 1, NullableWrap: 1, Retype: 1, MakeOptional: 1}
	for k, n := range want {
		if kinds[k] != n {
			t.Fatalf("kind %s: got %d want %d (%+v)", k, kinds[k], n, changes)
		}
	}
}

func TestDeriveChangesFromOverlayAndDedupe(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ov := &contract.Overlay{Admissions: []contract.Admission{
		{Kind: contract.AdmitField, Method: "GET", Template: "/tx/{id}", StatusClass: "2xx",
			Field: "data.fee", Type: "number", Presence: 0.99, At: at},
		{Kind: contract.AdmitValue, Method: "GET", Template: "/tx/{id}", StatusClass: "2xx",
			Field: "data.status", Value: "on_hold", At: at},
		{Kind: contract.AdmitStatus, Method: "GET", Template: "/tx/{id}", StatusClass: "4xx", Value: "418", At: at},
		{Kind: contract.AdmitEndpoint, Method: "POST", Template: "/new", StatusClass: "2xx", At: at},
	}}
	rep := &conformance.Report{Violations: []conformance.Violation{

		{Method: "GET", Template: "/tx/{id}", Status: 418, Code: "status_undeclared", Occurrences: 5},
	}}
	changes := DeriveChanges(rep, ov)
	statuses := 0
	for _, c := range changes {
		if c.Kind == AddStatus {
			statuses++
		}
		if c.Kind == AddProperty {
			if c.Pointer != "/data" || c.Value != "fee" || c.PropType != "number" {
				t.Fatalf("add-property mapping: %+v", c)
			}
			if c.Evidence.Presence != 0.99 || c.Evidence.Since == "" {
				t.Fatalf("evidence: %+v", c.Evidence)
			}
		}
	}
	if statuses != 1 {
		t.Fatalf("duplicate AddStatus not collapsed: %d", statuses)
	}
}

func TestDottedToPointer(t *testing.T) {
	cases := map[string]string{
		"data.fee":           "/data/fee",
		"data.entries[].fee": "/data/entries/*/fee",
		"items[]":            "/items/*",
		"":                   "",
	}
	for in, want := range cases {
		if got := dottedToPointer(in); got != want {
			t.Errorf("dottedToPointer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindResponseLadder(t *testing.T) {
	rangeSpec := `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /w:
    get:
      responses:
        "4XX":
          content:
            application/json:
              schema:
                type: object
                properties:
                  code: {type: string, enum: [expired]}
`
	res, err := Apply([]byte(rangeSpec), []Change{
		change(EnumUnion, func(c *Change) { c.Template, c.Status, c.Pointer, c.Value = "/w", 404, "/code", "revoked" }),
	})
	if err != nil || len(res.Applied) != 1 {
		t.Fatalf("range-key anchor: %v applied=%d skipped=%+v", err, len(res.Applied), res.Skipped)
	}

	defaultSpec := `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /w:
    get:
      responses:
        default:
          content:
            application/json:
              schema:
                type: object
                properties:
                  code: {type: string, enum: [expired]}
`
	res, err = Apply([]byte(defaultSpec), []Change{
		change(EnumUnion, func(c *Change) { c.Template, c.Status, c.Pointer, c.Value = "/w", 500, "/code", "revoked" }),
	})
	if err != nil || len(res.Applied) != 1 {
		t.Fatalf("default anchor: %v applied=%d", err, len(res.Applied))
	}

	classSpec := `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /w:
    get:
      responses:
        "201":
          content:
            application/json:
              schema:
                type: object
                properties:
                  state: {type: string, enum: [a]}
`
	res, err = Apply([]byte(classSpec), []Change{
		change(EnumUnion, func(c *Change) {
			c.Template, c.Status, c.StatusClass, c.Pointer, c.Value = "/w", 0, "2xx", "/state", "b"
		}),
	})
	if err != nil || len(res.Applied) != 1 {
		t.Fatalf("class anchor: %v applied=%d skipped=%+v", err, len(res.Applied), res.Skipped)
	}
}

func TestApplyRefusesOversizedDocument(t *testing.T) {
	big := make([]byte, maxApplyBytes+1)
	if _, err := Apply(big, nil); err == nil {
		t.Fatal("oversized document must be refused")
	}
}
