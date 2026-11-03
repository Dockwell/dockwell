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

	"github.com/dockwell/dockwell/internal/logging"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// env names the environment variables that set flag defaults.
const (
	envLogLevel  = "DOCKWELL_LOG_LEVEL"
	envLogFormat = "DOCKWELL_LOG_FORMAT"
	envListen    = "DOCKWELL_LISTEN"
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
	if errors.As(err, &usage) {
		return exitUsage
	}
	return exitError
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

type globalFlags struct {
	logLevel  string
	logFormat string
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

	root.AddCommand(
		newVersionCmd(),
		newRunCmd(stdio, &g),
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
