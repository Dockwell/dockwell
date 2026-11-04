// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/datadir"
)

// Roles dockwell run takes on a server.
const (
	roleControl = "control"
	roleAgent   = "agent"
)

// decideRole says what dockwell run does with a data directory: the control
// plane (with its own agent) when it already holds one, or when the server
// hasn't joined another; otherwise only the agent, with its identity.
func decideRole(dir datadir.Dir) (string, *enroll.Identity, error) {
	id, err := enroll.Load(dir.Agent())
	switch {
	case exists(dir.StateDB()) || errors.Is(err, enroll.ErrNotJoined):
		return roleControl, nil, nil
	case err != nil:
		return "", nil, err
	default:
		return roleAgent, id, nil
	}
}

func newRoleCmd(stdio IO, g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "role",
		Short: "Show which role dockwell run takes on this server",
		Long: "Prints the role dockwell run takes with this data directory, for scripts:\n\n" +
			"  control        the control plane and its own agent\n" +
			"  agent <url>    only the agent, of the control plane at url",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := g.dir(stdio)
			if err != nil {
				return err
			}
			role, id, err := decideRole(dir)
			if err != nil {
				return err
			}
			if role == roleAgent {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", role, id.ControlURL)
			} else {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), role)
			}
			return err
		},
	}
}
