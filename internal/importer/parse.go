package importer

import "strings"

type documentFormat int

const (
	formatAuto documentFormat = iota
	formatJSON
	formatYAML
)

// parseStructured parses under the hardening limits; auto mode routes a leading
// `{`/`[` to the JSON parser for duplicate-key rejection and safe depth caps.
func parseStructured(text string, format documentFormat, limits ParseLimits) (any, error) {
	effective := format
	if format == formatAuto {
		trimmed := strings.TrimLeft(text, " \t\r\n\v\f")
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			effective = formatJSON
		} else {
			effective = formatYAML
		}
	}
	if effective == formatJSON {
		return parseJSONSafely(text, limits)
	}
	return parseYAMLSafely(text, limits)
}
