package importer

import "testing"

func TestJsEqualNeverPanicsOnUncomparables(t *testing.T) {
	if jsEqual([]any{"a"}, []any{"a"}) {
		t.Fatal("non-scalars have no identity")
	}
	if jsEqual(map[string]any{}, map[string]any{}) {
		t.Fatal("maps have no identity")
	}
	if !jsEqual("id", "id") || jsEqual("id", "other") || !jsEqual(float64(1), float64(1)) {
		t.Fatal("scalar semantics broken")
	}
	if jsEqual([]any{}, "id") || jsEqual("id", []any{}) {
		t.Fatal("mixed scalar/non-scalar must be false")
	}
}

func TestSwagger2NonScalarParamIdentityDoesNotPanic(t *testing.T) {
	doc := `{
  "swagger": "2.0",
  "info": {"title": "T", "version": "1"},
  "paths": {
    "/w/{id}": {
      "parameters": [{"name": ["id"], "in": ["path"], "type": "string", "required": true}],
      "get": {
        "parameters": [{"name": ["id"], "in": ["path"], "type": "string", "required": true}],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`

	_, _ = NormalizeOpenAPI([]byte(doc))
}
