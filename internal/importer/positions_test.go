package importer

import (
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

const positionedSpec = `openapi: 3.1.0
info:
  title: t
  version: "1"
paths:
  /widgets:
    get:
      responses:
        "200":
          description: ok
components:
  schemas:
    Widget:
      type: object
      properties:
        name:
          type: string
        color:
          type: string
`

func TestPointerToLine(t *testing.T) {
	_, pos, err := NormalizeOpenAPIFrom([]byte(positionedSpec), &Source{File: "openapi.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := pos.Lookup("#/components/schemas/Widget/properties/color")
	if !ok || got.Line != 18 || got.Col != 9 || got.File != "openapi.yaml" {
		t.Fatalf("color property position: %+v %v", got, ok)
	}
	if got, ok := pos.Lookup("#/paths/~1widgets/get"); !ok || got.Line != 7 {
		t.Fatalf("operation position: %+v %v", got, ok)
	}
	if got, ok := pos.Lookup("#/paths/~1widgets/get/responses/200/nope"); !ok || got.Line != 9 {
		t.Fatalf("an unknown leaf falls back to its nearest known parent: %+v %v", got, ok)
	}
	if _, ok := pos.Lookup("#/nowhere/at/all"); ok {
		t.Fatal("a pointer with no known ancestor has no position")
	}
}

func TestPositionsAbsentForJSONDoNotError(t *testing.T) {
	def, pos, err := NormalizeOpenAPIFrom([]byte(emitOnlySpec), &Source{File: "spec.json"})
	if err != nil || len(def.Endpoints) != 1 {
		t.Fatalf("json still normalizes: %v", err)
	}
	if got, ok := pos.Lookup("#/paths/~1x/get"); ok && got.Line == 0 {
		t.Fatalf("a position that exists must carry a real line: %+v", got)
	}
	def2, pos2, err := NormalizeOpenAPIFrom([]byte(`{"swagger":"2.0","info":{"title":"t","version":"1"},"paths":{"/x":{"get":{"responses":{"200":{"description":"ok"}}}}}}`), nil)
	if err != nil || len(def2.Endpoints) != 1 || len(pos2) != 0 {
		t.Fatalf("a converted document carries no positions rather than wrong ones: %v %d", err, len(pos2))
	}
}

func TestPositionIndexDoesNotChangeHash(t *testing.T) {
	def, err := NormalizeOpenAPI([]byte(emitOnlySpec))
	if err != nil {
		t.Fatal(err)
	}
	h, err := ir.NormalizedHash(def)
	if err != nil {
		t.Fatal(err)
	}
	if h != "61a3754e5028c42914351be407f800e2dc67e6df992d650d427827662c9c1b0e" {
		t.Fatalf("hash moved: %s", h)
	}
	def2, _, err := NormalizeOpenAPIFrom([]byte(emitOnlySpec), &Source{File: "spec.json"})
	if err != nil {
		t.Fatal(err)
	}
	if h2, _ := ir.NormalizedHash(def2); h2 != h {
		t.Fatalf("collecting positions changed the hash: %s", h2)
	}
}
