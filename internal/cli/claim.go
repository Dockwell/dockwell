// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

// Settings that record panel addresses.
const (
	// settingPanelAddress records where the running control plane serves
	// the panel, so dockwell claim and dockwell recover print links that
	// work.
	settingPanelAddress = "panel.address"
	// settingClaimAddress records the panel address the newest claim link
	// was printed for, so a restart can tell whether it still works as
	// printed.
	settingClaimAddress = "claim.address"
)

// panelAddress is where the panel answers.
type panelAddress struct {
	// Domain is the panel's hostname over HTTPS, or empty.
	Domain string `json:"domain,omitempty"`
	// HTTPSPort is the HTTPS listener's port, when it isn't 443.
	HTTPSPort string `json:"https_port,omitempty"`
	// Host is the loopback address the panel's listener is bound to, such
	// as "127.0.0.1" or "::1". Empty means 127.0.0.1.
	Host string `json:"host,omitempty"`
	// Port is the loopback listener's port.
	Port string `json:"port"`
	// PublicHost is this server's address as other machines reach it.
	PublicHost string `json:"public_host,omitempty"`
}

// url is the panel's base URL for a link: the domain when there is one,
// otherwise localhost through a tunnel.
func (a panelAddress) url() string {
	switch {
	case a.Domain != "" && a.HTTPSPort != "" && a.HTTPSPort != "443":
		return "https://" + net.JoinHostPort(a.Domain, a.HTTPSPort)
	case a.Domain != "":
		return "https://" + a.Domain
	default:
		return "http://localhost:" + a.Port
	}
}

// tunnelHost is the address an SSH tunnel forwards to on this server, as
// ssh -L takes it: an IPv6 address in brackets.
func (a panelAddress) tunnelHost() string {
	switch {
	case a.Host == "":
		return "127.0.0.1"
	case strings.Contains(a.Host, ":"):
		return "[" + a.Host + "]"
	default:
		return a.Host
	}
}

// reachable is the public host, or empty when all that is known is a
// loopback address, which is no use to anyone elsewhere.
func (a panelAddress) reachable() string {
	if ip := net.ParseIP(a.PublicHost); ip != nil && ip.IsLoopback() {
		return ""
	}
	return a.PublicHost
}

// sharedAddresses are carrier-grade NAT's (RFC 6598): not public, though
// not private by net/netip's reckoning either.
var sharedAddresses = netip.MustParsePrefix("100.64.0.0/10")

