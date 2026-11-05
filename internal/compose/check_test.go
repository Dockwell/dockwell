// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"errors"
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
	if got := inspect(t.Context(), []byte(file), nil).Variables; !slices.Equal(got, want) {
		t.Errorf("Variables = %+v, want %+v", got, want)
	}
	if got := inspect(t.Context(), []byte("not: [yaml"), nil).Variables; got != nil {
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
		{"a relative mount", "services:\n  web:\n    image: nginx\n    volumes:\n      - /srv/a:/a\n      - ./data:/data\n", 6},
		{"network_mode", "services:\n  web:\n    image: nginx\n    network_mode: host\n", 4},
		{"a restart it can't run", "services:\n  web:\n    image: nginx\n    user: a\n    restart: on-failure:3\n", 5},
		{"a port it can't run", "services:\n  web:\n    image: nginx\n    ports:\n      - \"8080:80\"\n      - \"80\"\n", 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(t.Context(), []byte(tt.file), nil)
			if err == nil {
				t.Fatal("Parse took it")
			}
			if got, _ := Lines(t.Context(), []byte(tt.file), err); got != tt.line {
				t.Errorf("Lines = %d, want %d, for %v", got, tt.line, err)
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
	got := inspect(t.Context(), []byte(b.String()), nil).Variables
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
	if got := inspect(t.Context(), []byte(b.String()), nil).Variables; got != nil {
		t.Errorf("Variables = %+v, want none", got)
	}
}

// A file that isn't YAML is said at the line the parser gave up on, the last it
// names, in words for people.
func TestNotYAML(t *testing.T) {
	if got := Message(notYAML(errors.New("yaml: no line here"), nil)); strings.Contains(got, "yaml:") {
		t.Errorf("with no line, it says the parser's words: %q", got)
	}
}

// What YAML says of a mistake is often about another line than the mistake's, as
// it counts from 0 in some errors, and finds out late in others. It either names
// the right one, or says near and marks them.
func TestAYAMLMistakeSaysWhereItIs(t *testing.T) {
	for _, tt := range []struct {
		name, file string
		from, to   int
		says       string
	}{
		{"a line too far out", "services:\n  web:\n    image: nginx\n   ports:\n      - 80\n", 4, 4, "line 4: this line doesn't line up"},
		{"a line too far in", "services:\n  web:\n    image: nginx\n      ports: [80]\n", 3, 4, "near lines 3\u20134: check for a missing colon"},
		{"a quote that doesn't close", "services:\n  web:\n    image: nginx\n    command:\n      - \"run\n    user: x\n", 5, 5, "line 5: this quote doesn't close"},
		{"a colon left out", "services:\n  web:\n    image nginx\n    user: x\n", 3, 4, "near lines 3\u20134: check for a missing colon"},
		{"a colon left out of a service", "services:\n  web\n    image: nginx\n", 2, 3, "near lines 2\u20133: check for a missing colon"},
		{"a bracket that doesn't close", "services:\n  web:\n    ports: [80\n    user: x\n", 3, 4, "near lines 3\u20134: a bracket doesn't close"},
		{"a key set twice", "services:\n  web:\n    image: a\n    image: b\n", 4, 4, "line 4: `image` is set twice, here and on line 3"},
		{"a tab", "services:\n  web:\n\timage: nginx\n", 3, 3, "line 3: check the indentation"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(t.Context(), []byte(tt.file), nil)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse = %v", err)
			}
			if !strings.HasPrefix(Message(err), tt.says) {
				t.Errorf("it says %q, want it to start with %q", Message(err), tt.says)
			}
			if from, to := Lines(t.Context(), []byte(tt.file), err); from != tt.from || to != tt.to {
				t.Errorf("Lines = %d, %d, want %d, %d, for %v", from, to, tt.from, tt.to, err)
			}
		})
	}
}

// What a file would be refused for whatever its variables hold comes before
// asking for them, and what might only be about a stand-in for one doesn't.
func TestAFileIsRefusedBeforeItsVariablesHaveValues(t *testing.T) {
	const head = "services:\n  web:\n    image: nginx:${TAG}\n"
	for name, c := range map[string]struct {
		file string
		says string
		line int
	}{
		"a relative mount":   {head + "    volumes: [\"./data:/data\"]\n", "a path relative to the Compose file", 4},
		"network_mode":       {head + "    network_mode: host\n", "has network_mode host", 4},
		"nothing wrong":      {head, "", 0},
		"its own variable":   {head + "    network_mode: ${MODE}\n", "", 0},
		"a variable's mount": {head + "    volumes: [\"${DATA}:/data\"]\n", "", 0},
		"a restart":          {head + "    restart: ${R}\n", "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			got := inspect(t.Context(), []byte(c.file), nil).Refused
			if c.says == "" {
				if got != nil {
					t.Errorf("Refused = %+v, want nothing", got)
				}
				return
			}
			if got == nil || !strings.Contains(got.Message, c.says) || got.From != c.line {
				t.Errorf("Refused = %+v, want line %d saying %q", got, c.line, c.says)
			}
		})
	}
	// Once there's nothing missing, Parse says it itself.
	if got := inspect(t.Context(), []byte(head+"    network_mode: host\n"), map[string]string{"TAG": "1"}).Refused; got != nil {
		t.Errorf("with every value given, Refused = %+v", got)
	}
}
