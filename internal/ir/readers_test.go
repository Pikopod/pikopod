package ir_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pikopod/pikopod/internal/ir"
)

const irPath = "github.com/pikopod/pikopod/internal/ir"

var serialisationOnly = map[string]string{
	"ApiDefinition.IrVersion":         "pins the on-disk shape; read by people and the hash, never by code",
	"ApiDefinition.NormalizerVersion": "pins the source-to-IR mapping; same",
	"ApiDefinition.SourceKind":        "recorded for the hash and the IR file",
	"ApiDefinition.SourceTier":        "recorded for the hash; code keys on Status instead",
	"ApiDefinition.Servers":           "the provider's base URLs, carried for the person reading the IR file",
	"Metadata.Title":                  "documentation carried verbatim",
	"Metadata.Description":            "documentation carried verbatim",
	"Metadata.TermsOfService":         "documentation carried verbatim",
	"Endpoint.Tags":                   "documentation carried verbatim",
	"NamedSchema.Name":                "the schema's own name; code addresses schemas by ID",
	"Prov.Confidence":                 "the trust score behind IsUncertain, kept for the reader of the IR file",
	"Prov.Evidence":                   "debug pointer into the source document, excluded from the hash",
	"Server.*":                        "the provider's base URLs and their variables, carried under Servers for the reader",
	"ServerVariable.*":                "same",
	"SecurityRequirement.Scopes":      "OAuth scopes carried verbatim; the sandbox enforces the scheme, not its scopes",
	"Webhook.Method":                  "the documented delivery method; the sandbox always POSTs",
	"*.SourcePointer":                 "debug pointer into the source document",
	"*.Description":                   "documentation carried verbatim",
	"*.ID":                            "stable identity pinned by the goldens; tools address nodes by it",
}

func exempt(field string) bool {
	if serialisationOnly[field] != "" {
		return true
	}
	i := strings.LastIndex(field, ".")
	if i < 0 {
		return false
	}
	return serialisationOnly["*"+field[i:]] != "" || serialisationOnly[field[:i]+".*"] != ""
}

func TestExemptionListJustified(t *testing.T) {
	for k, why := range serialisationOnly {
		if strings.TrimSpace(why) == "" {
			t.Fatalf("exemption %s has no reason", k)
		}
	}
}

func TestNormalizerVersionBumped(t *testing.T) {
	if ir.IRVersion != "1.3.0" || ir.NormalizerVersion != "1.2.0" {
		t.Fatalf("IRVersion %s NormalizerVersion %s: a shape change bumps IRVersion deliberately", ir.IRVersion, ir.NormalizerVersion)
	}
}

func TestEveryIRFieldHasAReader(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	irPkg := checkPackage(t, fset, filepath.Join(root, "internal", "ir"), irPath, importer.Default())
	declared := reachableFields(irPkg)

	reads := map[string]bool{}
	fake := &stubImporter{ir: irPkg}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." || strings.HasPrefix(rel, ".") || strings.HasPrefix(rel, "testdata") || rel == "internal/ir" || rel == "internal/importer" {
			if rel == "internal/ir" || rel == "internal/importer" {
				return filepath.SkipDir
			}
			if rel != "." {
				return filepath.SkipDir
			}
		}
		if !(strings.HasPrefix(rel, "cmd") || strings.HasPrefix(rel, "internal")) && rel != "." {
			return filepath.SkipDir
		}
		files := parseDir(t, fset, path, false)
		if len(files) == 0 {
			return nil
		}
		info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
		conf := types.Config{Importer: fake, Error: func(error) {}}
		conf.Check(rel, fset, files, info)
		for _, sel := range info.Selections {
			if sel.Kind() != types.FieldVal {
				continue
			}
			obj := sel.Obj()
			if obj.Pkg() == nil || obj.Pkg().Path() != irPath {
				continue
			}
			reads[ownerName(sel.Recv())+"."+obj.Name()] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var dead []string
	for _, f := range declared {
		if reads[f] || exempt(f) {
			continue
		}
		dead = append(dead, f)
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Fatalf("IR fields nothing outside internal/ir and internal/importer reads (delete them, or justify a serialisation-only exemption):\n  %s", strings.Join(dead, "\n  "))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		if dir == filepath.Dir(dir) {
			t.Fatal("go.mod not found")
		}
	}
}

func parseDir(t *testing.T, fset *token.FileSet, dir string, tests bool) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || (!tests && strings.HasSuffix(name, "_test.go")) {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

func checkPackage(t *testing.T, fset *token.FileSet, dir, path string, imp types.Importer) *types.Package {
	t.Helper()
	files := parseDir(t, fset, dir, false)
	conf := types.Config{Importer: imp}
	pkg, err := conf.Check(path, fset, files, nil)
	if err != nil {
		t.Fatalf("type-check %s: %v", path, err)
	}
	return pkg
}

type stubImporter struct{ ir *types.Package }

func (s *stubImporter) Import(path string) (*types.Package, error) {
	if path == irPath {
		return s.ir, nil
	}
	if !strings.Contains(path, ".") {
		if pkg, err := importer.Default().Import(path); err == nil {
			return pkg, nil
		}
	}
	pkg := types.NewPackage(path, path[strings.LastIndex(path, "/")+1:])
	pkg.MarkComplete()
	return pkg, nil
}

func reachableFields(pkg *types.Package) []string {
	root, _ := pkg.Scope().Lookup("ApiDefinition").(*types.TypeName)
	seen := map[*types.Named]bool{}
	fields := map[string]bool{}
	var out []string
	var walk func(typ types.Type)
	walk = func(typ types.Type) {
		switch u := typ.(type) {
		case *types.Pointer:
			walk(u.Elem())
		case *types.Slice:
			walk(u.Elem())
		case *types.Map:
			walk(u.Elem())
		case *types.Named:
			if seen[u] || u.Obj().Pkg() == nil || u.Obj().Pkg().Path() != irPath {
				return
			}
			seen[u] = true
			st, ok := u.Underlying().(*types.Struct)
			if !ok {
				return
			}
			name := u.Obj().Name()
			for i := 0; i < st.NumFields(); i++ {
				f := st.Field(i)
				if !f.Exported() {
					continue
				}
				if !fields[name+"."+f.Name()] {
					fields[name+"."+f.Name()] = true
					out = append(out, name+"."+f.Name())
				}
				walk(f.Type())
			}
		}
	}
	walk(root.Type())
	sort.Strings(out)
	return out
}

func ownerName(recv types.Type) string {
	for {
		switch u := recv.(type) {
		case *types.Pointer:
			recv = u.Elem()
			continue
		case *types.Named:
			return u.Obj().Name()
		}
		return "?"
	}
}
