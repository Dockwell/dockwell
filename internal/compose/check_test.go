// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"slices"
	"strings"
	"testing"
)

func TestVariablesSaysWhatEachNeeds(t *testing.T) {
	file := `
services:
  web:
    image: ghcr.io/acme/shop:${TAG:-1.4}
    environment:
      PASSWORD: ${DB_PASSWORD}
      HOST: $DB_HOST
      NOTE: costs $$5
      NESTED: ${A:-${B}}
      CHECKED: ${C:?set C}
      PLUS: ${D:+yes}
      BOTH: ${TWICE:-x}-${TWICE}
`
	want := []Variable{
		{Name: "A"},
		{Name: "B", Required: true},
		{Name: "C", Required: true},
		{Name: "D"},
		{Name: "DB_HOST", Required: true},
		{Name: "DB_PASSWORD", Required: true},
		{Name: "TAG"},
		{Name: "TWICE", Required: true},
	}
	if got := Variables(t.Context(), []byte(file)); !slices.Equal(got, want) {
		t.Errorf("Variables = %+v, want %+v", got, want)
	}
	if got := Variables(t.Context(), []byte("not: [yaml")); got != nil {
		t.Errorf("Variables of what isn't YAML = %+v", got)
	}
}

// A refusal points at the line it is about, so the panel can mark it.
func TestLineFindsWhatIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name, file string
		line       int
	}{
		{"a field a service can't take", "services:\n  web:\n    image: nginx\n    build: .\n", 4},
		{"a top-level field", "services:\n  web:\n    image: nginx\nsecrets:\n  a: {}\n", 4},
		{"a field of a health check", "services:\n  web:\n    image: nginx\n    healthcheck:\n      test: [CMD, x]\n      start_interval: 1s\n", 6},
		{"a volume's own field", "services:\n  web:\n    image: nginx\nvolumes:\n  data:\n    external: true\n", 6},
		{"a value of the wrong kind", "services:\n  web:\n    image: nginx\n    ports: 80\n", 4},
		{"a service without an image", "services:\n  web:\n    command: x\n  api:\n    command: y\n", 4},
		{"what isn't YAML", "services:\n  web:\n  - [\n", 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(t.Context(), []byte(tt.file), nil)
			if err == nil {
				t.Fatal("Parse took it")
			}
			if got := Line(t.Context(), []byte(tt.file), err); got != tt.line {
				t.Errorf("Line = %d, want %d, for %v", got, tt.line, err)
			}
		})
	}
}

// Each level of a nested default reads the rest of its string again, so a
// file of nothing else mustn't take time or stack.
func TestVariablesStopsAtTheDepthParseDoes(t *testing.T) {
	var b strings.Builder
	b.WriteString("services:\n  web:\n    image: nginx\n    command: \"")
	const depth = 40_000
	for range depth {
		b.WriteString("${A:-")
	}
	b.WriteString("x")
	for range depth {
		b.WriteString("}")
	}
	b.WriteString("\"\n")
	got := Variables(t.Context(), []byte(b.String()))
	if len(got) != 1 || got[0].Name != "A" || got[0].Required {
		t.Errorf("Variables = %+v", got)
	}
}

// Reading each alias again would cost what the aliases come to, which isn't
// what the file does: 256 KiB can come to 87 GiB.
func TestVariablesLeavesAFileThatComesToTooMuch(t *testing.T) {
	var b strings.Builder
	b.WriteString("x-a: &a \"${LONG}" + strings.Repeat("v", 128<<10) + "\"\nservices:\n  web:\n    image: nginx\n    command:\n")
	for range 8000 {
		b.WriteString("      - *a\n")
	}
	if b.Len() > MaxFile {
		t.Fatalf("the file is %d bytes, over the cap", b.Len())
	}
	if got := Variables(t.Context(), []byte(b.String())); got != nil {
		t.Errorf("Variables = %+v, want none", got)
	}
}
