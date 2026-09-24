package importer

import (
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

const llmSpecFixture = `{
  "openapi": "3.0.0",
  "info": {"title": "extracted", "version": "1"},
  "security": [{"bearerAuth": []}],
  "paths": {"/things": {
    "get": {"responses": {"200": {"description": "ok"}}},
    "post": {
      "requestBody": {"content": {"application/json": {"schema": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}}}}}},
      "responses": {"201": {"description": "created"}}
    }
  }},
  "components": {"securitySchemes": {"bearerAuth": {"type": "http", "scheme": "bearer"}}}
}`

func TestLLMExtractedProvenance(t *testing.T) {
	def, err := NormalizeLLMExtracted([]byte(llmSpecFixture))
	if err != nil {
		t.Fatal(err)
	}
	if def.Status != "DRAFT" {
		t.Fatalf("Tier-C contracts land DRAFT (ir.go contract), got %q", def.Status)
	}

	checkedProvs := 0
	for _, e := range def.Endpoints {
		for _, p := range []ir.Prov[string]{e.Method, e.PathTemplate} {
			checkedProvs++
			if p.Provenance != ir.ProvenanceLLMExtracted {
				t.Fatalf("field claims %s — a model-written contract must carry LLM_EXTRACTED", p.Provenance)
			}
			if p.Confidence >= 1 {
				t.Fatalf("LLM_EXTRACTED confidence must be capped below 1, got %v", p.Confidence)
			}
			if p.Evidence == "" {
				t.Fatal("evidence (source pointer) must survive the downgrade")
			}
		}
	}
	if checkedProvs == 0 {
		t.Fatal("no provenance fields checked")
	}
	scheme := def.AuthSchemes[0].Kind
	if !scheme.IsUncertain() {
		t.Fatal("LLM_EXTRACTED must read as uncertain for diff/bind surfacing")
	}

	if got := def.NormalizerVersion; got != ir.NormalizerVersion+"+llm-extracted" {
		t.Fatalf("normalizer version must mark the origin, got %q", got)
	}

	plain, err := NormalizeOpenAPI([]byte(llmSpecFixture))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Status != "ACTIVE" || plain.Endpoints[0].Method.Provenance == ir.ProvenanceLLMExtracted {
		t.Fatal("the ordinary import path must be unaffected")
	}
}
