package archetype

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
)

type scenarioGolden struct {
	Spec       string           `json:"spec"`
	Bindings   map[string]any   `json:"bindings"`
	Expansions map[string][]any `json:"expansions"`
	Inventory  json.RawMessage  `json:"inventory"`
}

func loadGolden(t *testing.T, name string) *scenarioGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "parity", "scenario", name+".golden.json"))
	if err != nil {
		t.Fatalf("read golden — parity goldens are committed, not generated; restore it from git: %v", err)
	}
	var g scenarioGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	return &g
}

func loadIR(t *testing.T, specFile string) *ir.ApiDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "parity", "importer", "specs", specFile))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	def, err := importer.NormalizeOpenAPI(raw)
	if err != nil {
		t.Fatalf("normalize spec: %v", err)
	}
	return def
}

func jsonShape(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func parityOverSpec(t *testing.T, name, specFile string) {
	golden := loadGolden(t, name)
	apiDef := loadIR(t, specFile)

	if len(golden.Bindings) != len(Catalogue) {
		t.Fatalf("golden has %d archetypes, catalogue has %d — regenerate goldens", len(golden.Bindings), len(Catalogue))
	}

	for i := range Catalogue {
		a := &Catalogue[i]
		want, ok := golden.Bindings[a.ID]
		if !ok {
			t.Errorf("%s: archetype %s missing from golden", name, a.ID)
			continue
		}
		binding := Bind(a, apiDef)
		got := jsonShape(t, binding)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: binding diverges for %s:\n  got:  %s\n  want: %s", name, a.ID, mustJSON(got), mustJSON(want))
			continue
		}
		if !binding.Applicable {
			continue
		}
		wantExp := golden.Expansions[a.ID]
		n := len(binding.Candidates)
		if n > 3 {
			n = 3
		}
		if len(wantExp) != n {
			t.Errorf("%s: %s golden has %d expansions, expected %d", name, a.ID, len(wantExp), n)
			continue
		}
		for j := 0; j < n; j++ {
			exp, err := Expand(a, binding.Candidates[j].Bindings, apiDef)
			if err != nil {
				t.Errorf("%s: expand %s candidate %d: %v", name, a.ID, j, err)
				continue
			}
			gotExp := jsonShape(t, exp)
			if !reflect.DeepEqual(gotExp, wantExp[j]) {
				t.Errorf("%s: expansion diverges for %s candidate %d:\n  got:  %s\n  want: %s", name, a.ID, j, mustJSON(gotExp), mustJSON(wantExp[j]))
			}
		}
	}
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<unmarshalable: %v>", err)
	}
	return string(raw)
}

func TestParity_ScenarioExpand_AppVeyor(t *testing.T) {
	parityOverSpec(t, "appveyor", "appveyor-swagger.json")
}

func TestParity_ScenarioExpand_Paystack(t *testing.T) {
	parityOverSpec(t, "paystack", "paystack.json")
}
