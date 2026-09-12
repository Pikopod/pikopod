package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

// Normalizer behaviour is pinned by goldens over real-world specs — never
// hand-written, never unit toys. The golden IR is compared structurally
// (JSON-normalized deep equality), and array order must match exactly: the
// goldens are the contract, sourcePointer/evidence included.

type importerGolden struct {
	Spec           string          `json:"spec"`
	Converted      *string         `json:"converted"`
	NormalizedHash string          `json:"normalizedHash"`
	IR             json.RawMessage `json:"ir"`
}

func loadImporterGolden(t *testing.T, name string) (importerGolden, []byte) {
	t.Helper()
	dir := filepath.Join("..", "..", "testdata", "parity", "importer")
	raw, err := os.ReadFile(filepath.Join(dir, name+".golden.json"))
	if err != nil {
		t.Fatalf("golden missing — parity goldens are committed, not generated; restore it from git: %v", err)
	}
	var g importerGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("golden unreadable: %v", err)
	}
	spec, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(g.Spec)))
	if err != nil {
		t.Fatalf("spec missing: %v", err)
	}
	return g, spec
}

func assertParity(t *testing.T, name string) {
	t.Helper()
	g, spec := loadImporterGolden(t, name)

	def, err := NormalizeOpenAPI(spec)
	if err != nil {
		t.Fatalf("NormalizeOpenAPI(%s): %v", g.Spec, err)
	}

	gotRaw, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal Go IR: %v", err)
	}
	var got, want any
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatalf("reparse Go IR: %v", err)
	}
	if err := json.Unmarshal(g.IR, &want); err != nil {
		t.Fatalf("reparse golden IR: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		diffs := diffJSON("ir", want, got, nil, 25)
		t.Fatalf("IR diverges from golden (%d+ paths):\n%s", len(diffs), joinLines(diffs))
	}

	hash, err := ir.NormalizedHash(def)
	if err != nil {
		t.Fatalf("NormalizedHash: %v", err)
	}
	if hash != g.NormalizedHash {
		t.Errorf("normalizedHash = %s, golden = %s", hash, g.NormalizedHash)
	}
}

func TestParity_ImporterPaystack(t *testing.T) { assertParity(t, "paystack") }
func TestParity_ImporterStripe(t *testing.T)   { assertParity(t, "stripe") }
func TestParity_ImporterAppveyor(t *testing.T) { assertParity(t, "appveyor") }

// A second Swagger 2.0 corpus entry exercising http-basic auth (AppVeyor
// covers apiKey). SYNTHETIC (tools/gen-synthetic-fixtures.py): shaped like a
// real SMS provider's spec without any provider's content.
func TestParity_ImporterSyntheticSMS(t *testing.T) { assertParity(t, "synthetic-sms") }

// TestParity_IRRoundTrip proves the IR structs round-trip exactly: golden
// JSON unmarshalled into ir.ApiDefinition and marshalled back must be
// structurally identical (field names, nulls, empty arrays).
func TestParity_IRRoundTrip(t *testing.T) {
	for _, name := range []string{"paystack", "stripe", "appveyor", "synthetic-sms"} {
		g, _ := loadImporterGolden(t, name)
		var def ir.ApiDefinition
		if err := json.Unmarshal(g.IR, &def); err != nil {
			t.Fatalf("%s: unmarshal golden IR into Go structs: %v", name, err)
		}
		out, err := json.Marshal(&def)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var got, want any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: reparse: %v", name, err)
		}
		if err := json.Unmarshal(g.IR, &want); err != nil {
			t.Fatalf("%s: reparse golden: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			diffs := diffJSON("ir", want, got, nil, 25)
			t.Fatalf("%s: IR round-trip not structurally identical:\n%s", name, joinLines(diffs))
		}

		// The round-tripped struct must also reproduce the canonical hash.
		hash, err := ir.NormalizedHash(&def)
		if err != nil {
			t.Fatalf("%s: NormalizedHash: %v", name, err)
		}
		if hash != g.NormalizedHash {
			t.Errorf("%s: round-trip normalizedHash = %s, golden = %s", name, hash, g.NormalizedHash)
		}
	}
}

