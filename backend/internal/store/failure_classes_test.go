package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// Every FailureClass* string constant declared in this package must be in
// AllFailureClasses, and nothing else may be. Parsed from the source, so a
// new constant cannot be forgotten.
func TestAllFailureClassesListsEveryConstant(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				vs, ok := n.(*ast.ValueSpec)
				if !ok {
					return true
				}
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "FailureClass") || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, _ := strconv.Unquote(lit.Value)
						declared[v] = true
					}
				}
				return true
			})
		}
	}
	listed := map[string]bool{}
	for _, c := range AllFailureClasses {
		if listed[c] {
			t.Errorf("%s is listed twice", c)
		}
		listed[c] = true
	}
	for c := range declared {
		if !listed[c] {
			t.Errorf("failure class %q is declared but missing from AllFailureClasses", c)
		}
	}
	for c := range listed {
		if !declared[c] {
			t.Errorf("AllFailureClasses lists %q, which no FailureClass constant declares", c)
		}
	}
	if len(declared) < 20 {
		t.Fatalf("found only %d FailureClass constants; the parse is broken", len(declared))
	}
}
