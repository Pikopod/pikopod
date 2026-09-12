// A deliberately small, bounded JSONPath getter — `$.a.b`, `$.a[0]`,
// `$['a']` and nothing that could hang a worker.
package scenario

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const maxJSONPathDepth = 32

type pathSegment struct {
	kind  string // "key" | "index"
	key   string
	index int
}

var jsonPathKeyChar = regexp.MustCompile(`^[A-Za-z0-9_\-$]$`)

func parseJSONPath(path string) ([]pathSegment, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed != "$" && !strings.HasPrefix(trimmed, "$.") && !strings.HasPrefix(trimmed, "$[") {
		return nil, fmt.Errorf("path must start with '$': %s", path)
	}
	var segments []pathSegment
	i := 1 // skip '$'
	for i < len(trimmed) {
		if len(segments) > maxJSONPathDepth {
			return nil, fmt.Errorf("path exceeds maximum depth")
		}
		ch := trimmed[i]
		switch {
		case ch == '.':
			i++
			key := ""
			for i < len(trimmed) && jsonPathKeyChar.MatchString(string(trimmed[i])) {
				key += string(trimmed[i])
				i++
			}
			if len(key) == 0 {
				return nil, fmt.Errorf("empty key in path: %s", path)
			}
			segments = append(segments, pathSegment{kind: "key", key: key})
		case ch == '[':
			close := strings.Index(trimmed[i:], "]")
			if close == -1 {
				return nil, fmt.Errorf("unclosed '[' in path: %s", path)
			}
			close += i
			inner := strings.TrimSpace(trimmed[i+1 : close])
			if ok, _ := regexp.MatchString(`^\d+$`, inner); ok {
				n, _ := strconv.Atoi(inner)
				segments = append(segments, pathSegment{kind: "index", index: n})
			} else if len(inner) >= 2 && ((inner[0] == '\'' && inner[len(inner)-1] == '\'' && !strings.Contains(inner[1:len(inner)-1], "'")) ||
				(inner[0] == '"' && inner[len(inner)-1] == '"' && !strings.Contains(inner[1:len(inner)-1], `"`))) {
				segments = append(segments, pathSegment{kind: "key", key: inner[1 : len(inner)-1]})
			} else {
				return nil, fmt.Errorf("unsupported bracket segment '%s' in path: %s", inner, path)
			}
			i = close + 1
		default:
			return nil, fmt.Errorf("unexpected character '%c' in path: %s", ch, path)
		}
	}
	return segments, nil
}

// getByPath resolves a path against a root document. found=false means the
// path is absent; a malformed path is an error.
func getByPath(root any, path string) (found bool, value any, err error) {
	segments, err := parseJSONPath(path)
	if err != nil {
		return false, nil, err
	}
	current := root
	for _, seg := range segments {
		if current == nil {
			return false, nil, nil
		}
		if seg.kind == "index" {
			arr, ok := current.([]any)
			if !ok || seg.index >= len(arr) {
				return false, nil, nil
			}
			current = arr[seg.index]
		} else {
			obj, ok := current.(map[string]any)
			if !ok {
				return false, nil, nil
			}
			v, has := obj[seg.key]
			if !has {
				return false, nil, nil
			}
			current = v
		}
	}
	return true, current, nil
}
