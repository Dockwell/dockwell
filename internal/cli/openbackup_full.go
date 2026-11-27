// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/dockwell/dockwell/internal/controlbackup"
)

func openControlBackup(ctx context.Context, stdio IO, reader io.Reader, passphrase, output string) error {
	if output == "" || passphrase == "" {
		return usageError{errors.New("give --output a new directory and pass the recovery passphrase in " +
			envBackupPassphrase + " or --passphrase-file")}
	}
	h, err := controlbackup.Extract(ctx, reader, passphrase, output)
	if err != nil {
		return fmt.Errorf("can't open the control plane backup: %w", err)
	}
	_, err = fmt.Fprintf(stdio.Out, "Verified control plane backup %s, generation %d, schema %d. Wrote state.db and key to %s\n",
		h.Bundle, h.Generation, h.Schema, output)
	return err
}
