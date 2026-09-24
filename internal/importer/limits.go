package importer

type ParseLimits struct {
	MaxDepth int

	MaxNodes int

	MaxDocumentBytes int

	MaxAliasExpansions int

	MaxRefResolutions int

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
