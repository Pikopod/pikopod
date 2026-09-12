package importer

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// refResolver dereferences LOCAL non-schema $refs only — remote refs are never
// fetched, and a budget plus cycle detection bounds recursive expansion.
type refResolver struct {
	document    *OrdMap
	limits      ParseLimits
	resolutions int
}

func newRefResolver(document *OrdMap, limits ParseLimits) *refResolver {
	return &refResolver{document: document, limits: limits}
}

func (r *refResolver) isRef(node any) (string, bool) {
	m, ok := node.(*OrdMap)
	if !ok {
		return "", false
	}
	ref, ok := m.Get("$ref")
	if !ok {
		return "", false
	}
	s, ok := ref.(string)
	return s, ok
}

// resolve follows a possibly-chained local $ref to its concrete target.
func (r *refResolver) resolve(node any) (any, error) {
	return r.resolveSeen(node, map[string]bool{})
}

func (r *refResolver) resolveSeen(node any, seen map[string]bool) (any, error) {
	pointer, ok := r.isRef(node)
	if !ok {
		return node, nil
	}
	if !strings.HasPrefix(pointer, "#/") {
		return nil, &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("remote or external $ref is not permitted: %q", pointer), Pointer: pointer}
	}
	if seen[pointer] {
		return nil, &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("cyclic $ref detected at %q", pointer), Pointer: pointer}
	}
	r.resolutions++
	if r.resolutions > r.limits.MaxRefResolutions {
		return nil, specErr(SpecRefUnresolvable, "ref resolution budget exceeded")
	}
	seen[pointer] = true
	target, found := r.getByPointer(pointer)
	if !found {
		return nil, &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("$ref target not found: %q", pointer), Pointer: pointer}
	}
	return r.resolveSeen(target, seen)
}

// getByPointer resolves a JSON pointer like #/components/parameters/Foo
// against the document. The second result is false when the target is absent.
func (r *refResolver) getByPointer(pointer string) (any, bool) {
	parts := strings.Split(pointer[2:], "/")
	var current any = r.document
	for _, raw := range parts {
		part := jpunescape(raw)
		m, ok := current.(*OrdMap)
		if !ok {
			// Mirror JS indexing semantics: arrays are objects whose index
			// properties are their elements.
			if arr, isArr := current.([]any); isArr {
				idx, ok := arrayIndex(part, len(arr))
				if !ok {
					return nil, false
				}
				current = arr[idx]
				continue
			}
			return nil, false
		}
		v, ok := m.Get(part)
		if !ok {
			return nil, false
		}
		current = v
	}
	return current, true
}

func arrayIndex(part string, length int) (int, bool) {
	n := 0
	if part == "" {
		return 0, false
	}
	for i := 0; i < len(part); i++ {
		if part[i] < '0' || part[i] > '9' {
			return 0, false
		}
		n = n*10 + int(part[i]-'0')
		if n >= 1<<31 {
			return 0, false
		}
	}
	if len(part) > 1 && part[0] == '0' {
		return 0, false // non-canonical index reads undefined in JS
	}
	if n >= length {
		return 0, false
	}
	return n, true
}

var schemaRefRe = regexp.MustCompile(`^#/components/schemas/([^/]+)$`)

// schemaRefName returns the component name a local schema pointer targets,
// else "".
func (r *refResolver) schemaRefName(pointer string) string {
	m := schemaRefRe.FindStringSubmatch(pointer)
	if m == nil {
		return ""
	}
	name := jpunescape(m[1])
	if decoded, err := url.PathUnescape(name); err == nil {
		return decoded
	}
	return name
}
