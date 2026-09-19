package importer

import (
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

const exampleSpec = `{"openapi":"3.1.0","info":{"title":"W","version":"1"},"paths":{
"/widgets":{"get":{"responses":{"200":{"description":"ok","content":{"application/json":{
  "schema":{"type":"object"},"example":{"items":[{"id":"w_1"}],"total":1}}}}}}}}}`

const namedExamplesSpec = `{"openapi":"3.1.0","info":{"title":"W","version":"1"},"paths":{
"/widgets":{"post":{
  "requestBody":{"content":{"application/json":{"schema":{"type":"object"},"examples":{"minimal":{"value":{"name":"a"}},"full":{"value":{"name":"b","color":"red"}}}}}},
  "responses":{"201":{"description":"c","content":{"application/json":{"schema":{"type":"object"},"examples":{"ok":{"value":{"id":"w_1"}},"empty":{"value":{}}}}}}}}}}}`

const noExampleSpec = `{"openapi":"3.1.0","info":{"title":"W","version":"1"},"paths":{
"/widgets":{"get":{"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`

func TestMediaTypeExampleImported(t *testing.T) {
	def, err := NormalizeOpenAPI([]byte(exampleSpec))
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Examples) != 1 {
		t.Fatalf("want one example, got %d", len(def.Examples))
	}
	ex := def.Examples[0]
	if want := ir.ResponseID(ir.EndpointID("GET", "/widgets"), "200"); ex.ForNodeID != want {
		t.Fatalf("forNodeId %q, want %q", ex.ForNodeID, want)
	}
	if ex.MediaType == nil || *ex.MediaType != "application/json" {
		t.Fatalf("mediaType %v", ex.MediaType)
	}
	val, _ := ex.Value.Value.(map[string]any)
	if val == nil || val["total"] != 1.0 {
		t.Fatalf("value did not round-trip: %#v", ex.Value.Value)
	}
	if ex.Value.Provenance != ir.ProvenanceExplicit || !strings.HasSuffix(ex.SourcePointer, "/content/application~1json/example") {
		t.Fatalf("provenance/pointer: %s %s", ex.Value.Provenance, ex.SourcePointer)
	}
}

func TestNamedExamplesImported(t *testing.T) {
	def, err := NormalizeOpenAPI([]byte(namedExamplesSpec))
	if err != nil {
		t.Fatal(err)
	}
	if len(def.Examples) != 4 {
		t.Fatalf("want four examples (two per body), got %d", len(def.Examples))
	}
	ids := map[string]bool{}
	for _, ex := range def.Examples {
		if ids[ex.ID] {
			t.Fatalf("duplicate example id %s", ex.ID)
		}
		ids[ex.ID] = true
	}
}

func TestExamplesAbsentStaysEmpty(t *testing.T) {
	def, err := NormalizeOpenAPI([]byte(noExampleSpec))
	if err != nil {
		t.Fatal(err)
	}
	if def.Examples == nil || len(def.Examples) != 0 {
		t.Fatalf("want a non-nil empty slice, got %#v", def.Examples)
	}
}

func TestExampleIDIsOrderIndependent(t *testing.T) {
	reordered := strings.Replace(namedExamplesSpec, `"ok":{"value":{"id":"w_1"}},"empty":{"value":{}}`, `"empty":{"value":{}},"ok":{"value":{"id":"w_1"}}`, 1)
	if reordered == namedExamplesSpec {
		t.Fatal("fixture edit did not apply")
	}
	a, err := NormalizeOpenAPI([]byte(namedExamplesSpec))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizeOpenAPI([]byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	ha, _ := ir.NormalizedHash(a)
	hb, _ := ir.NormalizedHash(b)
	if ha != hb {
		t.Fatalf("example order changed the hash: %s vs %s", ha, hb)
	}
	for i := range a.Examples {
		if a.Examples[i].ID != b.Examples[i].ID {
			t.Fatalf("ids depend on source order: %s vs %s", a.Examples[i].ID, b.Examples[i].ID)
		}
	}
}

func TestExampleRefResolvedLocallyOnly(t *testing.T) {
	remote := strings.Replace(namedExamplesSpec, `"ok":{"value":{"id":"w_1"}}`, `"ok":{"$ref":"https://example.invalid/ex.json"}`, 1)
	_, err := NormalizeOpenAPI([]byte(remote))
	if err == nil || !strings.Contains(err.Error(), "SPEC_REF_UNRESOLVABLE") {
		t.Fatalf("a remote example $ref must be refused as unresolvable, got %v", err)
	}
	local := strings.Replace(namedExamplesSpec, `"ok":{"value":{"id":"w_1"}}`, `"ok":{"$ref":"#/components/examples/ok"}`, 1)
	local = strings.Replace(local, `"paths":{`, `"components":{"examples":{"ok":{"value":{"id":"w_ref"}}}},"paths":{`, 1)
	def, err := NormalizeOpenAPI([]byte(local))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ex := range def.Examples {
		if m, _ := ex.Value.Value.(map[string]any); m != nil && m["id"] == "w_ref" {
			found = true
		}
	}
	if !found {
		t.Fatal("a local example $ref must resolve to its value")
	}
}
