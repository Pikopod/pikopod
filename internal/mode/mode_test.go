package mode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
	"github.com/pikopod/pikopod/internal/scenario/resolve"
)

func loadIR(t *testing.T, specFile string) *ir.ApiDefinition {
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

func compileArchetype(t *testing.T, def *ir.ApiDefinition, name string) (*Spec, error) {
	t.Helper()
	parsed, err := resolve.Resolve(def, name, resolve.Options{})
	if err != nil {
		t.Skipf("%s does not bind against this spec: %v", name, err)
	}
	return Compile(name, "archetype "+name, parsed)
}

// The failure-injection archetypes become modes; the ones that assert correct
// behaviour do not, because their first step is driving.
func TestCompileSplitsFailureArchetypesFromAssertions(t *testing.T) {
	def := loadIR(t, "stripe.trimmed.json")
	modeable := []string{"declines", "retry_storm", "timeouts", "rate_limit_backoff", "downtime_recovery"}
	assertions := []string{"happy_path", "unauthorized"}

	for _, name := range modeable {
		spec, err := compileArchetype(t, def, name)
		if err != nil {
			t.Errorf("%s should compile to a mode: %v", name, err)
			continue
		}
		if len(spec.Faults) == 0 {
			t.Errorf("%s compiled with nothing armed", name)
		}
	}
	for _, name := range assertions {
		if _, err := compileArchetype(t, def, name); err == nil {
			t.Errorf("%s asserts behaviour and must not compile to a mode", name)
		} else if !strings.Contains(err.Error(), "no standing state") {
			t.Errorf("%s refusal should name the reason, got: %v", name, err)
		}
	}
}

// declines ends with CLEAR_FAULT. Folding every conditioning step would arm a
// fault and then cancel it, leaving a mode that does nothing.
func TestCompileFoldsThePrefixNotTheWholeDefinition(t *testing.T) {
	def := loadIR(t, "stripe.trimmed.json")
	spec, err := compileArchetype(t, def, "declines")
	if err != nil {
		t.Fatalf("declines: %v", err)
	}
	if len(spec.Faults) != 1 {
		t.Fatalf("declines must arm exactly one rule, got %d", len(spec.Faults))
	}
	if spec.Faults[0].Kind != "error" || spec.Faults[0].Status != 400 {
		t.Fatalf("declines armed %+v, want a 400 error", spec.Faults[0])
	}
}

// retry_storm carries its sequence in the rule (times/per), not in the steps,
// so it projects onto a mode faithfully.
func TestCompileKeepsTimesWindowFromTheRule(t *testing.T) {
	def := loadIR(t, "stripe.trimmed.json")
	spec, err := compileArchetype(t, def, "retry_storm")
	if err != nil {
		t.Fatalf("retry_storm: %v", err)
	}
	f := spec.Faults[0]
	if f.Times != 2 || f.Per != "idempotency-key" {
		t.Fatalf("retry_storm armed %+v, want times=2 per=idempotency-key", f)
	}
}

func TestDescribeNamesWhatIsArmed(t *testing.T) {
	def := loadIR(t, "stripe.trimmed.json")
	spec, err := compileArchetype(t, def, "declines")
	if err != nil {
		t.Fatal(err)
	}
	out := spec.Describe()
	for _, want := range []string{"mode: declines", "armed", "error 400"} {
		if !strings.Contains(out, want) {
			t.Errorf("Describe missing %q:\n%s", want, out)
		}
	}
}
