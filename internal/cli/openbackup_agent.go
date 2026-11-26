// SPDX-License-Identifier: AGPL-3.0-only

//go:build agent

package cli

import (
	"context"
	"errors"
	"io"
)

func openControlBackup(context.Context, IO, io.Reader, string, string) error {
	return usageError{errors.New("use the full build of dockwell to open a control plane backup")}
}
