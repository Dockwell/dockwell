// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

const maxPinnedFolders = 50

// ErrPinnedFoldersFull means a user needs to remove a pin to add another.
var ErrPinnedFoldersFull = errors.New("you have 50 pinned folders on this server. Unpin one to add another")

// PinnedFolders lists a user's folder paths on a server.
func (t *Tx) PinnedFolders(ctx context.Context, user, server string) ([]string, error) {
	rows, err := t.query(ctx, `SELECT path FROM pinned_folders WHERE user_id = ? AND server_id = ? ORDER BY path`, user, server)
	if err != nil {
		return nil, fmt.Errorf("can't list pinned folders: %w", err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

// PinFolder records a pin in a serialized write transaction.
func (t *Tx) PinFolder(ctx context.Context, user, server, path string) error {
	paths, err := t.PinnedFolders(ctx, user, server)
	if err != nil {
		return err
	}
	if slices.Contains(paths, path) {
		return nil
	}
	if len(paths) >= maxPinnedFolders {
		return ErrPinnedFoldersFull
	}
	_, err = t.exec(ctx, `INSERT INTO pinned_folders (user_id, server_id, path, path_hash) VALUES (?, ?, ?, ?)`, user, server, path, pinHash(path))
	return err
}

// UnpinFolder removes a pin if present.
func (t *Tx) UnpinFolder(ctx context.Context, user, server, path string) error {
	_, err := t.exec(ctx, `DELETE FROM pinned_folders WHERE user_id = ? AND server_id = ? AND path_hash = ? AND path = ?`, user, server, pinHash(path), path)
	return err
}

// Index the digest so Postgres can pin a path as long as the agent accepts.
func pinHash(path string) string {
	digest := sha256.Sum256([]byte(path))
	return hex.EncodeToString(digest[:])
}
