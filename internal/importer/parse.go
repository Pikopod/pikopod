package importer

import "strings"

type documentFormat int

const (
	formatAuto documentFormat = iota
	formatJSON
	formatYAML
)

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

func parseStructuredWithPositions(text string, limits ParseLimits) (any, Positions, error) {
	trimmed := strings.TrimLeft(text, " \t\r\n\v\f")
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		doc, err := parseJSONSafely(text, limits)
		if err != nil {
			return nil, nil, err
		}
		_, pos, perr := parseYAMLWithPositions(text, limits, true)
		if perr != nil {
			pos = nil
		}
		return doc, pos, nil
	}
	return parseYAMLWithPositions(text, limits, true)
}
