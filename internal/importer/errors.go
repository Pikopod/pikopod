package importer

import "regexp"

// SpecErrorCode enumerates spec failures: each maps to a specific, actionable
// failure so a bad spec never surfaces as an opaque internal error.
type SpecErrorCode string

const (
	SpecParseError         SpecErrorCode = "SPEC_PARSE_ERROR"
	SpecTooLarge           SpecErrorCode = "SPEC_TOO_LARGE"
	SpecDepthExceeded      SpecErrorCode = "SPEC_DEPTH_EXCEEDED"
	SpecRefUnresolvable    SpecErrorCode = "SPEC_REF_UNRESOLVABLE"
	SpecUnsupportedVersion SpecErrorCode = "SPEC_UNSUPPORTED_VERSION"
	SpecConversionFailed   SpecErrorCode = "SPEC_CONVERSION_FAILED"
)

// SpecError is a typed parse/normalize failure. NormalizeOpenAPI translates
// it into the errfmt contract at the API boundary.
type SpecError struct {
	Code    SpecErrorCode
	Message string
	Pointer string
}

func (e *SpecError) Error() string {
	return string(e.Code) + ": " + e.Message
}

func specErr(code SpecErrorCode, message string) *SpecError {
	return &SpecError{Code: code, Message: message}
}

// detectLimits bounds Detect's structural JSON probe. Being stricter than a
// native parse can only move an eventual rejection earlier.
var detectLimits = DefaultParseLimits

var yamlSpecMarker = regexp.MustCompile(`(?m)^\s*(openapi|swagger)\s*:`)
var graphqlMarker = regexp.MustCompile(`\b(type|interface|input|enum|union|scalar|schema)\b\s+\w`)

// isJSWhitespace mirrors the character class ECMAScript trimStart strips:
// whitespace plus line terminators, NBSP, BOM and Unicode space separators.
func isJSWhitespace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f', 0x00A0, 0xFEFF, 0x2028, 0x2029, 0x0085:
		return true
	}
	return r >= 0x2000 && r <= 0x200A || r == 0x1680 || r == 0x202F || r == 0x205F || r == 0x3000
}

// looksLikeGraphQLSDL stands in for a real GraphQL parse: balanced braces and
// a type-system definition at brace depth zero.
func looksLikeGraphQLSDL(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	if depth != 0 {
		return false
	}
	return regexp.MustCompile(`(?m)^\s*(type|interface|input|enum|union|scalar|schema|extend|directive)\b`).MatchString(s)
}
