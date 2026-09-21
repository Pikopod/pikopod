package importer

import (
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

func TestDeletedFieldsAbsentFromCanonicalJSON(t *testing.T) {
	def, err := NormalizeOpenAPI([]byte(emitOnlySpec))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := ir.Canonicalize(def)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"resources"`, `"apiStyle"`, `"errorCatalogue"`, `"relationships"`, `"stateTransitions"`, `"pagination"`, `"INFERRED"`} {
		if strings.Contains(canonical, key) {
			t.Fatalf("%s must be gone from the canonical IR", key)
		}
	}
	if !strings.Contains(canonical, `"examples"`) {
		t.Fatal("examples stays: it has readers")
	}
}
