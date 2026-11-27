// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
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

func TestControlBackupRoomRemovesPartialCopiesInItsGeneration(t *testing.T) {
	root := t.TempDir()
	install := id.New("ins")
	for _, generation := range []int64{1, 2} {
		if err := os.MkdirAll(filepath.Join(root, "control-backups", keep.Prefix(install, generation)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	first := filepath.Join(root, "control-backups", keep.Prefix(install, 1), ".partial-abandoned")
	other := filepath.Join(root, "control-backups", keep.Prefix(install, 2), ".partial-other")
	whole := filepath.Join(root, "control-backups", keep.Prefix(install, 1), "complete.dwcb")
	for _, file := range []string{first, other, whole} {
		if err := os.WriteFile(file, []byte("sealed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := &calls{cfg: Config{Dir: root, keepingControlBackup: new(sync.Mutex)}}
	got := c.keepControlBackup(t.Context(), &agentv1.KeepControlBackupCall{Install: install, Generation: 1})
	if got.GetKeepControlBackup() == nil {
		t.Fatalf("room check failed: %v", got.GetError())
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first generation kept its abandoned copy: %v", err)
	}
	for _, file := range []string{other, whole} {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("sweep removed a file it should keep: %v", err)
		}
	}
}
