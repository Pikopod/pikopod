package scenario_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/sandbox"
	"github.com/pikopod/pikopod/internal/scenario"
	"github.com/pikopod/pikopod/internal/scenario/archetype"
)

func seedIR(t *testing.T, specFile string) *ir.ApiDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "parity", "importer", "specs", specFile))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	def, err := importer.NormalizeOpenAPI(raw)
	if err != nil {
		t.Fatalf("normalize spec: %v", err)
	}
	return def
}

func TestExtensionArchetypesRunGreenOnAnyProvider(t *testing.T) {
	providers := map[string]*ir.ApiDefinition{
		"appveyor": seedIR(t, "appveyor-swagger.json"),
		"paystack": seedIR(t, "paystack.json"),
	}
	if len(archetype.Extensions) < 4 {
		t.Fatalf("expected at least 4 extension archetypes, found %d", len(archetype.Extensions))
	}

	for i := range archetype.Extensions {
		if archetype.Find(archetype.Extensions[i].ID) != nil {
			t.Fatalf("extension id %q collides with the catalogue", archetype.Extensions[i].ID)
		}
	}

	for provName, apiDef := range providers {
		for i := range archetype.Extensions {
			a := &archetype.Extensions[i]
			t.Run(provName+"/"+a.ID, func(t *testing.T) {
				binding := archetype.Bind(a, apiDef)
				if !binding.Applicable {
					t.Fatalf("must bind against %s: %s", provName, binding.Reason)
				}
				exp, err := archetype.Expand(a, binding.Candidates[0].Bindings, apiDef)
				if err != nil {
					t.Fatalf("expand: %v", err)
				}
				def, errs := scenario.ParseDefinition(exp.Definition)
				if len(errs) > 0 {
					t.Fatalf("expanded definition does not parse: %+v", errs)
				}
				vr, _ := scenario.ValidateScenario(exp.Definition, apiDef)
				if !vr.Valid {
					t.Fatalf("expanded definition does not ground against %s: %+v", provName, vr.Errors)
				}

				run := func() *scenario.RunResult {
					store, err := sandbox.OpenMemoryStore()
					if err != nil {
						t.Fatalf("open store: %v", err)
					}
					defer store.Close()
					eng, err := sandbox.NewEngine(apiDef, sandbox.Config{ID: "sbx_seed", Seed: "seed-pack"}, store)
					if err != nil {
						t.Fatalf("new engine: %v", err)
					}
					res, err := scenario.Run(eng, def, nil, "seed-pack")
					if err != nil {
						t.Fatalf("run: %v", err)
					}
					return res
				}
				a1, a2 := run(), run()
				if a1.Status != scenario.RunPassed {
					steps, _ := json.MarshalIndent(a1.Steps, "", " ")
					t.Fatalf("must PASS on %s: %s (%s)\nsteps: %s", provName, a1.Status, a1.Summary, steps)
				}
				if a1.ResultHash != a2.ResultHash {
					t.Fatalf("run is not deterministic: %s vs %s", a1.ResultHash, a2.ResultHash)
				}
			})
		}
	}
}

func TestPackSchemaIsValidJSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schema", "scenario-pack.schema.json"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if v["title"] != "ScenarioPack" {
		t.Fatalf("unexpected schema title: %v", v["title"])
	}
}
