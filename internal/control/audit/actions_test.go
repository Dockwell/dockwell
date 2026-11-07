// SPDX-License-Identifier: AGPL-3.0-only

package audit

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
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

const module = "github.com/dockwell/dockwell"

// actionLike is how an action reads: a noun, a dot and a verb.
var actionLike = regexp.MustCompile(`^[a-z][a-z_]*\.[a-z][a-z_]*$`)

// Every action the Go side records is in actionsFile, and nothing else is, so a new
// action can't reach the audit log without the panel's test asking for its words.
// The scan reads the source, and fails where it can't tell what an action is.
func TestEveryRecordedActionIsListed(t *testing.T) {
	root := repoRoot(t)
	files := map[string]string{}
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	found, problems := scanActions(t, files)
	for _, p := range problems {
		t.Error(p)
	}
	data, err := os.ReadFile(filepath.Join(root, actionsFile))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		t.Fatalf("%s: %v", actionsFile, err)
	}
	want := slices.Sorted(maps.Keys(found))
	if !slices.Equal(names, want) {
		var missing, extra []string
		for _, a := range want {
			if !slices.Contains(names, a) {
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

// The scan follows an action however it's written, and only an action: through
// wrappers, plain calls, assignments, slices of events, package variables and
// joined strings, and not through a function that never records one.
func TestTheScanFollowsAnActionAnywhere(t *testing.T) {
	found, problems := scanActions(t, map[string]string{
		"internal/store/store.go": `package store
type Event struct{ Action, Detail string }
type Tx struct{}
func (t *Tx) RecordEvent(e Event) error { return nil }`,
		"internal/one/one.go": `package one
import "` + module + `/internal/store"
const ActionDone = "job.done"
var verb = "job.started"
type H struct{ tx *store.Tx }
// A helper, and a wrapper of it, and a wrapper of that.
func (h *H) record(action string) error { return h.tx.RecordEvent(store.Event{Action: action}) }
func (h *H) say(what, action string) error { return h.record(action) }
func (h *H) tell(action string) error { return h.say("x", action) }
// A plain function that records.
func note(tx *store.Tx, a string) error { return tx.RecordEvent(store.Event{Action: a}) }
func (h *H) run(failed bool) error {
	action := ActionDone
	if failed {
		action = "job" + ".failed"
	}
	_ = h.tell(action)
	_ = h.say("y", verb)
	_ = note(h.tx, "job.noted")
	var e store.Event
	e.Action = "job.assigned"
	_ = h.tx.RecordEvent(e)
	for _, ev := range []store.Event{{Action: "job.listed"}} {
		_ = h.tx.RecordEvent(ev)
	}
	// Neither of these records anything.
	h.finish("compose.yaml")
	eventType := "container.die"
	_ = eventType
	return nil
}
func (h *H) finish(file string) {}`,
		"internal/two/two.go": `package two
import (
	"` + module + `/internal/one"
	"` + module + `/internal/store"
)
func record(tx *store.Tx) error { return tx.RecordEvent(store.Event{Action: one.ActionDone + ""}) }`,
	})
	want := []string{"job.assigned", "job.done", "job.failed", "job.listed", "job.noted", "job.started"}
	if got := slices.Sorted(maps.Keys(found)); !slices.Equal(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}
	if len(problems) != 0 {
		t.Errorf("problems: %v", problems)
	}

	// What it can't tell, it says, rather than miss it.
	_, problems = scanActions(t, map[string]string{
		"internal/store/store.go": `package store
type Event struct{ Action string }
type Tx struct{}
func (t *Tx) RecordEvent(e Event) error { return nil }`,
		"internal/three/three.go": `package three
import "` + module + `/internal/store"
type Thing struct{ Kind string }
func record(tx *store.Tx, th Thing) error { return tx.RecordEvent(store.Event{Action: th.Kind + ".made"}) }
func helper(tx *store.Tx, action string) error { return tx.RecordEvent(store.Event{Action: action}) }
func caller(tx *store.Tx, th Thing) error { return helper(tx, th.Kind) }`,
	})
	if len(problems) != 2 || !strings.Contains(problems[0], "Name the action with a constant or a literal") {
		t.Errorf("problems with unknown actions = %v, want one for each", problems)
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

// source is one parsed file, with the package it's in and what it imports.
type source struct {
	pkg     string            // its package, as a directory such as "internal/auth"
	f       *ast.File         //
	imports map[string]string // the name an internal package is used by, to its directory
}

// fn is a function or method, by its package and name.
type fn struct {
	src  *source
	decl *ast.FuncDecl
}

// scanner follows actions through the source.
type scanner struct {
	fset   *token.FileSet
	files  []*source
	values map[string]map[string][]ast.Expr // package, then a package-level constant's or variable's name, to its values
	funcs  map[string][]*fn                 // package + "." + name
	// helpers holds, for a function by package and name, the parameters whose
	// argument becomes an event's action.
	helpers  map[string]map[int]bool
	found    map[string]bool
	problems []string
}

// scanActions finds every action the files record, and what it can't tell.
func scanActions(t *testing.T, files map[string]string) (map[string]bool, []string) {
	t.Helper()
	s := &scanner{
		fset: token.NewFileSet(), values: map[string]map[string][]ast.Expr{}, funcs: map[string][]*fn{},
		helpers: map[string]map[int]bool{}, found: map[string]bool{},
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f, err := parser.ParseFile(s.fset, name, files[name], 0)
		if err != nil {
			t.Fatal(err)
		}
		src := &source{pkg: path.Dir(name), f: f, imports: map[string]string{}}
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			rest, ok := strings.CutPrefix(p, module+"/")
			if !ok {
				continue
			}
			local := path.Base(p)
			if im.Name != nil {
				local = im.Name.Name
			}
			src.imports[local] = rest
		}
		s.files = append(s.files, src)
		if s.values[src.pkg] == nil {
			s.values[src.pkg] = map[string][]ast.Expr{}
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, _ := spec.(*ast.ValueSpec)
					if vs == nil {
						continue
					}
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							s.values[src.pkg][n.Name] = append(s.values[src.pkg][n.Name], vs.Values[i])
						}
					}
				}
			case *ast.FuncDecl:
				key := src.pkg + "." + d.Name.Name
				s.funcs[key] = append(s.funcs[key], &fn{src: src, decl: d})
			}
		}
	}
	// Each pass finds the functions that hand a parameter on as an action, until
	// no more do, so a wrapper of a wrapper counts.
	for {
		before := s.helperCount()
		s.walk(false)
		if s.helperCount() == before {
			break
		}
	}
	s.walk(true)
	slices.Sort(s.problems)
	return s.found, slices.Compact(s.problems)
}

