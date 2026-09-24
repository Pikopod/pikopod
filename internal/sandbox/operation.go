package sandbox

import (
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

type operationKind string

const (
	opCreate      operationKind = "create"
	opRead        operationKind = "read"
	opList        operationKind = "list"
	opReplace     operationKind = "replace"
	opMerge       operationKind = "merge"
	opDelete      operationKind = "delete"
	opPassthrough operationKind = "passthrough"
)

type operation struct {
	kind operationKind

	typ string

	key *string
}

func concretize(templateSegments []string, pathParams map[string]string) string {
	parts := make([]string, len(templateSegments))
	for i, seg := range templateSegments {
		if m := paramSegment.FindStringSubmatch(seg); m != nil {
			if v, ok := pathParams[m[1]]; ok {
				parts[i] = v
			} else {
				parts[i] = seg
			}
		} else {
			parts[i] = seg
		}
	}
	return "/" + strings.Join(parts, "/")
}

func deriveOperation(endpoint *ir.Endpoint, pathParams map[string]string) operation {
	method := strings.ToUpper(endpoint.Method.Value)
	segs := pathSegments(endpoint.PathTemplate.Value)
	isItem := false
	var last string
	if len(segs) > 0 {
		last = segs[len(segs)-1]
		isItem = paramSegment.MatchString(last)
	}

	if isItem {
		paramName := paramSegment.FindStringSubmatch(last)[1]
		typ := concretize(segs[:len(segs)-1], pathParams)
		key, ok := pathParams[paramName]
		if !ok {
			return operation{kind: opPassthrough, typ: typ, key: nil}
		}
		switch method {
		case "GET":
			return operation{kind: opRead, typ: typ, key: &key}
		case "PUT":
			return operation{kind: opReplace, typ: typ, key: &key}
		case "PATCH":
			return operation{kind: opMerge, typ: typ, key: &key}
		case "DELETE":
			return operation{kind: opDelete, typ: typ, key: &key}
		default:
			return operation{kind: opPassthrough, typ: typ, key: &key}
		}
	}

	typ := concretize(segs, pathParams)
	switch method {
	case "GET":
		return operation{kind: opList, typ: typ, key: nil}
	case "POST":
		return operation{kind: opCreate, typ: typ, key: nil}
	default:
		return operation{kind: opPassthrough, typ: typ, key: nil}
	}
}
