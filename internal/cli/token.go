// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

func newTokenCmd(stdio IO, g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Make join tickets that add servers",
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	var ttl time.Duration
	var controlURL string
	create := &cobra.Command{
		Use:   "create",
		Short: "Make a ticket that adds one server",
		Long: "Prints a ticket to paste on the server you're adding. It works once and expires after --ttl.\n\n" +
			"Run it on the control server, as the user that runs dockwell: with sudo, when it runs as the system service.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger, err := g.logger(stdio.Err)
			if err != nil {
				return err
			}
			dir, err := g.controlDir(stdio, "dockwell token create")
			if err != nil {
				return err
			}
			location, err := g.databaseOf(stdio, dir)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			auth, closeDB, err := openAuthority(ctx, dir, location, logger)
			if err != nil {
				return err
			}
			defer closeDB()
			var others []string
			if controlURL == "" {
				if controlURL, others, err = auth.JoinURLs(ctx); err != nil {
					return err
				}
			}
			tk, err := auth.CreateTicket(ctx, controlURL, actor(), ttl, others...)
			if err != nil {
				return usageError{err}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Run this on the server you're adding, as root:\n\n  dockwell join %s\n\n"+
				"The ticket adds one server through %s. It works once and expires in %s, at %s.\n%s",
				tk.Value, strings.Join(append([]string{controlURL}, others...), ", or else "), human.Duration(ttl), tk.ExpiresAt.UTC().Format("15:04 MST on 2 Jan"), joinWords(controlURL))
			return nil
		},
	}
	create.Flags().DurationVar(&ttl, "ttl", identity.DefaultTicketTTL, "how long the ticket works, at most 24h")
	create.Flags().StringVar(&controlURL, "control-url", "", "the control plane's agent URL to put in the ticket; defaults to what dockwell run advertises")
	cmd.AddCommand(create)
	return cmd
}

// joinWords says who a ticket's address can reach. A private address only reaches
// its own network, and nothing else says so until a join fails. A public one needs
// its port open, and a name can be anything.
func joinWords(controlURL string) string {
	u, err := url.Parse(controlURL)
	if err != nil {
		return ""
	}
	host, port := u.Hostname(), u.Port()
	ip, err := netip.ParseAddr(host)
	switch {
	case err != nil:
		return ""
	case resource.IsPublicAddress(ip):
		return fmt.Sprintf("\nA server on another network reaches this one on port %[1]s, so open that at your provider and on this machine, or forward it if this server is behind a router.\n", port)
	}
	return fmt.Sprintf("\n%[1]s is a private address, so only a server on the same network can use this ticket. "+
		"For a server elsewhere, set --advertise-url on dockwell run to an address it can reach, or open port %[2]s from that network.\n", host, port)
}

// actor names who ran a command, for the audit log.
func actor() string {
	user := os.Getenv("SUDO_USER")
	if user == "" {
		user = os.Getenv("USER")
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("cli:%s@%s", user, host)
}

// openControl opens an existing control plane's key and database at
// location, for commands that run beside it on the control server. It
// never creates either. The caller closes the database.
// It never migrates the state: only dockwell run does, as it starts the
// control plane on it. A command run between an upgrade and the service's
// restart would otherwise change the schema under the control plane that is
// still running.
func openControl(ctx context.Context, dir datadir.Dir, location string) (*seal.Key, *store.DB, error) {
	if err := controlState(dir, location); err != nil {
		return nil, nil, err
	}
	key, err := seal.Load(dir.SealKey())
	if err != nil {
		return nil, nil, err
	}
	db, err := openLocalState(ctx, dir, location)
	if err != nil {
		return nil, nil, err
	}
	return key, db, nil
}

// controlState checks that dir has a control plane, with its state at
// location.
func controlState(dir datadir.Dir, location string) error {
	switch {
	case !hasControlPlane(dir):
		return fmt.Errorf("there's no control plane in %s. Run this on the control server, or pass its --data-dir", dir.Root)
	case !store.IsPostgres(location) && !exists(location):
		return fmt.Errorf("the control plane in %s has no state in %s. If it keeps its state in Postgres, pass --database or set %s to its URL, as dockwell run has it. "+
			"sudo leaves %s out, so with sudo, keep it: sudo --preserve-env=%s",
			dir.Root, location, envDatabase, envDatabase, envDatabase)
	}
	return nil
}

// openAuthority opens an existing control plane's authority, for commands
// that run beside it. It never creates a key or an install.
func openAuthority(ctx context.Context, dir datadir.Dir, location string, logger *slog.Logger) (*identity.Authority, func(), error) {
	key, db, err := openControl(ctx, dir, location)
	if err != nil {
		return nil, nil, err
	}
	auth, err := identity.Open(ctx, db, key, logger)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return auth, func() { _ = db.Close() }, nil
}
