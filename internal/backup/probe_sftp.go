// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"path"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/backup/sftpstore"
	"github.com/dockwell/dockwell/internal/netguard"
)

// TestSFTP verifies the SSH identity and tests the destination folder.
func TestSFTP(ctx context.Context, cfg sftpstore.Config, dial netguard.DialFunc) *dockwellv1.TestBackupDestinationResponse {
	client, ssh, err := sftpstore.Open(ctx, cfg, dial)
	checks := &dockwellv1.BackupDestinationSftpChecks{
		Reached: ssh.Reached, HostKeyFingerprint: ssh.Fingerprint, HostKeyVerified: ssh.Verified, HostKeyAlgorithm: ssh.HostKeyAlgorithm,
	}
	out := &dockwellv1.TestBackupDestinationResponse{SignedIn: ssh.SignedIn, Sftp: checks}
	fail := func(problem dockwellv1.BackupDestinationProblem, err error) {
		out.Problem, out.Said = problem, err.Error()
	}
	switch {
	case errors.Is(err, sftpstore.ErrHostKeyUnverified):
		fail(dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_HOST_KEY_UNVERIFIED, err)
	case errors.Is(err, sftpstore.ErrHostKeyChanged):
		fail(dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_HOST_KEY_CHANGED, err)
	case errors.Is(err, sftpstore.ErrUnreachable), errors.Is(err, sftpstore.ErrIdle):
		fail(dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_UNREACHABLE, err)
	case errors.Is(err, sftpstore.ErrAuth):
		fail(dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_KEYS_REFUSED, err)
	case err != nil:
		fail(dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_STORAGE_ERROR, err)
	}
	if err != nil {
		return out
	}
	defer client.Close()
	if err := client.Folder(); err != nil {
		fail(dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_CANT_OPEN_FOLDER, err)
		return out
	}
	checks.FolderReady = true

	const body = "Dockwell checks it can write here, and deletes this at once.\n"
	key := ".dockwell-test-" + rand.Text()
	staged, err := client.Stage(key, int64(len(body)), strings.NewReader(body))
	if err != nil {
		fail(dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_CANT_WRITE, err)
		return out
	}
	defer staged.Discard()
	if err := staged.Commit(); err != nil {
		fail(dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_CANT_WRITE, err)
		return out
	}
	out.Wrote = true
	if f, err := client.Get(ctx, key); err == nil {
		got, readErr := io.ReadAll(io.LimitReader(f, int64(len(body)+1)))
		checks.ReadBack = readErr == nil && string(got) == body
		_ = f.Close()
	}
	if !checks.ReadBack {
		fail(dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_CANT_READ, sftpstore.ErrRead)
	}
	if err := client.Delete(ctx, key); err != nil {
		out.TestFile = path.Join(cfg.Path, key)
		if checks.ReadBack {
			fail(dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_CANT_DELETE, err)
		}
	} else {
		checks.Deleted = true
	}
	if free, ok := client.FreeSpace(); ok {
		checks.FreeBytes = &free
	}
	return out
}
