// SPDX-License-Identifier: AGPL-3.0-only

package lazyre

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// An expression made with Regexp compiles on its first use, so a broken one would
// panic then, in a running agent, not as it starts. So every one in the module's
// source compiles here: a string literal, or literals and the package's string
// constants and variables joined with + (#628's review, L2).
func TestEveryExpressionInTheSourceCompiles(t *testing.T) {
	const root = "../.."
	found := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || path == root {
			return nil
		}
		// What holds no Go of the module's, and this package, whose tests make broken ones.
		switch name := d.Name(); {
		case strings.HasPrefix(name, "."), name == "node_modules", name == "web", name == "packages", name == "design",
			name == "docs", name == "gen", name == "testdata", path == filepath.Join(root, "internal", "lazyre"):
			return filepath.SkipDir
		}
		found += compilesIn(t, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("compiled %d expressions made with lazyre.Regexp", found)
	// 64 when this was written.
	if found < 50 {
		t.Errorf("it found %d expressions made with lazyre.Regexp, so it read less of the source than there is", found)
	}
}

// compilesIn compiles each expression made with lazyre.Regexp in the package in dir,
// and says how many there were.
func compilesIn(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	// The package's string constants and variables, by name, as written.
	named := map[string]ast.Expr{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				v, ok := spec.(*ast.ValueSpec)
				if !ok || len(v.Names) != len(v.Values) {
					continue
				}
				for i, name := range v.Names {
					named[name.Name] = v.Values[i]
				}
			}
		}
	}
	var value func(e ast.Expr, depth int) (string, bool)
	value = func(e ast.Expr, depth int) (string, bool) {
		if depth > 8 {
			return "", false
		}
		switch e := e.(type) {
		case *ast.BasicLit:
			if e.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(e.Value)
			return s, err == nil
		case *ast.BinaryExpr:
			l, lok := value(e.X, depth+1)
			r, rok := value(e.Y, depth+1)
			return l + r, e.Op == token.ADD && lok && rok
		case *ast.ParenExpr:
			return value(e.X, depth+1)
		case *ast.Ident:
			if v, ok := named[e.Name]; ok {
				return value(v, depth+1)
			}
		}
		return "", false
	}
	found := 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Regexp" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "lazyre" {
				return true
			}
			found++
			at := fset.Position(call.Pos())
			if len(call.Args) != 1 {
				t.Errorf("%s: lazyre.Regexp takes one expression", at)
				return true
			}
			expr, ok := value(call.Args[0], 0)
			if !ok {
				t.Errorf("%s: this test can't read the expression lazyre.Regexp is given. "+
					"Give it a string literal, or literals and the package's string constants joined with +", at)
				return true
			}
			if _, err := regexp.Compile(expr); err != nil {
				t.Errorf("%s: %v. A running agent would panic on its first use", at, err)
			}
			return true
		})
	}
	return found
}
