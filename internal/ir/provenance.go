package ir

const (
	ProvenanceExplicit     = "EXPLICIT"
	ProvenanceDerived      = "DERIVED"
	ProvenanceLLMExtracted = "LLM_EXTRACTED"
)

type Prov[T any] struct {
	Value      T       `json:"value"`
	Provenance string  `json:"provenance"`
	Confidence float64 `json:"confidence"`

	Evidence string `json:"evidence"`
}

func Explicit[T any](value T, evidence string) Prov[T] {
	return Prov[T]{Value: value, Provenance: ProvenanceExplicit, Confidence: 1, Evidence: evidence}
}

func Derived[T any](value T, evidence string) Prov[T] {
	return Prov[T]{Value: value, Provenance: ProvenanceDerived, Confidence: 1, Evidence: evidence}
}

func (p Prov[T]) IsUncertain() bool {
	return p.Provenance == ProvenanceLLMExtracted
}