func TestParity_Detect(t *testing.T) {
	cases := []struct {
		name string
		want Kind
	}{
		{"paystack", KindOpenAPI},
		{"stripe", KindOpenAPI},
		{"appveyor", KindOpenAPI},
		{"synthetic-sms", KindOpenAPI},
	}
	for _, c := range cases {
		_, spec := loadImporterGolden(t, c.name)
		kind, err := Detect(spec)
		if err != nil {
			t.Fatalf("Detect(%s): %v", c.name, err)
		}
		if kind != c.want {
			t.Errorf("Detect(%s) = %s, want %s", c.name, kind, c.want)
		}
	}
	// The YAML source of the committed paystack.json must classify identically.
	yamlRaw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "parity", "importer", "specs", "paystack.yaml"))
	if err != nil {
		t.Fatalf("paystack.yaml missing: %v", err)
	}
	if kind, err := Detect(yamlRaw); err != nil || kind != KindOpenAPI {
		t.Errorf("Detect(paystack.yaml) = %s, %v; want openapi", kind, err)
	}
}

// TestParity_YAMLSource feeds the original Paystack YAML (not the committed
// JSON conversion) through the Go YAML path: it must produce the exact same
// IR, proving the yaml.v3-based safe-yaml port agrees with the yaml npm
// package on a real spec.
func TestParity_YAMLSource(t *testing.T) {
	g, _ := loadImporterGolden(t, "paystack")
	yamlRaw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "parity", "importer", "specs", "paystack.yaml"))
	if err != nil {
		t.Fatalf("paystack.yaml missing: %v", err)
	}
	def, err := NormalizeOpenAPI(yamlRaw)
	if err != nil {
		t.Fatalf("NormalizeOpenAPI(paystack.yaml): %v", err)
	}
	hash, err := ir.NormalizedHash(def)
	if err != nil {
		t.Fatalf("NormalizedHash: %v", err)
	}
	if hash != g.NormalizedHash {
		t.Errorf("YAML-sourced normalizedHash = %s, JSON-sourced golden = %s", hash, g.NormalizedHash)
	}
}

// ------------------------------------------------------------------- diffing

func joinLines(lines []string) string {
	s := ""
	for _, l := range lines {
		s += l + "\n"
	}
	return s
}

// diffJSON walks two JSON-normalized values and reports the first divergent
// paths, so a golden mismatch points at the offending node instead of dumping
// megabytes.
func diffJSON(path string, want, got any, acc []string, limit int) []string {
	if len(acc) >= limit {
		return acc
	}
	switch w := want.(type) {
	case map[string]any:
		gm, ok := got.(map[string]any)
		if !ok {
			return append(acc, fmt.Sprintf("%s: want object, got %s", path, typeName(got)))
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, present := gm[k]
			if !present {
				acc = append(acc, fmt.Sprintf("%s.%s: missing in Go output (want %s)", path, k, preview(w[k])))
				continue
			}
			acc = diffJSON(path+"."+k, w[k], gv, acc, limit)
			if len(acc) >= limit {
				return acc
			}
		}
		for k := range gm {
			if _, present := w[k]; !present {
				acc = append(acc, fmt.Sprintf("%s.%s: extra in Go output (%s)", path, k, preview(gm[k])))
			}
		}
		return acc
	case []any:
		ga, ok := got.([]any)
		if !ok {
			return append(acc, fmt.Sprintf("%s: want array, got %s", path, typeName(got)))
		}
		if len(w) != len(ga) {
			acc = append(acc, fmt.Sprintf("%s: want len %d, got len %d", path, len(w), len(ga)))
		}
		n := len(w)
		if len(ga) < n {
			n = len(ga)
		}
		for i := 0; i < n; i++ {
			acc = diffJSON(fmt.Sprintf("%s[%d]", path, i), w[i], ga[i], acc, limit)
			if len(acc) >= limit {
				return acc
			}
		}
		return acc
	default:
		if !reflect.DeepEqual(want, got) {
			return append(acc, fmt.Sprintf("%s: want %s, got %s", path, preview(want), preview(got)))
		}
		return acc
	}
}

func typeName(v any) string {
	if v == nil {
		return "null"
	}
	return reflect.TypeOf(v).String()
}

func preview(v any) string {
	raw, _ := json.Marshal(v)
	s := string(raw)
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}
