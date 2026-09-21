package importer

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/pikopod/pikopod/internal/ir"
)

type Source struct {
	File string
	Dir  string
	Load func(rootRelative string) ([]byte, error)
}

const (
	maxRefFiles = 64
	maxRefBytes = 32 << 20
)

type externalDocs struct {
	src   *Source
	docs  map[string]*OrdMap
	bytes int
}

func (e *externalDocs) resolvePath(fromDir, ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil || u.Scheme != "" || u.Host != "" || strings.HasPrefix(ref, "//") {
		return "", &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("remote or external $ref is not permitted: %q", ref), Pointer: ref}
	}
	if strings.HasPrefix(ref, "/") {
		return "", &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("absolute $ref path is not permitted: %q", ref), Pointer: ref}
	}
	joined := path.Clean(path.Join(fromDir, ref))
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return "", &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("$ref escapes the repository root: %q", ref), Pointer: ref}
	}
	return joined, nil
}

func (e *externalDocs) load(file string, limits ParseLimits) (*OrdMap, error) {
	if doc, ok := e.docs[file]; ok {
		return doc, nil
	}
	if len(e.docs) >= maxRefFiles {
		return nil, &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("$ref file budget of %d exceeded at %q", maxRefFiles, file), Pointer: file}
	}
	raw, err := e.src.Load(file)
	if err != nil {
		return nil, &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("$ref target file %q could not be read: %v", file, err), Pointer: file}
	}
	e.bytes += len(raw)
	if e.bytes > maxRefBytes {
		return nil, &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("$ref files exceed %d bytes in total at %q", maxRefBytes, file), Pointer: file}
	}
	parsed, err := parseStructured(string(raw), formatAuto, limits)
	if err != nil {
		return nil, &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("$ref target file %q does not parse: %v", file, err), Pointer: file}
	}
	doc, ok := parsed.(*OrdMap)
	if !ok {
		return nil, &SpecError{Code: SpecRefUnresolvable, Message: fmt.Sprintf("$ref target file %q is not an object", file), Pointer: file}
	}
	if err := e.rewriteRefs(doc, path.Dir(file), file); err != nil {
		return nil, err
	}
	e.docs[file] = doc
	return doc, nil
}

func (e *externalDocs) rewriteRelative(node any, dir string) error {
	switch n := node.(type) {
	case *OrdMap:
		if ref, ok := n.GetOr("$ref").(string); ok && !strings.HasPrefix(ref, "#") && !strings.Contains(ref, "://") && !strings.HasPrefix(ref, "//") && !strings.HasPrefix(ref, "/") {
			target, fragment, _ := strings.Cut(ref, "#")
			resolved, err := e.resolvePath(dir, target)
			if err != nil {
				return err
			}
			n.Set("$ref", resolved+"#"+fragment)
		}
		for _, k := range n.Keys() {
			if err := e.rewriteRelative(n.GetOr(k), dir); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range n {
			if err := e.rewriteRelative(v, dir); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *externalDocs) rewriteRefs(node any, dir, file string) error {
	switch n := node.(type) {
	case *OrdMap:
		if ref, ok := n.GetOr("$ref").(string); ok {
			target, fragment, _ := strings.Cut(ref, "#")
			if target == "" {
				n.Set("$ref", file+"#"+fragment)
			} else {
				resolved, err := e.resolvePath(dir, target)
				if err != nil {
					return err
				}
				n.Set("$ref", resolved+"#"+fragment)
			}
		}
		for _, k := range n.Keys() {
			if err := e.rewriteRefs(n.GetOr(k), dir, file); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range n {
			if err := e.rewriteRefs(v, dir, file); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitExternal(ref string) (file, pointer string) {
	file, fragment, _ := strings.Cut(ref, "#")
	if fragment == "" {
		return file, ""
	}
	return file, "#" + fragment
}

func fileOf(p Positions, file string) ir.Positions {
	out := make(ir.Positions, len(p))
	for k, v := range p {
		v.File = file
		out[k] = v
	}
	return out
}
