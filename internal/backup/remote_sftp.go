// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"slices"
	"strings"

	ids "github.com/dockwell/dockwell/internal/id"

	"github.com/dockwell/dockwell/internal/backup/sftpstore"
)

// The seal's header may contain server-supplied text. Keep it out of Activity
// when a provider gives us something other than the backup we asked for.
var errSFTPInvalidCopy = errors.New("the SFTP copy couldn't be verified, so Dockwell kept no local copy. Check the destination and its backup passphrase, then fetch again")

func (r *Runner) sendSFTP(ctx context.Context, d *Destination, m *Manifest, key string, body io.Reader, total int64, bound string) (*Copied, error) {
	client, _, err := sftpstore.OpenUpload(ctx, *d.SFTP, nil)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	d.sweep(ctx, client, key)
	if err := client.PrepareUpload(ctx, key, total); err != nil {
		return nil, err
	}
	archive, err := client.Stage(key, total, body)
	if err != nil {
		return nil, err
	}
	defer archive.Discard()
	sealed, err := r.sealManifest(m, d, bound)
	if err != nil {
		return nil, err
	}
	manifest, err := client.Stage(key+".json", int64(len(sealed)), bytes.NewReader(sealed))
	if err != nil {
		return nil, err
	}
	defer manifest.Discard()
	// Publish the archive last. An interruption between renames can leave a
	// sealed manifest, but no incomplete archive is visible under its final name.
	if err := manifest.Commit(); err != nil {
		return nil, err
	}
	if err := archive.Commit(); err != nil {
		_ = client.Delete(ctx, key+".json")
		return nil, err
	}
	return &Copied{Key: key, Bytes: total}, nil
}

// sweep removes old files only for sends the control plane recorded as failed.
// Both the backup ID and the modification time must predate the cutoff.
func (d *Destination) sweep(ctx context.Context, client *sftpstore.Client, key string) {
	if d.sweepFailedIDs == "" || d.sweepFolder == "" || path.Dir(key) != d.sweepFolder || d.sweepBefore.Unix() <= 0 {
		return
	}
	failed := make(map[string]struct{})
	for id := range strings.SplitSeq(d.sweepFailedIDs, "\n") {
		failed[id] = struct{}{}
	}
	client.Sweep(ctx, d.sweepFolder, d.sweepBefore, func(name string) bool {
		id, suffix, ok := strings.Cut(strings.TrimSuffix(name, ".json"), ".")
		format, sealed := strings.CutSuffix(suffix, ".sealed")
		if !ok || !sealed || !slices.Contains(formats, format) {
			return false
		}
		if _, known := failed[id]; !known {
			return false
		}
		created, err := ids.Parse(ids.Backup, id)
		return err == nil && created.Before(d.sweepBefore)
	})
}
