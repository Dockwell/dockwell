// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/backup/seal"
)

const envBackupPassphrase = "DOCKWELL_BACKUP_PASSPHRASE"

// newOpenBackupCmd opens a backup taken from a bucket by hand, with nothing else of
// Dockwell's: no control plane, and no data directory (ADR 0033).
func newOpenBackupCmd(stdio IO) *cobra.Command {
	var output, passphraseFile string
	cmd := &cobra.Command{
		Use:   "open-backup <file>",
		Short: "Open a backup that was sealed before it left its server",
		Long: "Opens a backup you fetched from a destination yourself, with no control plane or anything else of Dockwell's.\n\n" +
			"For a control plane backup, use the full build and give --output a new directory. For a volume or database, what comes out is a .tar.gz or .sql.gz. The .json beside it opens the same way, and says which. " +
			"Use the recovery passphrase for a control plane backup, or the destination's passphrase for a volume or database. Pass it in " + envBackupPassphrase + ", or a file with --passphrase-file, so it stays out of the process list. " +
			"Nothing it writes can be trusted until it finishes without an error.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageError{fmt.Errorf("%s takes the file to open. Run %q to see how to use it", cmd.CommandPath(), cmd.CommandPath()+" --help")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			passphrase := stdio.Getenv(envBackupPassphrase)
			if passphraseFile != "" {
				b, err := os.ReadFile(passphraseFile) //nolint:gosec // the file the person running it names
				if err != nil {
					return fmt.Errorf("can't read the passphrase: %w", err)
				}
				passphrase = strings.TrimRight(string(b), "\r\n")
			}
			in, err := os.Open(args[0])
			if err != nil {
				return fmt.Errorf("can't read the backup: %w", err)
			}
			defer in.Close()
			reader := bufio.NewReader(in)
			const controlPrefix = "dockwell-control-backup-"
			magic, _ := reader.Peek(len(controlPrefix))
			if bytes.Equal(magic, []byte(controlPrefix)) {
				if passphraseFile != "" {
					passphrase = strings.TrimSpace(passphrase)
				}
				return openControlBackup(cmd.Context(), stdio, reader, passphrase, output)
			}
			if passphrase == "" {
				return usageError{errors.New("this needs the destination's passphrase. Pass it in " + envBackupPassphrase + ", or a file with --passphrase-file")}
			}
			out := stdio.Out
			if output != "" {
				f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // the file the person running it names
				if err != nil {
					return fmt.Errorf("can't write what it holds: %w", err)
				}
				defer f.Close()
				out = f
			}
			w := bufio.NewWriterSize(out, 1<<20)
			if _, err := seal.Open(w, reader, passphrase, ""); err != nil {
				if output != "" {
					_ = os.Remove(output)
				}
				return err
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "the file to write; standard output when unset")
	cmd.Flags().StringVar(&passphraseFile, "passphrase-file", "", "a file holding the passphrase, so it stays out of the process list")
	return cmd
}
