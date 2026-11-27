// SPDX-License-Identifier: AGPL-3.0-only

package sftpstore_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/backup/sftpstore"
	"github.com/dockwell/dockwell/internal/backup/sftpstore/sftptest"
)

func TestPrepareUploadCleansOnlyOwnedStaleFiles(t *testing.T) {
	const user, password = "backups", "example-password"
	server, err := sftptest.ListenDisk(t.Context(), "127.0.0.1:0", sftptest.Login{User: user, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	host, port, err := net.SplitHostPort(server.Address())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(port, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	cfg := sftpstore.Config{Host: host, Port: uint32(n), User: user, Password: password, Path: folder, Fingerprint: server.Fingerprint(), HostKeyAlgorithm: "ssh-ed25519"}
	client, _, err := sftpstore.Open(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	stale := ".dockwell-partial-" + strings.Repeat("A", 26)
	recent := ".dockwell-partial-" + strings.Repeat("B", 26)
	unrelated := ".dockwell-partial-not-a-dockwell-id"
	const archive = "backup.sealed"
	now := time.Now()
	for _, name := range []string{stale, recent, unrelated, archive + ".json"} {
		file := filepath.Join(folder, name)
		if err := os.WriteFile(file, []byte("leftovers"), 0o600); err != nil {
			t.Fatal(err)
		}
		if name != recent {
			old := now.Add(-25 * time.Hour)
			if err := os.Chtimes(file, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := client.PrepareUpload(t.Context(), archive, 0); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{stale, archive + ".json"} {
		if _, err := os.Stat(filepath.Join(folder, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was not cleaned up: %v", name, err)
		}
	}
	for _, name := range []string{recent, unrelated} {
		if _, err := os.Stat(filepath.Join(folder, name)); err != nil {
			t.Errorf("%s was not left alone: %v", name, err)
		}
	}
	if _, err := client.Stage(archive, 100, strings.NewReader("short")); !errors.Is(err, sftpstore.ErrWrite) {
		t.Fatalf("short stage: %v", err)
	}
	entries, err := os.ReadDir(folder)
	if err != nil || len(entries) != 2 {
		t.Fatalf("short stage left an extra file: %v, %v", entries, err)
	}
}
