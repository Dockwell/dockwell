// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/cli/apicli"
)

// The agent build says where each command it lacks is: the API commands
// through unknownCommand, and the commands that run beside the control
// plane through a stand-in for each one controlServerOnly names.
func TestControlServerOnlyNamesWhatTheAgentBuildLacks(t *testing.T) {
	stdio := IO{Out: io.Discard, Err: io.Discard, Getenv: func(string) string { return "" }}
	root, err := newRootCmd(stdio)
	if err != nil {
		t.Fatal(err)
	}
	var agent []string
	for _, cmd := range agentCommands(stdio, &globalFlags{}) {
		agent = append(agent, cmd.Name())
	}
	var full []string
	for _, cmd := range root.Commands() {
		full = append(full, cmd.Name())
		switch {
		case cmd.GroupID == apicli.GroupID, cmd.Name() == "help", cmd.Name() == "completion":
		case slices.Contains(agent, cmd.Name()):
			if slices.Contains(controlServerOnly, cmd.Name()) {
				t.Errorf("controlServerOnly names dockwell %s, which both builds have. Take it off the list", cmd.Name())
			}
		case !slices.Contains(controlServerOnly, cmd.Name()):
			t.Errorf("dockwell %s is only in the full build, so the agent build says nothing useful about it. "+
				"Add it to controlServerOnly in editions.go if it runs beside the control plane, or put it in the API commands' group", cmd.Name())
		}
	}
	for _, name := range controlServerOnly {
		if !slices.Contains(full, name) {
			t.Errorf("controlServerOnly names dockwell %s, which the full build doesn't have. Take it off the list", name)
		}
	}
}

func TestTheFullBuildSaysWhatIsntACommand(t *testing.T) {
	r := run(t, nil, "deploy-everything")
	want := `unknown command "deploy-everything". Run "dockwell --help" to see the commands`
	if r.code != exitUsage || !strings.Contains(r.err, want) {
		t.Errorf("an unknown command = %d %q, want %d and %q", r.code, r.err, exitUsage, want)
	}
}
