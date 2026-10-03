package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSandboxErrorsHaveDocumentationLinks(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sandbox.go", nil, 0)
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
		docsIndex := 3
		switch selector.Sel.Name {
		case "New":
		case "Newf":
			docsIndex = 2
		default:
			return true
		}
		checked++
		position := fset.Position(call.Pos())
		literal, ok := call.Args[docsIndex].(*ast.BasicLit)
		if !ok {
			t.Errorf("%s: docs argument must be a string literal", position)
			return true
		}
		docs, err := strconv.Unquote(literal.Value)
		if err != nil || docs == "" {
			t.Errorf("%s: errfmt.%s has no valid documentation link", position, selector.Sel.Name)
			return true
		}
		path, anchor, hasAnchor := strings.Cut(docs, "#")
		raw, err := os.ReadFile(filepath.Join("../..", path))
		if err != nil {
			t.Errorf("%s: documentation file %q: %v", position, path, err)
			return true
		}
		if hasAnchor {
			found := false
			for _, line := range strings.Split(string(raw), "\n") {
				if !strings.HasPrefix(line, "#") {
					continue
				}
				heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
				if strings.ReplaceAll(strings.ToLower(heading), " ", "-") == anchor {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: documentation anchor %q does not exist", position, docs)
			}
		}
		return true
	})
	if checked == 0 {
		t.Fatal("no sandbox error constructors found")
	}
}
