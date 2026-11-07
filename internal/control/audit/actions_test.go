// SPDX-License-Identifier: AGPL-3.0-only

package audit

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// actionsFile lists every action the control plane records, for the panel's test
// that each has a sentence in the audit log (auditWords.test.ts).
const actionsFile = "web/src/features/settings/auditActions.json"

// actionLike is how an action reads: a noun, a dot and a verb.
var actionLike = regexp.MustCompile(`^[a-z][a-z_]*\.[a-z][a-z_]*$`)

// Every action the Go side records is in actionsFile, and nothing else is, so a new
// action can't reach the audit log without the panel's test asking for its words.
// It reads the source: the Action of each store.Event written, the action each
// audit helper is handed, and the Action constants that writers use.
func TestEveryRecordedActionIsListed(t *testing.T) {
	root := repoRoot(t)
	found := recordedActions(t, filepath.Join(root, "internal"))
	listed := map[string]bool{}
	data, err := os.ReadFile(filepath.Join(root, actionsFile))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		t.Fatalf("%s: %v", actionsFile, err)
	}
	for _, n := range names {
		listed[n] = true
	}
	want := slices.Sorted(maps.Keys(found))
	if !slices.Equal(names, want) {
		var missing, extra []string
		for _, a := range want {
			if !listed[a] {
				missing = append(missing, a)
			}
		}
		for _, a := range names {
			if !found[a] {
				extra = append(extra, a)
			}
		}
		out, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("%s lists %v that nothing records, and lacks %v. Write it as:\n%s\nthen give each new action a sentence in auditWords.ts",
			actionsFile, extra, missing, out)
	}
	if len(found) < 40 {
		t.Errorf("found only %d actions, which says the scan missed how they're written: %v", len(found), want)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}

// helpers are the functions that take an action and record it, by name.
var helpers = map[string]bool{"audit": true, "auditFrom": true, "auditOn": true, "finish": true, "recordEvent": true}

// actionNames are what a writer names an action it picks at run time: a
// constant such as backups' ActionTaken or files' eventSave, or a variable.
func actionName(name string) bool {
	return strings.HasPrefix(name, "Action") || strings.HasPrefix(name, "action") || strings.HasPrefix(name, "event")
}

// recordedActions reads every non-test Go file under dir for the actions it records.
func recordedActions(t *testing.T, dir string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	type file struct {
		pkg string
		f   *ast.File
	}
	var files []file
	// consts holds each package's string constants, by package path and name.
	consts := map[string]map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		pkg := filepath.Dir(path)
		files = append(files, file{pkg: pkg, f: f})
		if consts[pkg] == nil {
			consts[pkg] = map[string]string{}
		}
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs, _ := spec.(*ast.ValueSpec)
				if vs == nil {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if s, ok := stringOf(vs.Values[i]); ok {
							consts[pkg][name.Name] = s
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// byName finds a package's constants by the name it's imported as, its last element.
	byName := map[string][]map[string]string{}
	for pkg, cs := range consts {
		byName[filepath.Base(pkg)] = append(byName[filepath.Base(pkg)], cs)
	}
	found := map[string]bool{}
	resolve := func(pkg string, e ast.Expr) {
		var s string
		switch v := e.(type) {
		case *ast.BasicLit:
			s, _ = stringOf(v)
		case *ast.Ident:
			s = consts[pkg][v.Name]
		case *ast.SelectorExpr:
			if x, ok := v.X.(*ast.Ident); ok {
				for _, cs := range byName[x.Name] {
					if c, ok := cs[v.Sel.Name]; ok {
						s = c
					}
				}
			}
		}
		if actionLike.MatchString(s) {
			found[s] = true
		}
	}
	for _, fl := range files {
		ast.Inspect(fl.f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CompositeLit:
				if !isEvent(v.Type) {
					return true
				}
				for _, el := range v.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Action" {
							resolve(fl.pkg, kv.Value)
						}
					}
				}
			case *ast.CallExpr:
				if sel, ok := v.Fun.(*ast.SelectorExpr); ok && helpers[sel.Sel.Name] {
					for _, arg := range v.Args {
						resolve(fl.pkg, arg)
					}
				}
			case *ast.GenDecl:
				// What a writer picks among at run time, such as backups' ActionTaken.
				if v.Tok != token.CONST {
					return true
				}
				for _, spec := range v.Specs {
					vs, _ := spec.(*ast.ValueSpec)
					if vs == nil {
						continue
					}
					for i, name := range vs.Names {
						if actionName(name.Name) && i < len(vs.Values) {
							resolve(fl.pkg, vs.Values[i])
						}
					}
				}
			case *ast.AssignStmt:
				// action := "passkey.add", and the others it may become.
				for i, lhs := range v.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && actionName(id.Name) && i < len(v.Rhs) {
						resolve(fl.pkg, v.Rhs[i])
					}
				}
			}
			return true
		})
	}
	return found
}

// isEvent reports store.Event, as written outside the store, or Event within it.
func isEvent(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		x, ok := v.X.(*ast.Ident)
		return ok && x.Name == "store" && v.Sel.Name == "Event"
	case *ast.Ident:
		return v.Name == "Event"
	}
	return false
}

func stringOf(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
