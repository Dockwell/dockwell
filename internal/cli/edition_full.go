// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/cli/apicli"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/store"
)

// This file is the full build's: the control plane with its own agent, the
// commands that run beside it, and the API commands. The agent build has
// edition_agent.go instead, and links none of it
// (docs/adr/0023-a-full-build-and-an-agent-build.md). Every file that
// reaches the control plane, its store or the API client carries the same
// build constraint as this one.

const rootLong = "Dockwell runs websites, apps, databases and deploys on your servers, " +
	"from one VPS to a fleet, from one panel.\n\n" +
	"This is the full build of dockwell, which the control server runs: the control plane, " +
	"an agent of its own, and every command. The servers that join it run the agent build."

// exitClaimed is dockwell claim's exit code when the install already has an
// owner, which install.sh tells apart from a failure.
const exitClaimed = 3

// globalFlags are the flags every command takes.
type globalFlags struct {
	sharedFlags
	// database is --database: where the control plane keeps its state.
	// Only the full build has it.
	database string
}

// newRootCmd returns the root command with all of its commands: the
// server's own and the API commands.
func newRootCmd(stdio IO) (*cobra.Command, error) {
	root, g := newServerCmd(stdio)
	return root, addAPICommands(root, stdio, g, nil)
}

// addEditionFlags adds the flags only the full build has to every command.
func addEditionFlags(pf *pflag.FlagSet, g *globalFlags) {
	pf.StringVar(&g.database, "database", "",
		"the control plane's Postgres database, as a postgres:// URL; SQLite in the data directory when unset. "+
			"Set it with "+envDatabase+" or a .pgpass file rather than here, so its password stays out of the process list (env "+envDatabase+")")
}

// controlServerCommands are the commands that run beside the control plane,
// on the control server.
func controlServerCommands(stdio IO, g *globalFlags) []*cobra.Command {
	return []*cobra.Command{
		newTokenCmd(stdio, g),
		newClaimCmd(stdio, g),
		newRecoverCmd(stdio, g),
		{
			// The control plane runs this as a child with limits, to read a
			// Compose file someone sent (compose.UseGuard).
			Use:    compose.GuardCommand,
			Hidden: true,
			Args:   cobra.NoArgs,
			RunE:   func(*cobra.Command, []string) error { return compose.RunGuard(os.Stdin) },
		},
		newRotateKeyCmd(stdio, g),
	}
}

// addAPICommands adds the API commands to root, for a command line that
// may name one.
func addAPICommands(root *cobra.Command, stdio IO, g *globalFlags, _ []string) error {
	api, err := apiCommands(stdio, g)
	if err != nil {
		return fmt.Errorf("can't build the API commands, so dockwell can't run them: %w. This is a bug in this build of dockwell; its own commands, such as dockwell run, still work", err)
	}
	root.AddGroup(&cobra.Group{ID: apicli.GroupID, Title: "API commands, which call the control plane:"})
	root.AddCommand(api...)
	return nil
}

// roleLong says what dockwell role prints in the full build.
const roleLong = "Prints the role dockwell run takes with this data directory, for scripts:\n\n" +
	"  control        the control plane and its own agent\n" +
	"  agent <url>    only the agent, of the control plane at url"

// editionRole is the role dockwell run takes in the full build: either.
func editionRole(dir datadir.Dir) (string, *enroll.Identity, error) { return decideRole(dir) }

// apiUsage reports whether an error is an API command's usage error.
func apiUsage(err error) bool {
	var usage apicli.UsageError
	return errors.As(err, &usage)
}

// apiExitCode is the exit code of an error from an API command, when it has
// one of its own.
func apiExitCode(err error) (int, bool) {
	var coded apicli.ExitError
	if errors.As(err, &coded) {
		return coded.Code, true
	}
	return 0, false
}

// databaseOf resolves where the control plane keeps its state: --database,
// then DOCKWELL_DATABASE, each a postgres:// URL, and otherwise SQLite in
// the data directory. Neither the URL nor its password is ever echoed: the
// flag's default isn't the variable, so help doesn't print it, and errors
// name the setting, not its value.
func (g *globalFlags) databaseOf(stdio IO, dir datadir.Dir) (string, error) {
	location, from := g.database, "--database"
	if location == "" {
		location, from = stdio.Getenv(envDatabase), envDatabase
	}
	switch {
	case location == "":
		return dir.StateDB(), nil
	case !store.IsPostgres(location):
		return "", usageError{fmt.Errorf("%s must be a postgres:// URL, such as postgres://dockwell@db.internal:5432/dockwell. "+
			"Leave it unset to keep the control plane's state in SQLite, in the data directory", from)}
	}
	return location, nil
}
