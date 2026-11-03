// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
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
		Args: cobra.NoArgs,
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
			return srv.ListenAndServe(cmd.Context(), listen)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", envOr(stdio, envListen, control.DefaultListen),
		"address for the API to listen on (env "+envListen+")")
	return cmd
}
