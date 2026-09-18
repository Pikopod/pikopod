package resolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
)

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

func TestResolveArchetypeNeedsNoConfig(t *testing.T) {
	def := loadIR(t, "stripe.trimmed.json")
	parsed, err := Resolve(def, "declines", Options{})
	if err != nil {
		t.Fatalf("declines must resolve against stripe: %v", err)
	}
	if len(parsed.Steps) == 0 {
		t.Fatal("resolved definition has no steps")
	}
}

func TestResolveRefusesUnbindableArchetypeWithReason(t *testing.T) {
	def := loadIR(t, "stripe.trimmed.json")
	_, err := Resolve(def, "invalid_request", Options{})
	if err == nil {
		t.Fatal("invalid_request cannot bind against stripe and must refuse")
	}
	msg := err.Error()
	for _, want := range []string{"archetype does not apply", "invalid_request", "hasErrorResponseClass"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal must name %q, got: %s", want, msg)
		}
	}
}

func TestResolveUnknownNameIsNeitherArchetypeNorPack(t *testing.T) {
	def := loadIR(t, "stripe.trimmed.json")
	_, err := Resolve(def, "not_a_thing", Options{PackDirs: []string{t.TempDir()}})
	if err == nil || !strings.Contains(err.Error(), "neither an archetype nor a saved pack") {
		t.Fatalf("unknown name must say so, got: %v", err)
	}
}

func TestListBindingsCarriesReasonsAndRoles(t *testing.T) {
	def := loadIR(t, "stripe.trimmed.json")
	bindings := ListBindings(def)
	if len(bindings) == 0 {
		t.Fatal("no bindings returned")
	}
	var applicable, refused int
	for _, b := range bindings {
		if b.Applicable {
			applicable++
			if len(b.Candidates) == 0 {
				t.Fatalf("%s is applicable with no candidates", b.ID)
			}
			for _, c := range b.Candidates {
				for _, r := range c.Roles {
					if r.Role == "" || r.OperationID == "" {
						t.Fatalf("%s has an empty role binding: %+v", b.ID, r)
					}
				}
			}
			continue
		}
		refused++
		if b.Reason == "" {
			t.Fatalf("%s refused with no reason", b.ID)
		}
	}
	if applicable == 0 || refused == 0 {
		t.Fatalf("stripe should both bind and refuse; got %d applicable, %d refused", applicable, refused)
	}
}

func TestPackByNameMissesCleanly(t *testing.T) {
	if p := PackByName("nothing-here", []string{t.TempDir()}); p != nil {
		t.Fatalf("expected no pack, got %+v", p)
	}
}
