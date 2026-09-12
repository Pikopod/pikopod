package importer

// ParseLimits are resource bounds for untrusted-document handling: admit large
// real specs, reject bombs. A limit change is a normalization-contract change.
type ParseLimits struct {
	// MaxDepth is the max container nesting depth before SPEC_DEPTH_EXCEEDED.
	MaxDepth int
	// MaxNodes is the max total parsed values before SPEC_TOO_LARGE.
	MaxNodes int
	// MaxDocumentBytes is the max raw document size; checked before parsing.
	MaxDocumentBytes int
	// MaxAliasExpansions bounds YAML alias resolution (billion-laughs).
	MaxAliasExpansions int
	// MaxRefResolutions bounds $ref resolutions across a single normalization.
	MaxRefResolutions int
	// MaxSchemaDepth bounds the schema-tree depth walked during normalization.
	MaxSchemaDepth int
}

var DefaultParseLimits = ParseLimits{
	MaxDepth:           200,
	MaxNodes:           3_000_000,
	MaxDocumentBytes:   20 * 1024 * 1024,
	MaxAliasExpansions: 100,
	MaxRefResolutions:  100_000,
	MaxSchemaDepth:     100,
}
