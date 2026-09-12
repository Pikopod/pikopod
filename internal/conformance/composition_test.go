package conformance

import (
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/proxy"
)

// allOf responses are flattened and checked member-wise; oneOf responses
// pass on ANY fitting variant and violate only when none fits.
const compositionSpec = `{
  "openapi": "3.0.0", "info": {"title": "C", "version": "1"},
  "paths": {
    "/merged": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
      "allOf": [
        {"type": "object", "required": ["id"], "properties": {"id": {"type": "string"}}},
        {"type": "object", "required": ["status"], "properties": {"status": {"type": "string"}}}
      ]
    }}}}}}},
    "/choice": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
      "oneOf": [
        {"type": "object", "required": ["card"], "properties": {"card": {"type": "string"}}},
        {"type": "object", "required": ["bank"], "properties": {"bank": {"type": "string"}}}
      ]
    }}}}}}}
  }
}`

func compositionDef(t *testing.T) *ir.ApiDefinition {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(compositionSpec))
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func compRec(path string, body any) *proxy.Record {
	return &proxy.Record{Method: "GET", Path: path, Status: 200, RespKind: "json", RespBody: body}
}

func TestAllOfIsFlattenedAndChecked(t *testing.T) {
	def := compositionDef(t)
	// Missing "status" (required by the SECOND allOf member) must now be a
	// violation — the pre-flattening code skipped composed schemas entirely.
	report := Check(def, []*proxy.Record{
		compRec("/merged", map[string]any{"id": "x_1"}),
	})
	if len(report.Violations) != 1 || report.Violations[0].Code != "required" || report.Violations[0].Pointer != "/status" {
		t.Fatalf("violations: %+v", report.Violations)
	}
	// Both members satisfied → clean.
	report = Check(def, []*proxy.Record{
		compRec("/merged", map[string]any{"id": "x_1", "status": "ok"}),
	})
	if len(report.Violations) != 0 {
		t.Fatalf("clean record flagged: %+v", report.Violations)
	}
}

func TestOneOfPassesOnAnyVariant(t *testing.T) {
	def := compositionDef(t)
	report := Check(def, []*proxy.Record{
		compRec("/choice", map[string]any{"card": "tok_1"}), // fits variant 1
		compRec("/choice", map[string]any{"bank": "058"}),   // fits variant 2
	})
	if len(report.Violations) != 0 {
		t.Fatalf("fitting variants flagged: %+v", report.Violations)
	}
}

func TestOneOfNoVariantFitsIsCertainViolation(t *testing.T) {
	def := compositionDef(t)
	report := Check(def, []*proxy.Record{
		compRec("/choice", map[string]any{"wallet": "w_1"}), // fits neither
	})
	if len(report.Violations) != 1 || report.Violations[0].Code != "composition" {
		t.Fatalf("violations: %+v", report.Violations)
	}
}
