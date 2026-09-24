package resolve

import (
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
)

const draftSpec = `{"openapi":"3.1.0","info":{"title":"Pay","version":"1"},
"paths":{
  "/payment-intents":{"post":{"operationId":"createIntent","responses":{"201":{"description":"created"}}}},
  "/payment-intents/{id}":{"get":{"operationId":"getIntent","responses":{"200":{"description":"ok"}}}}
}}`

func draftDef(t *testing.T) *ir.ApiDefinition {
	t.Helper()
	def, err := importer.NormalizeLLMExtracted([]byte(draftSpec))
	if err != nil {
		t.Fatal(err)
	}
	if !def.Endpoints[0].Method.IsUncertain() {
		t.Fatal("fixture must be all-inferred")
	}
	return def
}

func TestDraftDefinitionBindsNothingByItself(t *testing.T) {
	_, err := Resolve(draftDef(t), "declines", Options{})
	if err == nil || !strings.Contains(err.Error(), "extracted") {
		t.Fatalf("an all-inferred definition must refuse and say the facts are extracted, got %v", err)
	}
}

func TestBindOverrideGroundsADraftDefinition(t *testing.T) {
	parsed, info, err := ResolveDetailed(draftDef(t), "declines", Options{BindOverrides: map[string]string{"op": "createIntent"}})
	if err != nil {
		t.Fatalf("asserted binding must ground: %v", err)
	}
	if len(parsed.Steps) == 0 || parsed.Steps[0].Type != "INJECT_FAULT" {
		t.Fatalf("expected the declines expansion, got %+v", parsed.Steps)
	}
	if !info.UsesInferred || strings.Join(info.Asserted, ",") != "op" {
		t.Fatalf("the run must say it rests on extracted facts and which role was asserted: %+v", info)
	}
}

func TestBindOverrideIsCheckedAgainstTheRoleShape(t *testing.T) {
	_, err := Resolve(draftDef(t), "declines", Options{BindOverrides: map[string]string{"op": "getIntent"}})
	if err == nil || !strings.Contains(err.Error(), "getIntent") || !strings.Contains(err.Error(), "CREATE") {
		t.Fatalf("a wrong-shaped assertion must be refused naming the ref and the shape, got %v", err)
	}
	_, err = Resolve(draftDef(t), "declines", Options{BindOverrides: map[string]string{"op": "nope"}})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("an unknown ref must be refused by name, got %v", err)
	}
}

func TestListBindingsOffersInferredCandidatesOnDrafts(t *testing.T) {
	for _, b := range ListBindings(draftDef(t)) {
		if b.ID != "declines" {
			continue
		}
		if b.Applicable || len(b.Inferred) == 0 || b.Inferred[0].Roles[0].OperationID != "createIntent" {
			t.Fatalf("declines must list the extracted candidate to assert: %+v", b)
		}
		return
	}
	t.Fatal("declines missing from the listing")
}
