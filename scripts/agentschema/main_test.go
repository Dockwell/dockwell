// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const header = "// SPDX-License-Identifier: Apache-2.0\n\n"

// write lays out a schema under proto and the code generated from it under gen:
// each file's imports, by its path.
func write(t *testing.T, files map[string][]string) (protoRoot, genRoot string) {
	t.Helper()
	dir := t.TempDir()
	protoRoot, genRoot = filepath.Join(dir, "proto"), filepath.Join(dir, "gen")
	for file, imports := range files {
		var text strings.Builder
		text.WriteString(header + "syntax = \"proto3\";\n")
		for _, i := range imports {
			text.WriteString("import \"" + i + "\";\n")
		}
		put(t, filepath.Join(protoRoot, filepath.FromSlash(file)), text.String())
		put(t, generated(genRoot, file),
			header+string(generatedLine)+"// source: "+file+"\n\npackage x\n")
	}
	return protoRoot, genRoot
}

// generated is where the code generated for a schema file is, under genRoot.
func generated(genRoot, file string) string {
	return filepath.Join(genRoot, filepath.FromSlash(strings.TrimSuffix(file, ".proto")+".pb.go"))
}

func put(t *testing.T, name, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLeavesOutWhatTheAgentsProtocolDoesNotImport(t *testing.T) {
	protoRoot, genRoot := write(t, map[string][]string{
		"dockwell/agent/v1/agent.proto":          {"dockwell/v1/health.proto", "google/protobuf/timestamp.proto"},
		"dockwell/v1/health.proto":               {"dockwell/v1/change.proto"},
		"dockwell/v1/change.proto":               {"dockwell/resource/v1alpha1/specs.proto"},
		"dockwell/v1/auth.proto":                 {"dockwell/v1/change.proto"},
		"dockwell/v1/catalog.proto":              {"dockwell/catalog/v1alpha1/entry.proto"},
		"dockwell/catalog/v1alpha1/entry.proto":  {},
		"dockwell/resource/v1alpha1/specs.proto": {},
	})
	left, err := run(protoRoot, genRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Only files of a package the agent links: nothing else imports the catalog's
	// format once catalog.pb.go is out, so its package needs no constraint.
	want := []string{generated(genRoot, "dockwell/v1/auth.proto"), generated(genRoot, "dockwell/v1/catalog.proto")}
	if !slices.Equal(left, want) {
		t.Fatalf("left out %v, want %v", left, want)
	}
	for file, out := range map[string]bool{
		"dockwell/v1/auth.proto": true, "dockwell/v1/catalog.proto": true,
		"dockwell/v1/health.proto": false, "dockwell/v1/change.proto": false, "dockwell/agent/v1/agent.proto": false,
		"dockwell/catalog/v1alpha1/entry.proto": false, "dockwell/resource/v1alpha1/specs.proto": false,
	} {
		text, err := os.ReadFile(generated(genRoot, file))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(text), "\n"+buildLine); got != out {
			t.Errorf("%s: left out of the agent build is %v, want %v", file, got, out)
		}
		// The license stays the file's first line, and the constraint above the
		// header that marks the file as generated, and so above the package.
		if !strings.HasPrefix(string(text), header) {
			t.Errorf("%s no longer starts with its license", file)
		}
		if out && bytes.Index(text, []byte(buildLine)) > bytes.Index(text, generatedLine) {
			t.Errorf("%s has the constraint below protoc-gen-go's header", file)
		}
	}

	// A second run changes nothing.
	before, _ := os.ReadFile(generated(genRoot, "dockwell/v1/auth.proto"))
	if _, err := run(protoRoot, genRoot); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(generated(genRoot, "dockwell/v1/auth.proto"))
	if !bytes.Equal(before, after) {
		t.Error("a second run changed a file that already had the constraint")
	}
}

func TestSaysWhenThereIsNoAgentProtocol(t *testing.T) {
	protoRoot, genRoot := write(t, map[string][]string{"dockwell/v1/auth.proto": {}})
	_, err := run(protoRoot, genRoot)
	if err == nil || !strings.Contains(err.Error(), "no agent protocol to follow") {
		t.Fatalf("got %v, want an error that says there's no agent protocol", err)
	}
}

func TestSaysWhenTheGeneratedCodeIsMissing(t *testing.T) {
	protoRoot, genRoot := write(t, map[string][]string{
		"dockwell/agent/v1/agent.proto": {"dockwell/v1/health.proto"},
		"dockwell/v1/health.proto":      {},
		"dockwell/v1/auth.proto":        {},
	})
	if err := os.Remove(generated(genRoot, "dockwell/v1/auth.proto")); err != nil {
		t.Fatal(err)
	}
	_, err := run(protoRoot, genRoot)
	if err == nil || !strings.Contains(err.Error(), "Run make generate") {
		t.Fatalf("got %v, want an error that says to run make generate", err)
	}
}

// The schema as it is: the agent's protocol imports the files it needs, and what
// the repository has generated matches what this would write.
func TestTheRepositorysGeneratedCodeIsCurrent(t *testing.T) {
	root := filepath.Join("..", "..")
	imports, err := readImports(filepath.Join(root, "proto"))
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for file := range imports {
		if strings.HasPrefix(file, agentProtocol+"/") {
			roots = append(roots, file)
		}
	}
	kept := closure(imports, append(roots, also...))
	left := leftOut(imports, kept)
	if len(left) == 0 {
		t.Fatal("the agent build would leave out none of the schema's files")
	}
	for file := range imports {
		text, err := os.ReadFile(generated(filepath.Join(root, "gen"), file))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Contains(string(text), "\n"+buildLine), slices.Contains(left, file); got != want {
			t.Errorf("gen has %s left out of the agent build: %v, and the schema says %v. Run make generate", file, got, want)
		}
	}
}
