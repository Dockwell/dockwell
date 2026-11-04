// SPDX-License-Identifier: AGPL-3.0-only

//go:build agent

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
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

// typed is the command as it was given, up to its first flag, such as
// "dockwell token create".
func typed(cmd *cobra.Command, args []string) string {
	words := []string{cmd.CommandPath()}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			break
		}
		words = append(words, arg)
	}
	return strings.Join(words, " ")
}

// addAPICommands adds none: they, and dockwell login and logout, are the
// full build's. For a command line that names a command this build lacks,
// it adds a stand-in of that name, which says so, so that a flag only that
// command has, such as login's --url, isn't refused first.
func addAPICommands(root *cobra.Command, _ IO, _ *globalFlags, args []string) error {
	flags := pflag.NewFlagSet(root.Name(), pflag.ContinueOnError)
	flags.ParseErrorsAllowlist.UnknownFlags = true
	flags.SetOutput(io.Discard)
	flags.AddFlagSet(root.PersistentFlags())
	if err := flags.Parse(args); err != nil || flags.NArg() == 0 {
		return nil //nolint:nilerr // the command line itself reports what's wrong with it
	}
	name := flags.Arg(0)
	if found, _, err := root.Find([]string{name}); err != nil || found != root || name == "help" {
		return nil //nolint:nilerr // a command this build has, or help
	}
	root.AddCommand(&cobra.Command{
		Use:                name,
		Hidden:             true,
		DisableFlagParsing: true,
		RunE:               func(*cobra.Command, []string) error { return unknownCommand(name) },
	})
	return nil
}

// unknownCommand explains a command this build doesn't have. The API
// commands are among them.
func unknownCommand(name string) error {
	return usageError{fmt.Errorf("%q isn't a command of the agent build of dockwell, which this server runs. "+
		"The control plane's commands, such as dockwell servers list and dockwell token create, are in the full build, on the control server. "+
		"Run %q to see this build's commands", name, "dockwell --help")}
}

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
func runCommand(cmd *cobra.Command, _ IO, _ *globalFlags) runRole {
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
		return runAgent(ctx, logger, dir, id, build)
	}
}
