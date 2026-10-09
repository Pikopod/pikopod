package scenario

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPackErrorsHaveDocumentationLinks(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()

	file, err := goparser.ParseFile(fset, "pack.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "errfmt" {
			return true
		}

		docsIndex := -1
		switch selector.Sel.Name {
		case "New":
			docsIndex = 3
		case "Newf":
			docsIndex = 2
		default:
			return true
		}

		position := fset.Position(call.Pos())
		if len(call.Args) <= docsIndex {
			t.Errorf("%s: missing docs argument", position)
			return true
		}

		literal, ok := call.Args[docsIndex].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			t.Errorf("%s: docs argument must be a string literal", position)
			return true
		}

		docs, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Errorf("%s: invalid docs string: %v", position, err)
			return true
		}

		checked++
		if docs == "" {
			t.Errorf("%s: errfmt.%s has no documentation link",
				position, selector.Sel.Name)
			return true
		}

		path, anchor, hasAnchor := strings.Cut(docs, "#")
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Errorf("%s: documentation file %q: %v",
				position, path, err)
			return true
		}

		if hasAnchor {
			found := false
			for _, line := range strings.Split(string(raw), "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "#") {
					continue
				}

				heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
				slug := strings.ToLower(heading)
				slug = strings.ReplaceAll(slug, " ", "-")
				if slug == anchor {
					found = true
					break
				}
			}

			if !found {
				t.Errorf("%s: documentation anchor %q does not exist",
					position, docs)
			}
		}

		return true
	})

	if checked == 0 {
		t.Fatal("no pack error constructors found")
	}
}
