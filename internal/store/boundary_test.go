// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every change to managed state is a revision with a plan (PRD principle
// 2). The store keeps the writes that make one on store.RevisionTx, which
// only store.DB.Revise hands out, and only the revision store may call it.
// This test reads every Go file in the module, outside tests, and fails
// on any other package that reaches for either.
func TestOnlyTheRevisionStoreWritesManagedState(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{"internal/store", "internal/revision"}
	var found int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "packages", "dist", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Revise" && sel.Sel.Name != "RevisionTx" {
				return true
			}
			found++
			if !slices.Contains(allowed, dir) {
				t.Errorf("%s uses %s: only internal/revision writes managed state. Propose a change through the revision store instead",
					rel, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("found no use of Revise or RevisionTx at all; the test is looking in the wrong place")
	}
}
