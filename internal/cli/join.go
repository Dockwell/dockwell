// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/datadir"
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
			if hasControlPlane(dir) {
				return fmt.Errorf("this server runs a control plane, in %s, so it can't join another one. Join a different server, or move this one's servers first", dir.Root)
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
				id.ServerName, id.ServerID, id.Cert().NotAfter.UTC().Format("2 Jan 2006"))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "what to call this server; defaults to its hostname, made unique")
	return cmd
}
