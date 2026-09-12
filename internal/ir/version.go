package ir

// IRVersion tracks ApiDefinition's shape, NormalizerVersion the source→IR
// mapping. Both hash in, so changing one re-versions every document.
const (
	IRVersion         = "1.2.0"
	NormalizerVersion = "1.2.0"
)
