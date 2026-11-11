// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/control/settings"
)

// newPanelDomainCmd is the way back from a panel domain set in Settings that
// doesn't serve (ADR 0063): run on the control server, with local access to the
// data directory, as dockwell recover is, since no token can set it.
func newPanelDomainCmd(stdio IO, g *globalFlags) *cobra.Command {
	var clearIt bool
	cmd := &cobra.Command{
		Use:   "panel-domain --clear",
		Short: "Stop using the panel's domain set in Settings, when the panel can't be reached to",
		Long: "Clears the domain the panel was given under Settings, General, so it answers on loopback alone again, through an SSH tunnel, " +
			"and restarts the service when it runs as root under systemd or OpenRC, or says the command that does. It's on the activity log.\n\n" +
			"A domain given to dockwell run with --domain or DOCKWELL_DOMAIN is changed where dockwell run is started, not here.\n\n" +
			"Run it on the control server, as the user that runs dockwell: with sudo, when it runs as the system service, and with " +
			envDatabase + " passed through when the state is in Postgres, as in sudo --preserve-env=" + envDatabase + " dockwell panel-domain --clear.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !clearIt {
				return usageError{errors.New("say what to do: --clear stops using the panel's domain. Setting one is under Settings, General, in the panel")}
			}
			dir, err := g.controlDir(stdio, "dockwell panel-domain")
			if err != nil {
				return err
			}
			location, err := g.databaseOf(stdio, dir)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			_, db, err := openControl(ctx, dir, location)
			if err != nil {
				return err
			}
			defer db.Close()
			previous, err := settings.ClearPanelDomain(ctx, db, actor(), time.Now())
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if previous == "" {
				fmt.Fprintln(w, "The panel has no domain from its settings, so there's nothing to clear. It answers on loopback, through an SSH tunnel.")
				return nil
			}
			fmt.Fprintf(w, "The panel stops using %s, and answers on loopback alone once dockwell restarts.\n", previous)
			restart, how := restartCommand(ctx, stdio.Getenv)
			if restart == nil {
				fmt.Fprintf(w, "Restart dockwell for it to take effect: %s\n", how)
			} else {
				if out, err := restart.CombinedOutput(); err != nil {
					fmt.Fprintf(w, "Dockwell didn't restart: %v %s. Restart it with %s\n", err, out, how)
					return nil
				}
				fmt.Fprintln(w, "Dockwell restarted.")
			}
			fmt.Fprintln(w, "Reach the panel through an SSH tunnel, and dockwell recover prints a link to sign in there.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&clearIt, "clear", false, "stop using the panel's domain set in Settings")
	return cmd
}

// restartCommand is what restarts the dockwell service: run when this runs as
// root under systemd or OpenRC, and its words otherwise. A test runs none.
func restartCommand(ctx context.Context, getenv func(string) string) (*exec.Cmd, string) {
	systemd := exists("/run/systemd/system") && exists("/etc/systemd/system/dockwell.service")
	openrc := exists("/etc/init.d/dockwell")
	switch {
	case systemd:
		if os.Geteuid() == 0 && !isSet(getenv(envTest)) {
			return exec.CommandContext(ctx, "systemctl", "restart", "dockwell"), "sudo systemctl restart dockwell"
		}
		return nil, "sudo systemctl restart dockwell"
	case openrc:
		if os.Geteuid() == 0 && !isSet(getenv(envTest)) {
			return exec.CommandContext(ctx, "rc-service", "dockwell", "restart"), "sudo rc-service dockwell restart"
		}
		return nil, "sudo rc-service dockwell restart"
	}
	return nil, "stop dockwell run and start it again"
}
