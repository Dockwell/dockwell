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
	"github.com/dockwell/dockwell/internal/labels"
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
	var joinLabels []string
	create := &cobra.Command{
		Use:   "create",
		Short: "Make a ticket that adds one server",
		Long: "Prints a ticket to paste on the server you're adding. It works once and expires after --ttl.\n\n" +
			"Run it on the control server, as the user that runs dockwell: with sudo, when it runs as the system service.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			given, err := labels.Apply(nil, joinLabels, nil)
			if err != nil {
				return usageError{err}
			}
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
			urls := []identity.JoinURL{{URL: controlURL, WayIn: true}}
			if controlURL == "" {
				if urls, err = auth.JoinURLs(ctx); err != nil {
					return err
				}
			}
			controlURL = urls[0].URL
			var others []string
			for _, u := range urls[1:] {
				others = append(others, u.URL)
			}
			tk, err := auth.CreateLabeledTicket(ctx, controlURL, actor(), ttl, given, others...)
			if err != nil {
				return usageError{err}
			}
			labeled := ""
			if len(given) > 0 {
				labeled = " with the labels " + strings.Join(labels.Strings(given), " ")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Run this on the server you're adding, as root:\n\n  dockwell join %s\n\n"+
				"The ticket adds one server%s through %s. It works once and expires in %s, at %s.\n%s",
				tk.Value, labeled, strings.Join(append([]string{controlURL}, others...), ", or else "), human.Duration(ttl), tk.ExpiresAt.UTC().Format("15:04 MST on 2 Jan"), joinWords(urls))
			return nil
		},
	}
	create.Flags().DurationVar(&ttl, "ttl", identity.DefaultTicketTTL, "how long the ticket works, at most 24h")
	create.Flags().StringArrayVar(&joinLabels, "label", nil, "a label the server gets as it joins, as key:value, such as region:eu; repeat for more")
	create.Flags().StringVar(&controlURL, "control-url", "", "the control plane's agent URL to put in the ticket; defaults to what dockwell run advertises")
	cmd.AddCommand(create)
	return cmd
}

// joinWords says who a ticket's addresses can reach, and what each case needs, as
// nothing else says so until a join fails. A way in needs its port open. A way out
// reaches this server only through a forwarded port, and a private address only from
// its own network. A name can be anything.
func joinWords(urls []identity.JoinURL) string {
	lead := urls[0]
	u, err := url.Parse(lead.URL)
	if err != nil {
		return ""
	}
	host, port := u.Hostname(), u.Port()
	ip, err := netip.ParseAddr(host)
	words := ""
	switch {
	case err != nil:
	case resource.IsPublicAddress(ip) && lead.WayIn:
		words = fmt.Sprintf("\nA server on another network reaches this one at %[1]s on port %[2]s, so open that port at your provider and on this machine.\n", host, port)
	case resource.IsPublicAddress(ip):
		words = fmt.Sprintf("\nThe ticket leads with %[1]s, which is how this server reaches the internet, and may not be a way in. "+
			"A server on another network joins through it only if port %[2]s there leads to this server, as with a port forwarded on a router. "+
			"If you know this server's public address, set it under Network on its page, and tickets lead with that.\n", host, port)
	default:
		words = fmt.Sprintf("\n%[1]s is a private address, so only a server on the same network can use this ticket. "+
			"For a server elsewhere, set this server's public address under Network on its page, or restart dockwell run with --advertise-url set to an address that server reaches.\n", host)
	}
	if len(urls) > 1 {
		words += fmt.Sprintf("A server running an older Dockwell tries %s alone, so upgrade it first if that won't reach this one.\n", lead.URL)
	}
	return words
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
