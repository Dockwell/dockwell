// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

func newRotateKeyCmd(stdio IO, g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate-key",
		Short: "Rotate the key that wraps the keys of every stored secret",
		Long: "Makes a new key-encryption key, wraps the data key of every encrypted spec under it, and retires the old key, " +
			"forgetting it, all in one transaction. Secrets, and the specs that hold them, stay as they are: only how their keys " +
			"are kept changes. The rotation is in the audit log. The control plane can keep running: it unseals a key only for the " +
			"transaction that uses it, so from its next one it uses the new key, and holds the old one nowhere.\n\n" +
			"Run it on the control server, as the user that runs dockwell. See docs/adr/0022-secrets-encrypted-at-rest.md.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := g.dir(stdio)
			if err != nil {
				return err
			}
			location, err := g.databaseOf(stdio, dir)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			key, db, err := openControl(ctx, dir, location)
			if err != nil {
				return err
			}
			defer db.Close()
			// Which domain the panel has matters only to checking a new route, and this
			// reads stored specs alone.
			keeper := secrets.New(db, key, controlKinds("", nil).With(scope.Types()...))
			kekID, n, err := keeper.RotateKEK(ctx, actor(), store.SurfaceCLI)
			if err != nil {
				return fmt.Errorf("can't rotate the key-encryption key, so nothing changed: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrapped %d data keys under a new key-encryption key, %s, and retired the old one.\n", n, kekID)
			return nil
		},
	}
}
