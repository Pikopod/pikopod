package nl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/scenario/archetype"
)

func inventoryParity(t *testing.T, name, specFile string) {
	root := filepath.Join("..", "..", "..", "testdata", "parity")
	rawGolden, err := os.ReadFile(filepath.Join(root, "scenario", name+".golden.json"))
	if err != nil {
		t.Fatalf("read golden — parity goldens are committed, not generated; restore it from git: %v", err)
	}
	var golden struct {
		Inventory any `json:"inventory"`
	}
	if err := json.Unmarshal(rawGolden, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	rawSpec, err := os.ReadFile(filepath.Join(root, "importer", "specs", specFile))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	apiDef, err := importer.NormalizeOpenAPI(rawSpec)
	if err != nil {
		t.Fatalf("normalize spec: %v", err)
	}

	var applicable []archetype.Archetype
	for i := range archetype.Catalogue {
		if archetype.Bind(&archetype.Catalogue[i], apiDef).Applicable {
			applicable = append(applicable, archetype.Catalogue[i])
		}
	}
	inv := BuildInventory(apiDef, applicable)

	raw, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	var got any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal inventory: %v", err)
	}
	if !reflect.DeepEqual(got, golden.Inventory) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(golden.Inventory)
		t.Fatalf("%s inventory diverges:\n  got:  %s\n  want: %s", name, gotJSON, wantJSON)
	}
}

func TestParity_ScenarioInventory_AppVeyor(t *testing.T) {
	inventoryParity(t, "appveyor", "appveyor-swagger.json")
}

func TestParity_ScenarioInventory_Paystack(t *testing.T) {
	inventoryParity(t, "paystack", "paystack.json")
}