// dnsAddress is the public host as an address a domain's DNS record can
// point at, for a certificate from Let's Encrypt: a public IP address. It is
// empty when all that is known is a hostname, or a loopback, private,
// shared or link-local address, as behind NAT, where this server can't see
// its public address.
func (a panelAddress) dnsAddress() string {
	ip, err := netip.ParseAddr(a.PublicHost)
	if err != nil {
		return ""
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || sharedAddresses.Contains(ip) || ip.Zone() != "" {
		return ""
	}
	return ip.String()
}

// dnsRecord is the kind of DNS record that points at dnsAddress.
func (a panelAddress) dnsRecord() string {
	if strings.Contains(a.dnsAddress(), ":") {
		return "AAAA"
	}
	return "A"
}

// sameLink reports whether a link printed for address a still works as
// printed at b: the same URL and, through a tunnel, the same address on
// this server for the tunnel to forward to.
func (a panelAddress) sameLink(b panelAddress) bool {
	return a.url() == b.url() && (a.Domain != "" || a.tunnelHost() == b.tunnelHost())
}

func loadPanelAddress(ctx context.Context, db *store.DB) (panelAddress, bool, error) {
	return loadAddress(ctx, db, settingPanelAddress)
}

// loadAddress reads a panel address recorded under setting, and reports
// whether there was one.
func loadAddress(ctx context.Context, db *store.DB, setting string) (panelAddress, bool, error) {
	var a panelAddress
	var raw []byte
	err := db.View(ctx, func(tx *store.Tx) (err error) {
		raw, err = tx.GetSetting(ctx, setting)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, false, fmt.Errorf("can't read the panel's address from setting %s: %w", setting, err)
	}
	return a, true, nil
}

// saveAddress records a panel address under setting.
func saveAddress(ctx context.Context, db *store.DB, setting string, a panelAddress) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return db.Update(ctx, func(tx *store.Tx) error { return tx.PutSetting(ctx, setting, raw) })
}

// newClaimLink makes a claim link that replaces any earlier one, and
// records the address it is printed for.
func newClaimLink(ctx context.Context, db *store.DB, people *auth.Service, addr panelAddress, actor string) (auth.Link, error) {
	link, err := people.CreateClaimLink(ctx, actor)
	if err != nil {
		return auth.Link{}, err
	}
	if err := saveAddress(ctx, db, settingClaimAddress, addr); err != nil {
		return auth.Link{}, err
	}
	return link, nil
}

// announce records where the panel answers, and tells the operator what to
// do next: claim the install with a fresh link while it has no owner, or,
// when the panel has just gained a domain, how to get a passkey for it.
func announce(ctx context.Context, stdio IO, logger *slog.Logger, db *store.DB, people *auth.Service, addr panelAddress) error {
	before, known, err := loadPanelAddress(ctx, db)
	if err != nil {
		return err
	}
	if err := saveAddress(ctx, db, settingPanelAddress, addr); err != nil {
		return err
	}
	claimed, err := people.Claimed(ctx)
	if err != nil {
		return err
	}
	if !claimed {
		// A restart keeps the link it printed before, which may be the one
		// install.sh showed, while it works as printed. Only dockwell claim
		// replaces such a link.
		expires, waiting, err := people.WaitingClaimLink(ctx)
		if err != nil {
			return err
		}
		printedFor, recorded, err := loadAddress(ctx, db, settingClaimAddress)
		if err != nil {
			return err
		}
		if waiting && recorded && printedFor.sameLink(addr) {
			logger.Info("this install has no owner yet; the claim link printed earlier still works", slog.Time("expires", expires))
			fmt.Fprintf(stdio.Out, "\nThis install has no owner yet. The claim link printed earlier still works for %s. "+
				"If you don't have it, run dockwell claim for a new one; it replaces the earlier link.\n\n", until(expires))
			return nil
		}
		link, err := newClaimLink(ctx, db, people, addr, "system")
		if err != nil {
			return err
		}
		logger.Info("this install has no owner yet; open the claim link printed on standard output to claim it",
			slog.Time("expires", link.ExpiresAt))
		if waiting {
			fmt.Fprintf(stdio.Out, "\nThe panel's address changed since the last claim link was printed, so that link doesn't work as printed. "+
				"The new link below replaces it.\n")
		}
		printClaim(stdio.Out, addr, link, stdio.Getenv)
		return nil
	}
	switch {
	case addr.Domain != "" && (!known || before.Domain != addr.Domain):
		logger.Info("the panel now answers on its domain; passkeys made on localhost don't work there", slog.String("domain", addr.Domain))
		fmt.Fprintf(stdio.Out, "\nThe panel now answers at https://%[1]s, as well as through an SSH tunnel.\n\n"+
			"Passkeys belong to the address they were made on, so passkeys made on localhost don't work at %[1]s. "+
			"Sign in there with your authenticator app or a recovery code, or with a link from dockwell recover. "+
			"Then add a passkey for %[1]s under Profile and security.\n\n", addr.Domain)
	case addr.Domain == "" && known && before.Domain != "":
		logger.Info("the panel no longer has a domain and answers only on loopback", slog.String("was", before.Domain))
		fmt.Fprintf(stdio.Out, "\nThe panel no longer answers at https://%[1]s, only through an SSH tunnel. "+
			"Passkeys made for %[1]s work again once you restart with --domain %[1]s.\n\n", before.Domain)
	}
	return nil
}

// printClaim prints the claim link and how to reach it.
func printClaim(w io.Writer, addr panelAddress, link auth.Link, getenv func(string) string) {
	fmt.Fprintf(w, "\nThis install has no owner yet. Claim it within %s with the link below; it works once.\n\n",
		until(link.ExpiresAt))
	printLink(w, addr, "/claim#"+link.Token, getenv)
	switch {
	case addr.Domain == "" && addr.dnsAddress() != "":
		fmt.Fprintf(w, "The panel stays on this server's loopback address until you give it a domain. "+
			"To serve it over HTTPS, point a domain's DNS %s record at %s, then restart with --domain.\n", addr.dnsRecord(), addr.dnsAddress())
	case addr.Domain == "":
		fmt.Fprintf(w, "The panel stays on this server's loopback address until you give it a domain. "+
			"To serve it over HTTPS, point a domain's DNS record at this server's public address, then restart with --domain.\n")
	default:
		fmt.Fprintf(w, "The first visit gets the panel's certificate from Let's Encrypt, which takes a few seconds.\n")
	}
	fmt.Fprintf(w, "Run dockwell claim for a new link.\n\n")
}

// printLink prints a panel link: on its own with a domain, or after the SSH
// tunnel that reaches the loopback panel.
func printLink(w io.Writer, addr panelAddress, path string, getenv func(string) string) {
	if addr.Domain != "" {
		fmt.Fprintf(w, "  %s%s\n\n", addr.url(), path)
		return
	}
	fmt.Fprintf(w, "On your computer, open a tunnel to this server:\n\n  ssh -L %[1]s:%[2]s:%[1]s %[3]s\n\n"+
		"Then open this link. On this server's own desktop, open it directly.\n\n  %[4]s%[5]s\n\n",
		addr.Port, addr.tunnelHost(), sshTarget(addr, getenv), addr.url(), path)
}

// until says how long is left before t, to the nearest minute and then no
// more than there is: whole hours from two hours on, minutes below that.
func until(t time.Time) string {
	d := time.Until(t).Round(time.Minute)
	switch {
	case d >= 2*time.Hour:
		d = d.Truncate(time.Hour)
	case d >= time.Minute:
		d = d.Truncate(time.Minute)
	default:
		d = time.Minute
	}
	return human.Duration(d)
}

// sshTarget guesses the user@host the operator reaches this server with:
// the address of the SSH session this runs in, when there is one, and
// otherwise the server's public address.
func sshTarget(addr panelAddress, getenv func(string) string) string {
	user := getenv("SUDO_USER")
	if user == "" {
		user = getenv("USER")
	}
	if user == "" {
		user = "root"
	}
	host := addr.reachable()
	if fields := strings.Fields(getenv("SSH_CONNECTION")); len(fields) == 4 {
		host = fields[2]
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return user + "@" + orPlaceholder(host, "your-server")
}

func orPlaceholder(s, placeholder string) string {
	if s == "" {
		return placeholder
	}
	return s
}

// openPeople opens an existing control plane's people, for commands that run
// beside it on the control server.
func openPeople(ctx context.Context, dir datadir.Dir, location string, logger *slog.Logger) (*auth.Service, *store.DB, panelAddress, error) {
	key, db, err := openControl(ctx, dir, location)
	if err != nil {
		return nil, nil, panelAddress{}, err
	}
	return peopleOn(ctx, key, db, logger)
}

// peopleOn is the people of the control plane whose key and database these
// are. It closes db when it fails.
func peopleOn(ctx context.Context, key *seal.Key, db *store.DB, logger *slog.Logger) (*auth.Service, *store.DB, panelAddress, error) {
	addr, known, err := loadPanelAddress(ctx, db)
	if err == nil && !known {
		err = errors.New("the control plane hasn't started yet. Start it with dockwell run, which prints a claim link")
	}
	if err != nil {
		_ = db.Close()
		return nil, nil, panelAddress{}, err
	}
	people, err := auth.New(auth.Config{DB: db, Key: key, Logger: logger, Sites: auth.Sites{Domain: addr.Domain}})
	if err != nil {
		_ = db.Close()
		return nil, nil, panelAddress{}, err
	}
	return people, db, addr, nil
}

func newClaimCmd(stdio IO, g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "claim",
		Short: "Print a new link that claims this install",
		Long: "Prints a one-time link that creates the owner account with a passkey. It replaces any earlier link " +
			"and expires in 24 h. Once the install has an owner, there is nothing to claim, and it exits with status " +
			strconv.Itoa(exitClaimed) + ", so scripts can tell that from a failure.\n\n" +
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
			location, err := g.databaseOf(stdio, dir)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			people, db, addr, err := openPeople(ctx, dir, location, logger)
			if err != nil {
				return err
			}
			defer db.Close()
			link, err := newClaimLink(ctx, db, people, addr, actor())
			if errors.Is(err, auth.ErrClaimed) {
				owner, oerr := people.Owner(ctx)
				if oerr != nil {
					return exitCodeError{exitClaimed, err}
				}
				return exitCodeError{exitClaimed, fmt.Errorf("this install already has an owner, %s. To sign in without a passkey, run dockwell recover", owner.Email)}
			}
			if err != nil {
				return err
			}
			printClaim(cmd.OutOrStdout(), addr, link, stdio.Getenv)
			return nil
		},
	}
}

