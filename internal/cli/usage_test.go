// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"text/template"
	"unicode"

	"github.com/spf13/cobra"
)

// usageTemplate is the layout writeUsage replaced, as cobra ran it, with the two
// functions of cobra's it called. The test holds every command's help to it.
const usageTemplate = `Usage:
  {{if .HasAvailableSubCommands}}{{.CommandPath}} <command> [flags]{{else}}{{.UseLine}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }}  {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }}  {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableSubCommands}}

Run "{{.CommandPath}} <command> --help" for more about a command.{{end}}
`

// Help reads as it did when a template wrote it, for every command this build
// has, the API commands among them, and their examples, groups and flags (#622).
func TestHelpReadsAsTheTemplateWroteIt(t *testing.T) {
	tmpl := template.Must(template.New("usage").Funcs(template.FuncMap{
		"rpad":                    func(s string, n int) string { return fmt.Sprintf("%-*s", n, s) },
		"trimTrailingWhitespaces": func(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) },
	}).Parse(usageTemplate))
	stdio := IO{Out: io.Discard, Err: io.Discard, Getenv: func(string) string { return "" }}
	root, g := newServerCmd(stdio)
	if err := addAPICommands(root, stdio, g, nil); err != nil {
		t.Fatal(err)
	}
	root.InitDefaultHelpCmd()
	seen := map[string]bool{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		// writeUsage first, on the command as cobra built it: it merges the parents'
		// persistent flags itself, as cobra did before it ran the template.
		var want, got strings.Builder
		c.SetOut(&got)
		if err := writeUsage(c); err != nil {
			t.Fatalf("%s: %v", c.CommandPath(), err)
		}
		c.SetOut(nil)
		if err := tmpl.Execute(&want, c); err != nil {
			t.Fatalf("%s: %v", c.CommandPath(), err)
		}
		if got.String() != want.String() {
			t.Errorf("%s's help reads\n%s\nwhere the template wrote\n%s", c.CommandPath(), got.String(), want.String())
		}
		seen["examples"] = seen["examples"] || c.HasExample()
		seen["groups"] = seen["groups"] || len(c.Groups()) > 0
		seen["subcommands"] = seen["subcommands"] || c.HasAvailableSubCommands()
		seen["flags"] = seen["flags"] || c.HasAvailableLocalFlags()
		seen["global flags"] = seen["global flags"] || c.HasAvailableInheritedFlags()
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	for _, part := range []string{"groups", "subcommands", "flags", "global flags"} {
		if !seen[part] {
			t.Errorf("no command has %s, so this checked none", part)
		}
	}
}
