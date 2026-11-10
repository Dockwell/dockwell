// SPDX-License-Identifier: AGPL-3.0-only

//go:build agent

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/cli/apinames"
	"github.com/dockwell/dockwell/internal/datadir"
)

// This file is the agent build's, built with -tags agent: only this
// server's agent, for a server that joins a control plane. It stands in
// for edition_full.go, run_control.go and every other file that reaches
// the control plane, its store or the API client, none of which the agent
// build links, and says where the commands it lacks are
// (docs/adr/0023-a-full-build-and-an-agent-build.md).

const rootLong = "Dockwell runs websites, apps, databases and deploys on your servers, " +
	"from one VPS to a fleet, from one panel.\n\n" +
	"This is the agent build of dockwell, for a server that joins a control plane: it runs only this server's agent. " +
	"The control server runs the full build, which has the control plane and the commands that manage it, " +
	"such as dockwell token create and dockwell servers list."

// globalFlags are the flags every command takes. The full build's
// --database is the control plane's, so the agent build has only these.
type globalFlags struct {
	sharedFlags
}

// roleNone is the role dockwell run takes on a server that hasn't joined:
// none, as it has no identity to run an agent with, and never runs a
// control plane.
const roleNone = "none"

// addEditionFlags adds nothing: --database is the control plane's.
func addEditionFlags(*pflag.FlagSet, *globalFlags) {}

// controlServerCommands stand in for the commands that run beside the
// control plane, and say so.
func controlServerCommands(IO, *globalFlags) []*cobra.Command {
	cmds := make([]*cobra.Command, 0, len(controlServerOnly))
	for _, name := range controlServerOnly {
		cmds = append(cmds, &cobra.Command{
			Use:    name,
			Hidden: true,
			// Whatever follows, such as create --ttl 1h, gets the same answer.
			DisableFlagParsing: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				return usageError{fmt.Errorf("%s isn't in the agent build of dockwell, which this server runs: "+
					"it runs on the control server, beside the control plane, where the full build has it. Run it there", typed(cmd, args))}
			},
		})
	}
	return cmds
}

// typed is the command as it was given, without its flags, such as
// "dockwell token create" for dockwell token create --ttl 1h.
func typed(cmd *cobra.Command, args []string) string {
	return strings.Join(append([]string{cmd.CommandPath()}, words(cmd.Root(), args)...), " ")
}

// words are the words of a command line that aren't flags: its commands
// and arguments. Flags that every command takes are left out, and so are
// others, with what looks like their values. Nil when the flags don't
// parse, which the command line itself then reports.
func words(root *cobra.Command, args []string) []string {
	flags := pflag.NewFlagSet(root.Name(), pflag.ContinueOnError)
	flags.ParseErrorsAllowlist.UnknownFlags = true
	flags.SetOutput(io.Discard)
	flags.AddFlagSet(root.PersistentFlags())
	if err := flags.Parse(args); err != nil {
		return nil
	}
	return flags.Args()
}

// addAPICommands adds none: they, and dockwell login and logout, are the
// full build's. For a command line that names one of them, it adds a
// stand-in of that name, which says so, so that a flag only that command
// has, such as login's --url, isn't refused first. Any other name this
// build lacks is left to cobra, for its own commands, such as completion
// and __complete, which it adds as it runs, or to the root's Args, which
// says it isn't a command.
func addAPICommands(root *cobra.Command, _ IO, _ *globalFlags, args []string) error {
	w := words(root, args)
	if len(w) == 0 || !apiCommand(w[0]) {
		return nil
	}
	name := w[0]
	root.AddCommand(&cobra.Command{
		Use:                name,
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return usageError{fmt.Errorf("%s isn't in the agent build of dockwell, which this server runs: "+
				"the API commands, with dockwell login and logout, are in the full build. "+
				"Run it on the control server, or with the full build anywhere else", typed(cmd, args))}
		},
	})
	return nil
}

// apiCommand reports whether the full build has a command of this name
// that calls the control plane's API, or signs in to it.
func apiCommand(name string) bool {
	return slices.Contains(apiAccessCommands, name) || slices.Contains(apinames.Commands(), name)
}

// apiUsage finds no usage error of the API commands': there are none.
func apiUsage(error) bool { return false }

// apiExitCode finds no exit code of the API commands' own: there are none.
func apiExitCode(error) (int, bool) { return 0, false }

// roleLong says what dockwell role prints in the agent build.
const roleLong = "Prints the role dockwell run takes with this data directory, for scripts:\n\n" +
	"  agent <url>    the agent, of the control plane at url\n" +
	"  none           nothing yet: this server hasn't joined a control plane, so join one with dockwell join\n\n" +
	"This is the agent build of dockwell, which never runs a control plane."

// editionRole is the role dockwell run takes in the agent build: the agent,
// once the server has joined, or none. A data directory that holds a
// control plane needs the full build.
func editionRole(dir datadir.Dir) (string, *enroll.Identity, error) {
	role, id, err := decideRole(dir)
	switch {
	case err != nil:
		return "", nil, err
	case role == roleAgent:
		return role, id, nil
	case hasControlPlane(dir):
		return "", nil, fmt.Errorf("this data directory holds a control plane, in %s, and this is the agent build of dockwell, which can't run one. "+
			"Install the full build on this server: run the install script again, which installs the full build on a control server", dir.Control())
	}
	return roleNone, nil, nil
}

// runCommand makes dockwell run run this server's agent, once it has joined
// a control plane. Without an identity, it says how to get one, and never
// starts a control plane.
func runCommand(cmd *cobra.Command, stdio IO, g *globalFlags) runRole {
	cmd.Long = "Runs this server's agent until it is stopped with SIGINT or SIGTERM. " +
		"The server joins a control plane first, with dockwell join.\n\n" +
		"This is the agent build of dockwell, which never runs a control plane. The control server runs the full build."
	return func(ctx context.Context, logger *slog.Logger, dir datadir.Dir, build buildinfo.Info) error {
		role, id, err := editionRole(dir)
		if err != nil {
			return err
		}
		if role == roleNone {
			return errors.New("this server hasn't joined a control plane, and this is the agent build of dockwell, which runs only an agent, so there's nothing to run. " +
				"Join one with dockwell join <ticket>, with a ticket from dockwell token create on the control server. " +
				"To make this server a control server instead, run the install script without --join, which installs the full build")
		}
		return runAgentAlone(ctx, stdio, g, logger, dir, id, build)
	}
}
