package ir_test

import (
	"testing"

	"github.com/pikopod/pikopod/internal/importer"
	"github.com/pikopod/pikopod/internal/ir"
)

// The product property behind the normalized hash: a source document
// reformatted (whitespace, key order) yields an IDENTICAL hash — no phantom
// versions, no fabricated diffs — while a real change yields a new one.
func TestNormalizedHashIgnoresSourceFormatting(t *testing.T) {
	compact := `{"openapi":"3.0.0","info":{"title":"W","version":"1"},"paths":{"/widgets":{"get":{"responses":{"200":{"description":"ok"}}}}}}`
	reformatted := `{
	  "paths": { "/widgets": { "get": { "responses": { "200": { "description": "ok" } } } } },
	  "info":  { "version": "1", "title": "W" },
	  "openapi": "3.0.0"
	}`
	changed := `{"openapi":"3.0.0","info":{"title":"W","version":"1"},"paths":{"/widgets":{"get":{"responses":{"201":{"description":"ok"}}}}}}`

	hash := func(src string) string {
		t.Helper()
		def, err := importer.NormalizeOpenAPI([]byte(src))
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		h, err := ir.NormalizedHash(def)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		return h
	}
	if hash(compact) != hash(reformatted) {
		t.Fatal("reformatting the source must not change the normalized hash")
	}
	if hash(compact) == hash(changed) {
		t.Fatal("a real contract change must change the normalized hash")
	}
}

// Evidence pointers are debug-only: two IR fragments differing ONLY in
// evidence canonicalize identically (a re-import with different source
// pointers is the same contract).
func TestCanonicalStripsVolatileBackPointers(t *testing.T) {
	a := map[string]any{"value": "GET", "provenance": "EXPLICIT", "confidence": float64(1), "evidence": "#/paths/x"}
	b := map[string]any{"value": "GET", "provenance": "EXPLICIT", "confidence": float64(1), "evidence": "#/other/place", "sourcePointer": "line 9"}
	if ir.CanonicalStringify(a) != ir.CanonicalStringify(b) {
		t.Fatalf("evidence must not affect canonical form:\n%s\n%s", ir.CanonicalStringify(a), ir.CanonicalStringify(b))
	}
	// Order independence and JS-number quirks: arrays sort canonically, -0
	// normalizes to 0 (JSON.stringify parity — the goldens depend on it).
	x := ir.CanonicalStringify([]any{"b", "a", float64(0)})
	y := ir.CanonicalStringify([]any{"a", float64(-0.0), "b"})
	if x != y {
		t.Fatalf("array order and -0 must normalize: %s vs %s", x, y)
	}
}

func TestProvenanceTierGates(t *testing.T) {
	cases := []struct {
		p         ir.Prov[string]
		uncertain bool
	}{
		{ir.Explicit("x", "e"), false},
		{ir.Derived("x", "e"), false},
		{ir.Prov[string]{Value: "x", Provenance: ir.ProvenanceLLMExtracted, Confidence: 0.7}, true},
	}
	for _, c := range cases {
		if c.p.IsUncertain() != c.uncertain {
			t.Fatalf("%s: IsUncertain=%v, want %v", c.p.Provenance, c.p.IsUncertain(), c.uncertain)
		}
	}
}