func newRecoverCmd(stdio IO, g *globalFlags) *cobra.Command {
	var email string
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Print a one-time sign-in link, for when you lost your passkey",
		Long: "Prints a link that signs you in once, without a passkey or a code, and expires in " + human.Duration(auth.SignInLinkTTL) +
			". It replaces any earlier link for the same person. Use it to add a passkey after losing yours, " +
			"or for a new address after adding a domain.\n\n" +
			"Run it on the control server, as the user that runs dockwell. Anyone who can run it already controls this server.",
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
			location, err := g.databaseOf(stdio, dir)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			people, db, addr, err := openPeople(ctx, dir, location, logger)
			if err != nil {
				return err
			}
			defer db.Close()
			link, user, err := people.CreateSignInLink(ctx, email, actor())
			if errors.Is(err, auth.ErrInvalid) || errors.Is(err, auth.ErrNotFound) {
				return usageError{errors.New(strings.TrimPrefix(strings.TrimPrefix(err.Error(), auth.ErrInvalid.Error()+": "), auth.ErrNotFound.Error()+": "))}
			}
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "\nThis link signs you in as %s once, within %s.\n\n", user.Email, human.Duration(auth.SignInLinkTTL))
			printLink(w, addr, "/recover#"+link.Token, stdio.Getenv)
			fmt.Fprintf(w, "Then add a passkey for %s under Profile and security.\n\n", orPlaceholder(addr.Domain, "localhost"))
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "user", "", "the email of the person to sign in; defaults to the owner")
	return cmd
}
