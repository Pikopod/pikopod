package importer

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

type refResolver struct {
	document    *OrdMap
	limits      ParseLimits
	resolutions int
	external    *externalDocs
}

func newRefResolver(document *OrdMap, limits ParseLimits) *refResolver {
	return &refResolver{document: document, limits: limits}
}

func newRefResolverFrom(document *OrdMap, limits ParseLimits, src *Source) (*refResolver, error) {
	r := &refResolver{document: document, limits: limits}
	if src == nil || src.Load == nil {
		return r, nil
	}
	r.external = &externalDocs{src: src, docs: map[string]*OrdMap{}}
	if err := r.external.rewriteRelative(document, src.Dir); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *refResolver) isExternal(pointer string) bool {
	return r.external != nil && !strings.HasPrefix(pointer, "#") && !strings.Contains(pointer, "://") && !strings.HasPrefix(pointer, "//")
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

func (r *refResolver) resolve(node any) (any, error) {
	return r.resolveSeen(node, map[string]bool{})
}

func (r *refResolver) resolveSeen(node any, seen map[string]bool) (any, error) {
	pointer, ok := r.isRef(node)
	if !ok {
		return node, nil
	}
	if !strings.HasPrefix(pointer, "#/") && !r.isExternal(pointer) {
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
	var target any
	var found bool
	if strings.HasPrefix(pointer, "#/") {
		target, found = r.getByPointer(pointer)
	} else {
		file, frag := splitExternal(pointer)
		doc, err := r.external.load(file, r.limits)
		if err != nil {
			return nil, err
		}
		if frag == "" {
			target, found = doc, true
		} else if strings.HasPrefix(frag, "#/") {
			target, found = (&refResolver{document: doc}).getByPointer(frag)
		}
	}
	if !found {
		return nil, &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("$ref target not found: %q", pointer), Pointer: pointer}
	}
	return r.resolveSeen(target, seen)
}

func (r *refResolver) getByPointer(pointer string) (any, bool) {
	parts := strings.Split(pointer[2:], "/")
	var current any = r.document
	for _, raw := range parts {
		part := jpunescape(raw)
		m, ok := current.(*OrdMap)
		if !ok {

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
		return 0, false
	}
	if n >= length {
		return 0, false
	}
	return n, true
}

var schemaRefRe = regexp.MustCompile(`^#/components/schemas/([^/]+)$`)

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
