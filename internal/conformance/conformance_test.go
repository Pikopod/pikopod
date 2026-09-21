package conformance

import (
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/proxy"
)

const conformSpec = `{
  "openapi": "3.0.0", "info": {"title": "C", "version": "1"},
  "paths": {"/charges/{id}": {"get": {
    "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
      "type": "object",
      "required": ["id", "status"],
      "properties": {
        "id": {"type": "string"},
        "status": {"type": "string", "enum": ["pending", "success", "failed"]},
        "amount": {"type": "number"}
      }
    }}}}}
  }}}
}`

func specDef(t *testing.T) *ir.ApiDefinition {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(conformSpec))
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func rec(status int, body any, redacted ...proxy.SectionRedaction) *proxy.Record {
	return &proxy.Record{Method: "GET", Path: "/charges/ch_00000000001", Status: status,
		RespKind: "json", RespBody: body, Redacted: redacted}
}

// The provider disagreeing with its own docs: enum outside the documented
// set, required field absent, wrong type, undeclared status — each a
// structurally certain violation, deduped with occurrence counts.
func TestConformanceViolations(t *testing.T) {
	def := specDef(t)
	report := Check(def, []*proxy.Record{
		rec(200, map[string]any{"id": "ch_1", "status": "succeeded", "amount": float64(5)}), // enum
		rec(200, map[string]any{"id": "ch_1", "status": "succeeded", "amount": float64(5)}), // same → dedupe
		rec(200, map[string]any{"id": "ch_1"}),                                              // required: status absent
		rec(200, map[string]any{"id": "ch_1", "status": "success", "amount": "500"}),        // type
		rec(201, map[string]any{"id": "ch_1"}),                                              // undeclared 2xx status
	})
	if report.Records != 5 {
		t.Fatalf("records counted wrong: %d", report.Records)
	}
	byCode := map[string]Violation{}
	for _, v := range report.Violations {
		byCode[v.Code+v.Pointer] = v
	}
	if v := byCode["enum/status"]; v.Occurrences != 2 || v.Severity != "error" {
		t.Fatalf("enum violation wrong: %+v", report.Violations)
	}
	if v := byCode["required/status"]; v.Occurrences != 1 {
		t.Fatalf("required violation wrong: %+v", report.Violations)
	}
	if v := byCode["type/amount"]; v.Message == "" || !strings.Contains(v.Message, "number") {
		t.Fatalf("type violation wrong: %+v", report.Violations)
	}
	if v := byCode["status_undeclared"]; v.Severity != "error" {
		t.Fatalf("undeclared SUCCESS must be error severity: %+v", report.Violations)
	}
	// A clean record adds nothing.
	clean := Check(def, []*proxy.Record{rec(200, map[string]any{"id": "x", "status": "success"})})
	if len(clean.Violations) != 0 {
		t.Fatalf("clean traffic must not violate: %+v", clean.Violations)
	}
}

// Sanitizer awareness: redacted evidence proves nothing — a DROPped field
// is not a required-violation, a TOKENIZEd value is not an enum-violation.
// Both count as unverifiable instead.
func TestConformanceSuppressesRedactedEvidence(t *testing.T) {
	def := specDef(t)
	report := Check(def, []*proxy.Record{
		rec(200, map[string]any{"id": "ch_1"},
			proxy.SectionRedaction{Section: "resp_body", Pointer: "/status", Mode: "DROP"}),
		rec(200, map[string]any{"id": "ch_1", "status": "xkfjq"},
			proxy.SectionRedaction{Section: "resp_body", Pointer: "/status", Mode: "TOKENIZE"}),
	})
	if len(report.Violations) != 0 {
		t.Fatalf("redacted evidence must never become a violation: %+v", report.Violations)
	}
	if report.Unverifiable != 2 {
		t.Fatalf("suppressions must be counted honestly: %d", report.Unverifiable)
	}
}

func TestLLMExtractedClaimsStillChecked(t *testing.T) {
	def := specDef(t)
	downgraded := false
	for i := range def.Endpoints {
		for j := range def.Endpoints[i].Responses {
			for k := range def.Endpoints[i].Responses[j].Content {
				props := def.Endpoints[i].Responses[j].Content[k].Schema.Properties
				for m := range props {
					if props[m].Name == "status" && props[m].Schema.EnumValues != nil {
						props[m].Schema.EnumValues.Provenance = ir.ProvenanceLLMExtracted
						props[m].Schema.EnumValues.Confidence = 0.7
						downgraded = true
					}
				}
			}
		}
	}
	if !downgraded {
		t.Fatal("setup: enum claim not found to downgrade")
	}
	report := Check(def, []*proxy.Record{
		rec(200, map[string]any{"id": "ch_1", "status": "anything-goes"}),
	})
	for _, v := range report.Violations {
		if v.Code == "enum" {
			return
		}
	}
	t.Fatalf("an extracted enum claim is still a claim the provider can break: %+v", report.Violations)
}
