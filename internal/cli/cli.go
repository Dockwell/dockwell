// SPDX-License-Identifier: AGPL-3.0-only

// Package cli implements the dockwell command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/logging"
	"github.com/dockwell/dockwell/internal/store"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
	// exitClaimed is dockwell claim's when the install already has an
	// owner, which install.sh tells apart from a failure.
	exitClaimed = 3
)

// env names the environment variables that set flag defaults.
const (
	envLogLevel      = "DOCKWELL_LOG_LEVEL"
	envLogFormat     = "DOCKWELL_LOG_FORMAT"
	envDataDir       = "DOCKWELL_DATA_DIR"
	envListen        = "DOCKWELL_LISTEN"
	envAgentListen   = "DOCKWELL_AGENT_LISTEN"
	envAdvertise     = "DOCKWELL_ADVERTISE_URL"
	envDomain        = "DOCKWELL_DOMAIN"
	envHTTPSListen   = "DOCKWELL_HTTPS_LISTEN"
	envHTTPListen    = "DOCKWELL_HTTP_LISTEN"
	envACMEDirectory = "DOCKWELL_ACME_DIRECTORY_URL"
	envDatabase      = "DOCKWELL_DATABASE"
)

// IO carries the streams and environment a command runs with, so tests can
// substitute them.
type IO struct {
	Out    io.Writer
	Err    io.Writer
	Getenv func(string) string
}

// Execute runs the dockwell command line with args (without the program
// name) and returns the process exit code.
func Execute(ctx context.Context, args []string, stdio IO) int {
	if stdio.Getenv == nil {
		stdio.Getenv = os.Getenv
	}
	root := newRootCmd(stdio)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	fmt.Fprintf(stdio.Err, "dockwell: %s\n", err)
	var usage usageError
	var coded exitCodeError
	switch {
	case errors.As(err, &usage):
		return exitUsage
	case errors.As(err, &coded):
		return coded.code
	default:
		return exitError
	}
}

// usageTemplate replaces cobra's default help layout with one that follows
// Dockwell's content rules: sentence-case headings and a plain closing hint.
const usageTemplate = `Usage:
  {{if .HasAvailableSubCommands}}{{.CommandPath}} <command> [flags]{{else}}{{.UseLine}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }}  {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableSubCommands}}

Run "{{.CommandPath}} <command> --help" for more about a command.{{end}}
`

// usageError marks mistakes in how a command was invoked.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// exitCodeError is an error with its own exit code, for an outcome scripts
// tell apart from a failure.
type exitCodeError struct {
	code int
	error
}

func (e exitCodeError) Unwrap() error { return e.error }

type globalFlags struct {
	logLevel  string
	logFormat string
	dataDir   string
	database  string
}

// dir resolves the data directory: --data-dir, then the default for this user.
func (g *globalFlags) dir(stdio IO) (datadir.Dir, error) {
	if g.dataDir != "" {
		return datadir.Dir{Root: g.dataDir}, nil
	}
	return datadir.Default(stdio.Getenv)
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

func newRootCmd(stdio IO) *cobra.Command {
	var g globalFlags
	root := &cobra.Command{
		Use:   "dockwell",
		Short: "Run and manage Dockwell, the control plane for your servers",
		Long: "Dockwell runs websites, apps, databases and deploys on your servers, " +
			"from one VPS to a fleet, from one panel.\n\n" +
			"Every server runs this binary. The first one also runs the control plane.",
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
	}
	root.SetOut(stdio.Out)
	root.SetErr(stdio.Err)
	root.SetUsageTemplate(usageTemplate)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return usageError{fmt.Errorf("%w. Run %q to see the flags", err, cmd.CommandPath()+" --help")}
	})

	pf := root.PersistentFlags()
	pf.StringVar(&g.logLevel, "log-level", envOr(stdio, envLogLevel, "info"),
		"log level: debug, info, warn or error (env "+envLogLevel+")")
	pf.StringVar(&g.logFormat, "log-format", envOr(stdio, envLogFormat, string(logging.FormatAuto)),
		"log format: auto, text or json; auto writes text to a terminal (env "+envLogFormat+")")
	pf.StringVar(&g.dataDir, "data-dir", envOr(stdio, envDataDir, ""),
		"where dockwell keeps its state; /var/lib/dockwell for root, ~/.local/state/dockwell otherwise (env "+envDataDir+")")
	pf.StringVar(&g.database, "database", "",
		"the control plane's Postgres database, as a postgres:// URL; SQLite in the data directory when unset. "+
			"Set it with "+envDatabase+" or a .pgpass file rather than here, so its password stays out of the process list (env "+envDatabase+")")

	root.AddCommand(
		newVersionCmd(),
		newRunCmd(stdio, &g),
		newJoinCmd(stdio, &g),
		newTokenCmd(stdio, &g),
		newRoleCmd(stdio, &g),
		newClaimCmd(stdio, &g),
		newRecoverCmd(stdio, &g),
	)

	// Unknown subcommands are usage errors, with a pointer to the help.
	root.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return usageError{fmt.Errorf("unknown command %q. Run %q to see the commands", args[0], "dockwell --help")}
		}
		return nil
	}
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	return root
}

// noArgs rejects positional arguments as a usage error that points to the
// command's help. Use it instead of cobra.NoArgs.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError{fmt.Errorf("%s takes no arguments, but got %q. Run %q to see how to use it",
			cmd.CommandPath(), args[0], cmd.CommandPath()+" --help")}
	}
	return nil
}

func (g *globalFlags) logger(w io.Writer) (*slog.Logger, error) {
	logger, err := logging.New(w, logging.Options{Level: g.logLevel, Format: logging.Format(g.logFormat)})
	if err != nil {
		return nil, usageError{err}
	}
	return logger, nil
}

func envOr(stdio IO, key, fallback string) string {
	if v := stdio.Getenv(key); v != "" {
		return v
	}
	return fallback
}
