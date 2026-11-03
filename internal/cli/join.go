// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

func newJoinCmd(stdio IO, g *globalFlags) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "join <ticket>",
		Short: "Add this server to a control plane with a join ticket",
		Long: "Gives this server its identity with a ticket from dockwell token create on the control server. " +
			"The server's key is made here and never leaves it.\n\n" +
			"Afterwards, dockwell run on this server runs only the agent.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageError{fmt.Errorf("%s takes one join ticket. Make one on the control server with dockwell token create", cmd.CommandPath())}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := g.dir(stdio)
			if err != nil {
				return err
			}
			if existing, err := enroll.Load(dir.Agent()); err == nil {
				return fmt.Errorf("this server already joined %s as %s (%s). To join another control plane, remove %s first",
					existing.ControlURL, existing.ServerName, existing.ServerID, dir.Agent())
			} else if !errors.Is(err, enroll.ErrNotJoined) {
				return err
			}
			if name == "" {
				name, _ = os.Hostname()
			}
			if err := datadir.Ensure(dir.Root); err != nil {
				return err
			}
			id, err := enroll.Join(cmd.Context(), args[0], name, buildinfo.Read())
			if err != nil {
				return err
			}
			if err := id.Save(dir.Agent()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Joined as %s (%s). The certificate is valid until %s and renews itself.\n"+
				"Start the agent with: systemctl enable --now dockwell, or dockwell run.\n",
				id.ServerName, id.ServerID, id.Cert.NotAfter.UTC().Format("2 Jan 2006"))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "what to call this server; defaults to its hostname, made unique")
	return cmd
}

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
			"Run it on the control server, as the user that runs dockwell.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger, err := g.logger(stdio.Err)
			if err != nil {
				return err
			}
			dir, err := g.dir(stdio)
			if err != nil {
				return err
			}
			if !exists(dir.StateDB()) {
				return fmt.Errorf("there's no control plane in %s. Run this on the control server, or pass its --data-dir", dir.Root)
			}
			ctx := cmd.Context()
			auth, closeDB, err := openAuthority(ctx, dir, logger)
			if err != nil {
				return err
			}
			defer closeDB()
			if controlURL == "" {
				if controlURL, err = auth.AgentURL(ctx); err != nil {
					return err
				}
			}
			tk, err := auth.CreateTicket(ctx, controlURL, actor(), ttl)
			if err != nil {
				return usageError{err}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Run this on the server you're adding, as root:\n\n  dockwell join %s\n\n"+
				"The ticket adds one server through %s. It works once and expires in %s, at %s.\n",
				tk.Value, controlURL, humanDuration(ttl), tk.ExpiresAt.UTC().Format("15:04 MST on 2 Jan"))
			return nil
		},
	}
	create.Flags().DurationVar(&ttl, "ttl", identity.DefaultTicketTTL, "how long the ticket works, at most 24h")
	create.Flags().StringVar(&controlURL, "control-url", "", "the control plane's agent URL to put in the ticket; defaults to what dockwell run advertises")
	cmd.AddCommand(create)
	return cmd
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

// humanDuration writes durations the way Dockwell does: "1 h", "30 min", "90 s".
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%d h", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d min", d/time.Minute)
	default:
		return fmt.Sprintf("%d s", d/time.Second)
	}
}

// openAuthority opens an existing control plane's authority, for commands
// that run beside it. It never creates a key or an install.
func openAuthority(ctx context.Context, dir datadir.Dir, logger *slog.Logger) (*identity.Authority, func(), error) {
	key, err := seal.Load(dir.SealKey())
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(ctx, dir.StateDB())
	if err != nil {
		return nil, nil, err
	}
	auth, err := identity.Bootstrap(ctx, db, key, logger)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return auth, func() { _ = db.Close() }, nil
}
