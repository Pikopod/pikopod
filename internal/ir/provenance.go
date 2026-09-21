package ir

const (
	ProvenanceExplicit     = "EXPLICIT"
	ProvenanceDerived      = "DERIVED"
	ProvenanceLLMExtracted = "LLM_EXTRACTED"
)

// Prov wraps a value with its provenance, as {value, provenance, confidence,
// evidence}.
type Prov[T any] struct {
	Value      T       `json:"value"`
	Provenance string  `json:"provenance"`
	Confidence float64 `json:"confidence"`
	// Evidence is a JSON pointer into the source document, or an extraction
	// note. Debug-only: excluded from the normalized hash.
	Evidence string `json:"evidence"`
}

// Explicit wraps a value stated verbatim in the source document.
func Explicit[T any](value T, evidence string) Prov[T] {
	return Prov[T]{Value: value, Provenance: ProvenanceExplicit, Confidence: 1, Evidence: evidence}
}

// Derived wraps a deterministic transformation of explicit data.
func Derived[T any](value T, evidence string) Prov[T] {
	return Prov[T]{Value: value, Provenance: ProvenanceDerived, Confidence: 1, Evidence: evidence}
}

func (p Prov[T]) IsUncertain() bool {
	return p.Provenance == ProvenanceLLMExtracted
}
