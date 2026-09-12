package ir

// Provenance is the trust label on every non-trivial IR field; the levels are
// never flattened, so inferred behaviour never passes as documented.
const (
	ProvenanceExplicit     = "EXPLICIT"      // stated verbatim in the source document
	ProvenanceDerived      = "DERIVED"       // deterministic transformation of explicit data
	ProvenanceInferred     = "INFERRED"      // heuristic with a stated rule + confidence
	ProvenanceLLMExtracted = "LLM_EXTRACTED" // model output; capped downstream
)

// Prov wraps a value with its provenance, as {value, provenance, confidence,
// evidence}.
type Prov[T any] struct {
	Value      T      `json:"value"`
	Provenance string `json:"provenance"`
	// Confidence is 0..1. EXPLICIT/DERIVED are 1; INFERRED/LLM_EXTRACTED carry
	// the real score.
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

// Inferred wraps a heuristic result with its confidence score.
func Inferred[T any](value T, confidence float64, evidence string) Prov[T] {
	return Prov[T]{Value: value, Provenance: ProvenanceInferred, Confidence: confidence, Evidence: evidence}
}

// IsUncertain reports heuristic or model-output trust; the differ caps any
// change touching such a field at POTENTIALLY_BREAKING.
func (p Prov[T]) IsUncertain() bool {
	return p.Provenance == ProvenanceInferred || p.Provenance == ProvenanceLLMExtracted
}

// IsGuess reports per-field HEURISTIC trust only. Sandbox enforcement gates
// use this, not IsUncertain: Tier-C LLM extraction must still be simulated.
func (p Prov[T]) IsGuess() bool {
	return p.Provenance == ProvenanceInferred
}
