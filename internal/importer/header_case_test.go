package importer

import (
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/specdiff"
)

func specWithParams(params string) []byte {
	return []byte(`{"openapi":"3.1.0","info":{"title":"T","version":"1"},"paths":{"/charges/{chargeId}":{"get":{
	  "parameters":[` + params + `],
	  "responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`)
}

func paramNamed(t *testing.T, def *ir.ApiDefinition, name string) *ir.Parameter {
	t.Helper()
	for i := range def.Endpoints[0].Parameters {
		if def.Endpoints[0].Parameters[i].Name == name {
			return &def.Endpoints[0].Parameters[i]
		}
	}
	return nil
}

func TestHeaderParamNameLowercasedInIR(t *testing.T) {
	def, err := NormalizeOpenAPI(specWithParams(`{"name":"X-Request-Id","in":"header","schema":{"type":"string"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if paramNamed(t, def, "x-request-id") == nil {
		var names []string
		for _, p := range def.Endpoints[0].Parameters {
			names = append(names, p.Name)
		}
		t.Fatalf("header stored as %v, want x-request-id", names)
	}
}

// Header names are case-insensitive, so a case change in the spec is not a
// change in the API and must produce no findings at all.
func TestHeaderCaseChangeProducesNoFindings(t *testing.T) {
	oldDef, err := NormalizeOpenAPI(specWithParams(`{"name":"X-Request-Id","in":"header","schema":{"type":"string"}}`))
	if err != nil {
		t.Fatal(err)
	}
	newDef, err := NormalizeOpenAPI(specWithParams(`{"name":"x-request-id","in":"header","schema":{"type":"string"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if fs := specdiff.Diff(oldDef, newDef); len(fs) != 0 {
		var ids []string
		for _, f := range fs {
			ids = append(ids, f.ID)
		}
		t.Fatalf("a header case change produced %d finding(s): %s", len(fs), strings.Join(ids, ", "))
	}
}

func TestMediaTypeKeyLowercased(t *testing.T) {
	raw := []byte(`{"openapi":"3.1.0","info":{"title":"T","version":"1"},"paths":{"/x":{"get":{
	  "responses":{"200":{"description":"ok","content":{"Application/JSON":{"schema":{"type":"object"}}}}}}}}}`)
	def, err := NormalizeOpenAPI(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := def.Endpoints[0].Responses[0].Content[0].MediaType
	if got != "application/json" {
		t.Fatalf("media type stored as %q, want application/json", got)
	}
}

// Path and query names are case-sensitive and must keep their exact spelling.
func TestPathAndQueryParamCasePreserved(t *testing.T) {
	def, err := NormalizeOpenAPI(specWithParams(
		`{"name":"chargeId","in":"path","required":true,"schema":{"type":"string"}},
		 {"name":"pageSize","in":"query","schema":{"type":"integer"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"chargeId", "pageSize"} {
		if paramNamed(t, def, want) == nil {
			t.Errorf("%q lost its case", want)
		}
	}
}
