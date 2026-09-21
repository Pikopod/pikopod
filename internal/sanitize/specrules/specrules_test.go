package specrules

import (
	"reflect"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sanitize"
)

const enumSpec = `{"openapi":"3.1.0","info":{"title":"Pay","version":"1"},
"components":{"schemas":{"Charge":{"type":"object","properties":{
  "status":{"type":"string","enum":["ACTIVE","PENDING"]},
  "currency":{"type":"string","enum":["NGN","USD"]},
  "lines":{"type":"array","items":{"type":"object","properties":{"kind":{"type":"string","enum":["fee","principal"]}}}}
}}}},
"paths":{"/charges":{"post":{
  "requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"channel":{"type":"string","enum":["card","bank"]}}}}}},
  "responses":{
    "200":{"description":"ok","content":{"application/json":{"schema":{"$ref":"#/components/schemas/Charge"}}}},
    "400":{"description":"bad","content":{"application/json":{"schema":{"type":"object","properties":{"code":{"type":"string","enum":["INVALID"]}}}}}}
  }}}}}`

func TestRulesComeFromSuccessResponseEnumsOnly(t *testing.T) {
	def, err := importer.NormalizeOpenAPI([]byte(enumSpec))
	if err != nil {
		t.Fatal(err)
	}
	got := FromContract(def)
	want := []sanitize.Rule{
		{Field: "currency", Mode: sanitize.ModeAllow, AllowedValues: []string{"NGN", "USD"}},
		{Field: "kind", Mode: sanitize.ModeAllow, AllowedValues: []string{"fee", "principal"}},
		{Field: "status", Mode: sanitize.ModeAllow, AllowedValues: []string{"ACTIVE", "PENDING"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rules:\n got %+v\nwant %+v", got, want)
	}
}

// A guess must never relax redaction: extracted enums unlock nothing.
func TestExtractedEnumsUnlockNothing(t *testing.T) {
	def, err := importer.NormalizeLLMExtracted([]byte(enumSpec))
	if err != nil {
		t.Fatal(err)
	}
	if rules := FromContract(def); len(rules) != 0 {
		t.Fatalf("LLM_EXTRACTED enums produced rules: %+v", rules)
	}
	def, _ = importer.NormalizeOpenAPI([]byte(enumSpec))
	for i := range def.Schemas {
		walkProv(&def.Schemas[i].Schema, ir.ProvenanceDerived)
	}
	if rules := FromContract(def); len(rules) != 3 {
		t.Fatalf("DERIVED enums are deterministic transforms of explicit data and count: %+v", rules)
	}
}

func walkProv(n *ir.IrSchemaNode, prov string) {
	if n.EnumValues != nil {
		n.EnumValues.Provenance = prov
	}
	for i := range n.Properties {
		walkProv(&n.Properties[i].Schema, prov)
	}
	if n.Items != nil {
		walkProv(n.Items, prov)
	}
}

func TestNoContractNoRules(t *testing.T) {
	if got := ForContracts(map[string]*ir.ApiDefinition{"pay": nil}); len(got) != 0 {
		t.Fatalf("an upstream without a contract must get no rules: %v", got)
	}
}