func (s *scanner) helperCount() int {
	n := 0
	for _, h := range s.helpers {
		n += len(h)
	}
	return n
}

// walk visits every place an action is named. On the last pass it records what
// each names, and what it can't tell.
func (s *scanner) walk(last bool) {
	for _, src := range s.files {
		for _, decl := range src.f.Decls {
			d, ok := decl.(*ast.FuncDecl)
			if !ok || d.Body == nil {
				continue
			}
			f := &fn{src: src, decl: d}
			events := eventVars(src, d)
			ast.Inspect(d.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.CompositeLit:
					if isEvent(src, v.Type) {
						s.eventLit(f, v, last)
					} else if arr, ok := v.Type.(*ast.ArrayType); ok && isEvent(src, arr.Elt) {
						for _, el := range v.Elts {
							if lit, ok := el.(*ast.CompositeLit); ok && lit.Type == nil {
								s.eventLit(f, lit, last)
							}
						}
					}
				case *ast.AssignStmt:
					for i, lhs := range v.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "Action" || i >= len(v.Rhs) {
							continue
						}
						if x, ok := sel.X.(*ast.Ident); ok && events[x.Name] {
							s.sink(f, v.Rhs[i], last)
						}
					}
				case *ast.CallExpr:
					for idx := range s.helpers[s.callee(src, v)] {
						if idx < len(v.Args) {
							s.sink(f, v.Args[idx], last)
						}
					}
				}
				return true
			})
		}
	}
}

func (s *scanner) eventLit(f *fn, lit *ast.CompositeLit, last bool) {
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Action" {
				s.sink(f, kv.Value, last)
			}
		}
	}
}

// sink is an expression that becomes an action: a value to record, a parameter
// that makes its function a helper, or something the scan can't tell.
func (s *scanner) sink(f *fn, e ast.Expr, last bool) {
	vals, param, ok := s.resolve(f, e, map[string]bool{})
	switch {
	case !ok:
		if last {
			s.problems = append(s.problems, fmt.Sprintf("%s: can't tell which action this records. Name the action with a constant or a literal, or pass one to a function that records it",
				s.fset.Position(e.Pos())))
		}
	case param >= 0:
		key := f.src.pkg + "." + f.decl.Name.Name
		if s.helpers[key] == nil {
			s.helpers[key] = map[int]bool{}
		}
		s.helpers[key][param] = true
	case last:
		for _, v := range vals {
			if !actionLike.MatchString(v) {
				s.problems = append(s.problems, fmt.Sprintf("%s: %q isn't an action, a noun, a dot and a verb", s.fset.Position(e.Pos()), v))
				continue
			}
			s.found[v] = true
		}
	}
}

// callee names the function a call reaches, as a key of funcs: a function or
// method of the caller's package, or a function an internal package exports.
func (s *scanner) callee(src *source, call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return src.pkg + "." + f.Name
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok {
			if pkg, ok := src.imports[x.Name]; ok {
				return pkg + "." + f.Sel.Name
			}
		}
		// A method: one of the caller's package by its name.
		return src.pkg + "." + f.Sel.Name
	}
	return ""
}

