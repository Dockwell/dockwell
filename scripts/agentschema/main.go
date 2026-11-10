// SPDX-License-Identifier: AGPL-3.0-only

// Command agentschema keeps the schema's files that the agent never uses out of
// the agent build (docs/adr/0060-what-an-idle-agent-keeps-in-memory.md).
//
// The agent's protocol, proto/dockwell/agent, imports messages from the public
// API's schema, proto/dockwell/v1, so the agent build links the Go package
// generated from it. Go links a package whole, and each generated file registers
// every message in it as the program starts, so an agent paid memory at idle for
// sign-in, alerts, the catalog and the rest of the API it never speaks. A
// generated file can stay out of one build, though: this finds the files the
// agent's protocol imports, directly or through another, and gives each of the
// others in the same Go package the constraint //go:build !agent || agenttests.
// The full build has them all, as before. So do the tests of the agent build's
// packages, which run with -tags agent,agenttests: some stand up a control plane
// for the agent to join, and a control plane needs the whole schema.
//
// make generate runs it after buf, which writes the files again without the
// constraint:
//
//	go run ./scripts/agentschema proto gen
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// agentProtocol is where the agent's protocol is, under the schema's root.
const agentProtocol = "dockwell/agent"

// also are schema files the agent build keeps though its protocol doesn't import
// them, for agent code that uses a message of theirs directly. None does now.
var also = []string{}

// constraint is what a file the agent build leaves out says, above the line that
// leaves it out.
const constraint = `// The agent build leaves this file out: the agent's protocol imports nothing from
// it, and each file an agent links costs every server memory at idle. To use one of
// its messages in the agent, import its .proto file from the agent's protocol, or
// name it in scripts/agentschema, then run make generate. Tests of the agent build
// that link the control plane need the file: run them with -tags agent,agenttests,
// as make test does.

` + buildLine + `
`

// buildLine is the constraint itself.
const buildLine = "//go:build !agent || agenttests\n"

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/agentschema <proto> <gen>")
		os.Exit(2)
	}
	left, err := run(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentschema:", err)
		os.Exit(1)
	}
	fmt.Printf("agentschema: the agent build leaves out %d of the schema's files\n", len(left))
}

// run gives the constraint to each generated file under gen that the agent build
// can leave out, and returns them.
func run(protoRoot, genRoot string) ([]string, error) {
	imports, err := readImports(protoRoot)
	if err != nil {
		return nil, err
	}
	var roots []string
	for file := range imports {
		if strings.HasPrefix(file, agentProtocol+"/") {
			roots = append(roots, file)
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("%s has no .proto file under %s, so there's no agent protocol to follow. Check the first argument is the schema's root", protoRoot, agentProtocol)
	}
	for _, file := range also {
		if _, ok := imports[file]; !ok {
			return nil, fmt.Errorf("scripts/agentschema keeps %s for the agent, and the schema has no such file. Take it off the list", file)
		}
	}
	kept := closure(imports, append(roots, also...))
	var left []string
	for _, file := range leftOut(imports, kept) {
		name := filepath.Join(genRoot, filepath.FromSlash(strings.TrimSuffix(file, ".proto")+".pb.go"))
		if err := constrain(name); err != nil {
			return nil, err
		}
		left = append(left, name)
	}
	return left, nil
}

var importLine = regexp.MustCompile(`(?m)^\s*import\s+(?:public\s+|weak\s+)?"([^"]+)"\s*;`)

// readImports maps each .proto file under root, by its path from root, to the
// files it imports.
func readImports(root string) (map[string][]string, error) {
	imports := map[string][]string{}
	//nolint:gosec // the schema's root, which make generate names
	err := filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(name) != ".proto" {
			return err
		}
		text, err := os.ReadFile(name) //nolint:gosec // a file of the schema, which make generate names
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		file := filepath.ToSlash(rel)
		imports[file] = []string{}
		for _, m := range importLine.FindAllSubmatch(text, -1) {
			imports[file] = append(imports[file], string(m[1]))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("can't read the schema under %s: %w", root, err)
	}
	return imports, nil
}

// closure is the roots and every file they import, directly or through another.
// A file from outside the schema, such as google/protobuf/timestamp.proto, has no
// entry in imports and leads nowhere.
func closure(imports map[string][]string, roots []string) map[string]bool {
	kept := map[string]bool{}
	var follow func(string)
	follow = func(file string) {
		if kept[file] {
			return
		}
		kept[file] = true
		for _, next := range imports[file] {
			follow(next)
		}
	}
	for _, root := range roots {
		follow(root)
	}
	return kept
}

// leftOut is the schema's files the agent build can leave out, sorted: those it
// doesn't keep, in a directory it keeps another file of. A directory is a Go
// package, so one it keeps nothing of isn't linked at all, and needs no
// constraint.
func leftOut(imports map[string][]string, kept map[string]bool) []string {
	linked := map[string]bool{}
	for file := range kept {
		linked[path.Dir(file)] = true
	}
	var left []string
	for file := range imports {
		if !kept[file] && linked[path.Dir(file)] {
			left = append(left, file)
		}
	}
	slices.Sort(left)
	return left
}

// generatedLine is the line protoc-gen-go starts its own header with, below the
// .proto file's.
var generatedLine = []byte("// Code generated by protoc-gen-go. DO NOT EDIT.\n")

// constrain gives the generated file the constraint, above protoc-gen-go's
// header, unless it has it.
func constrain(name string) error {
	text, err := os.ReadFile(name) //nolint:gosec // a file make generate just wrote
	if err != nil {
		return fmt.Errorf("can't read the code generated for a file the agent build leaves out: %w. Run make generate, which writes it first", err)
	}
	if bytes.Contains(text, []byte("\n"+buildLine)) {
		return nil
	}
	at := bytes.Index(text, generatedLine)
	if at < 0 {
		return errors.New(name + " doesn't have protoc-gen-go's header, so there's no telling where the constraint goes. Check what wrote it")
	}
	out := slices.Concat(text[:at], []byte(constraint), text[at:])
	if err := os.WriteFile(name, out, 0o644); err != nil { //nolint:gosec // generated code, as buf writes it
		return fmt.Errorf("can't write %s: %w", name, err)
	}
	return nil
}
