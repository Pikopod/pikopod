package agent

import (
	"sort"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sanitize"
)

func ruleFor(rules []sanitize.Rule, field string) (sanitize.Rule, bool) {
	for _, r := range rules {
		if r.Field == field {
			return r, true
		}
	}
	return sanitize.Rule{}, false
}

// A spec-declared enum (EXPLICIT provenance, the only kind a real OpenAPI
// `enum:` produces) becomes a rule allowing exactly its declared members —
// the reproduction of issue #28's acceptance check at the IR layer.
func TestEnumRulesFromContract_DeclaredEnum(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(`{
	  "openapi": "3.0.0", "info": {"title": "p", "version": "1"},
	  "paths": {"/tx": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {
	    "type": "object",
	    "properties": {
	      "id": {"type": "string"},
	      "status": {"type": "string", "enum": ["ACTIVE", "PENDING"]},
	      "amount": {"type": "integer"}
	    }
	  }}}}}}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	rules := enumRulesFromContract(def)
	r, ok := ruleFor(rules, "status")
	if !ok {
		t.Fatalf("expected a rule for field %q, got %+v", "status", rules)
	}
	if r.Mode != sanitize.ModeAllow {
		t.Fatalf("enum rule mode = %s, want ALLOW", r.Mode)
	}
	got := append([]string(nil), r.Values...)
	sort.Strings(got)
	want := []string{"ACTIVE", "PENDING"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("rule.Values = %v, want %v", got, want)
	}
	if _, ok := ruleFor(rules, "id"); ok {
		t.Fatalf("non-enum field %q must not get a rule", "id")
	}
	if _, ok := ruleFor(rules, "amount"); ok {
		t.Fatalf("non-enum field %q must not get a rule", "amount")
	}
}

// Only EXPLICIT/DERIVED may unlock redaction — deliberately stricter than
// Prov.IsGuess(), which lets LLM_EXTRACTED through. A model-guessed or
// model-written enum must never be trusted to write raw values to disk.
func TestEnumRulesFromContract_ExcludesUntrustedProvenance(t *testing.T) {
	mk := func(provenance string) *ir.ApiDefinition {
		ev := ir.Prov[[]any]{Value: []any{"GUESSED_A", "GUESSED_B"}, Provenance: provenance}
		return &ir.ApiDefinition{
			Endpoints: []ir.Endpoint{{
				Responses: []ir.ResponseDef{{
					Content: []ir.MediaType{{
						Schema: ir.IrSchemaNode{
							Properties: []ir.PropertySchema{{
								Name:   "risk_level",
								Schema: ir.IrSchemaNode{EnumValues: &ev},
							}},
						},
					}},
				}},
			}},
		}
	}

	for _, provenance := range []string{ir.ProvenanceInferred, ir.ProvenanceLLMExtracted} {
		rules := enumRulesFromContract(mk(provenance))
		if _, ok := ruleFor(rules, "risk_level"); ok {
			t.Fatalf("provenance %s must not produce an enum rule, got %+v", provenance, rules)
		}
	}

	// Sanity check the harness: EXPLICIT on the same shape DOES produce one.
	rules := enumRulesFromContract(mk(ir.ProvenanceExplicit))
	if _, ok := ruleFor(rules, "risk_level"); !ok {
		t.Fatalf("EXPLICIT provenance should produce a rule, got %+v", rules)
	}
}

func TestEnumRulesFromContract_NilContract(t *testing.T) {
	if rules := enumRulesFromContract(nil); rules != nil {
		t.Fatalf("nil contract should produce no rules, got %+v", rules)
	}
}