// resolve says what an expression holds: strings, a parameter of f by its index,
// or not ok when the scan can't tell.
func (s *scanner) resolve(f *fn, e ast.Expr, seen map[string]bool) (vals []string, param int, ok bool) {
	switch v := e.(type) {
	case *ast.ParenExpr:
		return s.resolve(f, v.X, seen)
	case *ast.BasicLit:
		str, err := strconv.Unquote(v.Value)
		return []string{str}, -1, err == nil && v.Kind == token.STRING
	case *ast.CallExpr:
		// A conversion, such as string(action).
		if id, isIdent := v.Fun.(*ast.Ident); isIdent && id.Name == "string" && len(v.Args) == 1 {
			return s.resolve(f, v.Args[0], seen)
		}
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return nil, -1, false
		}
		left, lp, lok := s.resolve(f, v.X, seen)
		right, rp, rok := s.resolve(f, v.Y, seen)
		if !lok || !rok || lp >= 0 || rp >= 0 {
			return nil, -1, false
		}
		for _, l := range left {
			for _, r := range right {
				vals = append(vals, l+r)
			}
		}
		return vals, -1, true
	case *ast.SelectorExpr:
		x, isIdent := v.X.(*ast.Ident)
		if !isIdent {
			return nil, -1, false
		}
		pkg, imported := f.src.imports[x.Name]
		if !imported {
			return nil, -1, false
		}
		return s.global(&fn{src: &source{pkg: pkg, imports: s.importsOf(pkg)}}, v.Sel.Name, seen)
	case *ast.Ident:
		if i, isParam := params(f.decl)[v.Name]; isParam {
			return nil, i, true
		}
		if assigned := locals(f.decl, v.Name); len(assigned) > 0 {
			for _, rhs := range assigned {
				got, p, ok := s.resolve(f, rhs, seen)
				if !ok || p >= 0 {
					return nil, -1, false
				}
				vals = append(vals, got...)
			}
			return vals, -1, true
		}
		return s.global(f, v.Name, seen)
	}
	return nil, -1, false
}

// global resolves a package-level constant or variable of f's package.
func (s *scanner) global(f *fn, name string, seen map[string]bool) ([]string, int, bool) {
	key := f.src.pkg + "." + name
	exprs := s.values[f.src.pkg][name]
	if len(exprs) == 0 || seen[key] {
		return nil, -1, false
	}
	seen[key] = true
	var vals []string
	at := &fn{src: f.src, decl: &ast.FuncDecl{Name: ast.NewIdent(""), Type: &ast.FuncType{Params: &ast.FieldList{}}}}
	for _, e := range exprs {
		got, p, ok := s.resolve(at, e, seen)
		if !ok || p >= 0 {
			return nil, -1, false
		}
		vals = append(vals, got...)
	}
	return vals, -1, true
}

// importsOf is what a package's files import, for its constants that name another's.
func (s *scanner) importsOf(pkg string) map[string]string {
	out := map[string]string{}
	for _, src := range s.files {
		if src.pkg == pkg {
			maps.Copy(out, src.imports)
		}
	}
	return out
}

// params are a function's parameters, by name, numbered as a call passes them.
func params(d *ast.FuncDecl) map[string]int {
	out := map[string]int{}
	if d.Type.Params == nil {
		return out
	}
	i := 0
	for _, field := range d.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			out[n.Name] = i
			i++
		}
	}
	return out
}

// locals are what a function assigns to a local variable, where it says.
func locals(d *ast.FuncDecl, name string) []ast.Expr {
	var out []ast.Expr
	if d.Body == nil {
		return nil
	}
	ast.Inspect(d.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if len(v.Lhs) != len(v.Rhs) {
				return true
			}
			for i, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
					out = append(out, v.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, n := range v.Names {
				if n.Name == name && i < len(v.Values) {
					out = append(out, v.Values[i])
				}
			}
		}
		return true
	})
	return out
}

// eventVars are a function's variables that hold a store.Event: declared as one,
// made from one, or passed in as one.
func eventVars(src *source, d *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	if d.Type.Params != nil {
		for _, field := range d.Type.Params.List {
			t := field.Type
			if star, ok := t.(*ast.StarExpr); ok {
				t = star.X
			}
			if isEvent(src, t) {
				for _, n := range field.Names {
					out[n.Name] = true
				}
			}
		}
	}
	ast.Inspect(d.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			if v.Type != nil && isEvent(src, v.Type) {
				for _, n := range v.Names {
					out[n.Name] = true
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range v.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || i >= len(v.Rhs) {
					continue
				}
				rhs := v.Rhs[i]
				if u, ok := rhs.(*ast.UnaryExpr); ok {
					rhs = u.X
				}
				if lit, ok := rhs.(*ast.CompositeLit); ok && isEvent(src, lit.Type) {
					out[id.Name] = true
				}
			}
		}
		return true
	})
	return out
}

// isEvent reports store.Event, as written outside the store, or Event within it.
func isEvent(src *source, e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		x, ok := v.X.(*ast.Ident)
		return ok && src.imports[x.Name] == "internal/store" && v.Sel.Name == "Event"
	case *ast.Ident:
		return src.pkg == "internal/store" && v.Name == "Event"
	}
	return false
}
