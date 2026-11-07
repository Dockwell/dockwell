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
	"golang.org/x/term"

	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/logging"
	"github.com/dockwell/dockwell/internal/volumetool"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
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
	// envACMERoots names a PEM file of roots to trust for that CA, for the
	// ingress's certificates, as for Pebble.
	envACMERoots = "DOCKWELL_ACME_ROOTS"
	// envIngressImage names the repository to pull the ingress's image from,
	// for a registry mirror. Its digest stays pinned (ADR 0024).
	envIngressImage = "DOCKWELL_INGRESS_IMAGE"
	envDatabase     = "DOCKWELL_DATABASE"
	// envNoStun turns off asking a public STUN server, when set to anything but
	// false or 0 (ADR 0034).
	envNoStun = "DOCKWELL_NO_STUN"
	// envTest says this is a test, set to anything but false or 0, so the agent asks
	// nothing beyond this machine: no STUN server and no metadata service, as what they
	// said would depend on where it ran. The Makefile and CI set it for everything
	// they start.
	envTest = "DOCKWELL_TEST"
)

// IO carries the streams and environment a command runs with, so tests can
// substitute them.
type IO struct {
	Out    io.Writer
	Err    io.Writer
	Getenv func(string) string
	// In is standard input, for answers and tokens. Nil means os.Stdin.
	In io.Reader
	// Interactive reports whether a person is at a terminal to answer
	// questions. Nil means whether standard input and standard error are
	// terminals.
	Interactive func() bool
	// OutIsTerminal reports whether standard output is a terminal, which
	// dockwell run prints a claim link to and nowhere else. Nil means whether
	// Out is a file that is one.
	OutIsTerminal func() bool
}

// Execute runs the dockwell command line with args (without the program
// name) and returns the process exit code.
func Execute(ctx context.Context, args []string, stdio IO) int {
	if stdio.Getenv == nil {
		stdio.Getenv = os.Getenv
	}
	if stdio.In == nil {
		stdio.In = os.Stdin
	}
	if stdio.Interactive == nil {
		stdio.Interactive = func() bool {
			return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
		}
	}
	root, g := newServerCmd(stdio)
	// The API commands are built only for a command line that may name
	// one. The server's own commands, such as dockwell run, which the
	// control plane and the agent run as, never build them: building them
	// takes memory a server keeps, and nothing in them can stop a server
	// from starting.
	if found, _, _ := root.Find(args); found == root {
		if err := addAPICommands(root, stdio, g, args); err != nil {
			fmt.Fprintf(stdio.Err, "dockwell: %s\n", err)
			return exitError
		}
	}
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	fmt.Fprintf(stdio.Err, "dockwell: %s\n", err)
	return exitCode(err)
}

// exitCode is the process exit code for an error from a command. A usage
// error, the CLI's own or an API command's, comes first, then an exit code
// of the CLI's own, then an API command's.
func exitCode(err error) int {
	var usage usageError
	var coded exitCodeError
	switch {
	case errors.As(err, &usage), apiUsage(err):
		return exitUsage
	case errors.As(err, &coded):
		return coded.code
	}
	if code, ok := apiExitCode(err); ok {
		return code
	}
	return exitError
}

// unknownCommand explains a command that isn't one, and names the nearest
// command there is, when one is near.
func unknownCommand(root *cobra.Command, name string) error {
	msg := fmt.Sprintf("unknown command %q", name)
	if near := root.SuggestionsFor(name); len(near) > 0 {
		msg += ", though there is dockwell " + near[0]
	}
	return usageError{fmt.Errorf("%s. Run %q to see the commands", msg, "dockwell --help")}
}

// usageTemplate replaces cobra's default help layout with one that follows
// Dockwell's content rules: sentence-case headings and a plain closing hint.
// The root lists its commands in groups: those for this server, and those
// that call the control plane's API.
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

// sharedFlags are the flags every command takes in both builds. The
// globalFlags of each build hold them, and the full build's --database.
type sharedFlags struct {
	logLevel  string
	logFormat string
	dataDir   string
	// noStun is --no-stun, on dockwell run.
	noStun bool
}

// dir resolves the data directory: --data-dir, then the default for this user.
func (g *globalFlags) dir(stdio IO) (datadir.Dir, error) {
	if g.dataDir != "" {
		return datadir.Dir{Root: g.dataDir}, nil
	}
	return datadir.Default(stdio.Getenv)
}

// Help groups of the root's commands.
const groupServer = "server"

// newServerCmd returns the root command with the server's own commands,
// and the flags every command takes. Which commands those are depends on
// the build: the full build has them all, and the agent build only those
// an agent runs, with the rest saying where they are (edition_full.go,
// edition_agent.go, and docs/adr/0023-a-full-build-and-an-agent-build.md).
func newServerCmd(stdio IO) (*cobra.Command, *globalFlags) {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:               "dockwell",
		Short:             "Run and manage Dockwell, the control plane for your servers",
		Long:              rootLong,
		SilenceUsage:      true,
		SilenceErrors:     true,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
		// A command two letters off, as "joni" for join, is named.
		SuggestionsMinimumDistance: 2,
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
	addEditionFlags(pf, g)

	root.AddGroup(&cobra.Group{ID: groupServer, Title: "Commands:"})
	root.SetHelpCommandGroupID(groupServer)
	for _, cmd := range append(agentCommands(stdio, g), controlServerCommands(stdio, g)...) {
		cmd.GroupID = groupServer
		root.AddCommand(cmd)
	}

	// Unknown subcommands are usage errors, with a pointer to the help.
	root.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return unknownCommand(cmd, args[0])
		}
		return nil
	}
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
	return root, g
}

// agentCommands are the commands both builds have: what a server that
// joined a control plane runs.
func agentCommands(stdio IO, g *globalFlags) []*cobra.Command {
	return []*cobra.Command{
		newVersionCmd(),
		newRunCmd(stdio, g),
		newJoinCmd(stdio, g),
		newRoleCmd(stdio, g),
		{
			// What the agent runs in a throwaway container to read or write a
			// volume's bytes (ADR 0033).
			Use:                volumetool.Command,
			Hidden:             true,
			DisableFlagParsing: true,
			Run: func(_ *cobra.Command, args []string) {
				if code := volumetool.Main(args, stdio.In, stdio.Out, stdio.Err); code != 0 {
					os.Exit(code)
				}
			},
		},
	}
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
