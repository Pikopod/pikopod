package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
)

// A spec that DECLARES its 422 shape — the sandbox must answer invalid
// requests with the provider's own error body, not pikopod's.
const declaredErrorSpec = `{
  "openapi": "3.0.0", "info": {"title": "E", "version": "1"},
  "paths": {"/charges": {"post": {
    "requestBody": {"required": true, "content": {"application/json": {"schema": {
      "type": "object", "required": ["amount"], "properties": {"amount": {"type": "number"}}
    }}}},
    "responses": {
      "201": {"description": "c", "content": {"application/json": {"schema": {"type": "object", "properties": {"id": {"type": "string"}}}}}},
      "422": {"description": "invalid", "content": {"application/json": {"schema": {
        "type": "object", "required": ["error"], "properties": {"error": {
          "type": "object", "properties": {"type": {"type": "string"}, "doc_url": {"type": "string"}}
        }}
      }}}}
    }
  }}}
}`

func loadSpecDef(t *testing.T, raw string) *ir.ApiDefinition {
	t.Helper()
	def, err := importer.NormalizeOpenAPI([]byte(raw))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return def
}

// Provider-shaped validation errors (Prism's ladder): a declared 422
// schema wins over the neutral shape, the
// exact violations ride the header, and the body is seed-deterministic.
func TestValidationErrorUsesDeclaredSchema(t *testing.T) {
	e := newEngine(t, loadSpecDef(t, declaredErrorSpec), Config{ID: "sbx_ve", Seed: "ve-1"})
	got := do(t, e, "POST", "/charges", `{"currency":"NGN"}`, nil)

	if got.status != 422 {
		t.Fatalf("declared 422 must win the ladder, got %d %s", got.status, got.body)
	}
	var body map[string]any
	json.Unmarshal([]byte(got.body), &body)
	if _, ok := body["error"]; !ok {
		t.Fatalf("body must be the PROVIDER's declared shape: %s", got.body)
	}
	if _, pikopodShaped := body["errors"]; pikopodShaped {
		t.Fatalf("neutral pikopod shape must not leak when a schema is declared: %s", got.body)
	}
	viol := got.headers[violationsHeader]
	if !strings.Contains(viol, "amount is required") {
		t.Fatalf("violations header must carry the exact failures: %q", viol)
	}
	// Deterministic: same seed, same synthesized error body.
	e2 := newEngine(t, loadSpecDef(t, declaredErrorSpec), Config{ID: "sbx_ve2", Seed: "ve-1"})
	got2 := do(t, e2, "POST", "/charges", `{"currency":"NGN"}`, nil)
	if got.body != got2.body {
		t.Fatalf("declared-schema error must be seed-deterministic:\n%s\n%s", got.body, got2.body)
	}
}

// Specs that declare NO error schema keep the neutral shape byte-identical
// (transcript parity depends on this) — the header still explains.
func TestValidationErrorFallsBackToNeutralShape(t *testing.T) {
	spec := strings.Replace(declaredErrorSpec, `"422": {"description": "invalid", "content": {"application/json": {"schema": {
        "type": "object", "required": ["error"], "properties": {"error": {
          "type": "object", "properties": {"type": {"type": "string"}, "doc_url": {"type": "string"}}
        }}
      }}}}`, `"422": {"description": "invalid"}`, 1)
	e := newEngine(t, loadSpecDef(t, spec), Config{ID: "sbx_vn", Seed: "vn-1"})
	got := do(t, e, "POST", "/charges", `{"currency":"NGN"}`, nil)
	if got.status != 400 {
		t.Fatalf("no declared schema → neutral 400, got %d", got.status)
	}
	var body map[string]any
	json.Unmarshal([]byte(got.body), &body)
	if body["message"] != "Validation failed" {
		t.Fatalf("neutral shape must be preserved: %s", got.body)
	}
	if got.headers[violationsHeader] == "" {
		t.Fatal("violations header must be present on the neutral shape too")
	}
}

// The violations header is capped: a pathological schema with thousands of
// failures must never blow the header limit (Prism's 8KB rule).
func TestViolationsHeaderCap(t *testing.T) {
	errs := make([]string, 4000)
	for i := range errs {
		errs[i] = "field_" + strings.Repeat("x", 40) + " is required"
	}
	rendered := renderViolations(errs)
	if len(rendered) > maxViolationsHeaderBytes {
		t.Fatalf("header must stay under the cap: %d", len(rendered))
	}
	if !strings.Contains(rendered, "truncated") {
		t.Fatal("truncation must be visible, never silent")
	}
	var parsed []string
	if json.Unmarshal([]byte(rendered), &parsed) != nil || len(parsed) < 2 {
		t.Fatalf("capped header must remain valid JSON with real content: %.80s", rendered)
	}
}
