// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/cli/apicli"
	"github.com/dockwell/dockwell/internal/cli/apinames"
)

// The agent build says where each command it lacks is: the commands that
// run beside the control plane through a stand-in for each one
// controlServerOnly names, and the API commands through one for each name
// apinames and apiAccessCommands have.
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

// The agent build tells the full build's API commands from mistakes by the
// names apinames and apiAccessCommands have, so they must be exactly the
// full build's API commands, and none of them a command the agent build
// has, or one cobra adds.
func TestTheAgentBuildKnowsTheAPICommandsByName(t *testing.T) {
	stdio := IO{Out: io.Discard, Err: io.Discard, Getenv: func(string) string { return "" }}
	root, err := newRootCmd(stdio)
	if err != nil {
		t.Fatal(err)
	}
	var api []string
	for _, cmd := range root.Commands() {
		if cmd.GroupID == apicli.GroupID {
			api = append(api, cmd.Name())
		}
	}
	named := append(slices.Clone(apiAccessCommands), apinames.Commands()...)
	slices.Sort(api)
	slices.Sort(named)
	if !slices.Equal(api, named) {
		t.Errorf("the full build's API commands are %q, but apinames and apiAccessCommands name %q. "+
			"Run make generate after changing the schema, or change apiAccessCommands in editions.go with login and logout", api, named)
	}
	taken := []string{"help", "completion", "__complete", "__completeNoDesc"}
	for _, cmd := range agentCommands(stdio, &globalFlags{}) {
		taken = append(taken, cmd.Name())
	}
	for _, name := range named {
		if slices.Contains(taken, name) || slices.Contains(controlServerOnly, name) {
			t.Errorf("dockwell %s is an API command and another command at once, so the agent build can't tell which it is", name)
		}
	}
}

// A usage error comes first, whatever else an error carries, then an exit
// code of the CLI's own, then an API command's.
func TestExitCodesKeepTheirOrder(t *testing.T) {
	plain := errors.New("something failed")
	apiUsage := apicli.UsageError{Err: plain}
	apiCoded := apicli.ExitError{Code: apicli.ExitNeedsPerson, Err: plain}
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{
		{"an API command's usage error, with an exit code of the CLI's own", exitCodeError{exitClaimed, apiUsage}, exitUsage},
		{"an exit code of the CLI's own and an API command's usage error", errors.Join(exitCodeError{exitClaimed, plain}, apiUsage), exitUsage},
		{"a usage error, with an API command's exit code", usageError{apiCoded}, exitUsage},
		{"an exit code of the CLI's own, with an API command's", exitCodeError{exitClaimed, apiCoded}, exitClaimed},
		{"an API command's exit code", apiCoded, apicli.ExitNeedsPerson},
		{"an API command's usage error", apiUsage, exitUsage},
		{"any other error", plain, exitError},
	} {
		if got := exitCode(tt.err); got != tt.want {
			t.Errorf("%s exits %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestTheFullBuildSaysWhatIsntACommand(t *testing.T) {
	for _, tt := range []struct{ name, want string }{
		{"deploy-everything", `unknown command "deploy-everything". Run "dockwell --help" to see the commands`},
		{"servrs", `unknown command "servrs", though there is dockwell servers. Run "dockwell --help" to see the commands`},
	} {
		if r := run(t, nil, tt.name); r.code != exitUsage || !strings.Contains(r.err, tt.want) {
			t.Errorf("dockwell %s = %d %q, want %d and %q", tt.name, r.code, r.err, exitUsage, tt.want)
		}
	}
}
