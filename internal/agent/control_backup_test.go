// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/backup/keep"
	"github.com/dockwell/dockwell/internal/id"
)

func TestControlBackupRoomRefusal(t *testing.T) {
	if controlBackupRoom(10, 11) == nil || controlBackupRoom(10, 0) == nil || controlBackupRoom(10, 10) != nil {
		t.Fatal("room check did not respect the available bytes")
	}
}

func TestControlBackupKeepsAndPrunesOnlyAfterAWholeCopy(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := keep.Name(now.Add(-time.Hour), id.New("cbk"), id.New("phrase"))
	if err := os.WriteFile(filepath.Join(dir, old), []byte("older copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := keep.Name(now, id.New("cbk"), id.New("phrase"))
	sum := sha256.Sum256([]byte("sealed bytes"))
	next := func(part string) func() ([]byte, error) {
		return func() ([]byte, error) {
			defer func() { part = "" }()
			if part == "" {
				return nil, io.EOF
			}
			return []byte(part), nil
		}
	}
	if _, err := receiveControlBackup(dir, name, 12, sum[:], next("sealed")); err == nil {
		t.Fatal("accepted a truncated copy")
	}
	if _, err := os.Stat(filepath.Join(dir, old)); err != nil {
		t.Fatal("removed the previous copy after failure")
	}
	if problem, err := receiveControlBackup(dir, name, 12, sum[:], next("sealed bytes")); err != nil || problem != "" {
		t.Fatalf("could not keep a complete bundle: %v, %s", err, problem)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != name {
		t.Fatalf("wanted only the newer copy, got %v, %v", entries, err)
	}
}
