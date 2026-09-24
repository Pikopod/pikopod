package scenario

import (
	"fmt"
	"sort"
	"strings"
)

var knownCaptureSources = []string{"response.body", "response.headers", "webhook.delivery", "state.resource"}

type parsedCapture struct {
	source string
	path   string
}

func parseCaptureExpr(expr string) (parsedCapture, error) {
	dollar := strings.Index(expr, "$")
	if dollar == -1 {
		return parsedCapture{}, fmt.Errorf("capture '%s' must contain a '$' separating source from JSONPath", expr)
	}
	source := expr[:dollar]
	path := expr[dollar:]
	for _, s := range knownCaptureSources {
		if s == source {
			return parsedCapture{source: source, path: path}, nil
		}
	}
	return parsedCapture{}, fmt.Errorf("capture source '%s' is not one of %s", source, strings.Join(knownCaptureSources, ", "))
}

type captureDocuments map[string]any

func applyCaptures(captureMap map[string]string, docs captureDocuments) (map[string]any, error) {
	out := map[string]any{}
	names := make([]string, 0, len(captureMap))
	for name := range captureMap {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		expr := captureMap[name]
		pc, err := parseCaptureExpr(expr)
		if err != nil {
			return nil, err
		}
		doc, has := docs[pc.source]
		if !has {
			return nil, fmt.Errorf("capture '%s' reads '%s', which this step did not produce", name, pc.source)
		}
		found, value, err := getByPath(doc, pc.path)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("capture '%s' path '%s' did not resolve against %s", name, pc.path, pc.source)
		}
		out[name] = value
	}
	return out, nil
}
