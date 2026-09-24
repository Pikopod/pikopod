package importer

import "regexp"

type SpecErrorCode string

const (
	SpecParseError         SpecErrorCode = "SPEC_PARSE_ERROR"
	SpecTooLarge           SpecErrorCode = "SPEC_TOO_LARGE"
	SpecDepthExceeded      SpecErrorCode = "SPEC_DEPTH_EXCEEDED"
	SpecRefUnresolvable    SpecErrorCode = "SPEC_REF_UNRESOLVABLE"
	SpecUnsupportedVersion SpecErrorCode = "SPEC_UNSUPPORTED_VERSION"
	SpecConversionFailed   SpecErrorCode = "SPEC_CONVERSION_FAILED"
)

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

var detectLimits = DefaultParseLimits

var yamlSpecMarker = regexp.MustCompile(`(?m)^\s*(openapi|swagger)\s*:`)
var graphqlMarker = regexp.MustCompile(`\b(type|interface|input|enum|union|scalar|schema)\b\s+\w`)

func isJSWhitespace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f', 0x00A0, 0xFEFF, 0x2028, 0x2029, 0x0085:
		return true
	}
	return r >= 0x2000 && r <= 0x200A || r == 0x1680 || r == 0x202F || r == 0x205F || r == 0x3000
}

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
