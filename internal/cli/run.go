// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/control"
)

func newRunCmd(stdio IO, g *globalFlags) *cobra.Command {
	var listen string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run Dockwell on this server",
		Long: "Runs the roles enabled on this server until it is stopped with SIGINT or SIGTERM.\n\n" +
			"For now that is the control plane, serving the API on " + control.DefaultListen +
			" unless --listen says otherwise.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger, err := g.logger(stdio.Err)
			if err != nil {
				return err
			}
			build := buildinfo.Read()
			logger.Info("starting dockwell", "version", build.Version, "commit", build.ShortCommit())

			srv, err := control.New(control.Config{Logger: logger, Build: build})
			if err != nil {
				return err
			}
			return withListenHint(srv.ListenAndServe(cmd.Context(), listen))
		},
	}
	cmd.Flags().StringVar(&listen, "listen", envOr(stdio, envListen, control.DefaultListen),
		"address for the API to listen on, as host:port (env "+envListen+")")
	return cmd
}

// withListenHint adds the fix to errors an operator can resolve themselves.
func withListenHint(err error) error {
	var addrErr *net.AddrError
	var dnsErr *net.DNSError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EADDRINUSE):
		return fmt.Errorf("%w. Stop the process using that address, or pass a different --listen address", err)
	case errors.Is(err, syscall.EACCES):
		return fmt.Errorf("%w. Ports below 1024 need root or the CAP_NET_BIND_SERVICE capability; pass a higher port with --listen", err)
	case errors.As(err, &addrErr), errors.As(err, &dnsErr):
		return fmt.Errorf("%w. Pass --listen as host:port, such as %s", err, control.DefaultListen)
	default:
		return err
	}
}
