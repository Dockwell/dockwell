// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	ids "github.com/dockwell/dockwell/internal/id"

	"github.com/dockwell/dockwell/internal/backup/sftpstore"
	"github.com/dockwell/dockwell/internal/backup/sftpstore/sftptest"
	"github.com/dockwell/dockwell/internal/resource"
)

func sftpDestination(t *testing.T) (*Destination, *sftptest.Server) {
	t.Helper()
	const user, password = "backups", "example-fixture-password"
	server, err := sftptest.Listen(t.Context(), "127.0.0.1:0", sftptest.Login{User: user, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	host, port, err := net.SplitHostPort(server.Address())
	if err != nil {
		t.Fatal(err)
	}
	return DestinationFrom(map[string]string{
		"transport": "sftp", "host": host, "port": port, "user": user, "password": password,
		"path": "/off-site/backups", "host_key_fingerprint": server.Fingerprint(), "passphrase": "a long example archive passphrase",
	}), server
}

func TestSFTPSealedBackupsRestoreAndDelete(t *testing.T) {
	for _, kind := range []string{"volume", "database", "website"} {
		t.Run(kind, func(t *testing.T) {
			b := newBench(t)
			d, _ := sftpDestination(t)
			const backupID = "bkp_1"
			source := resource.RestoreSource{Backup: backupID, Change: "chg_1"}
			file := "index.html"
			content := "an example website"
			switch kind {
			case "website":
				source.HostPath = t.TempDir()
			case "database":
				file, content = "data.sql", "CREATE TABLE notes;"
				b.run("db", "postgres:17", "/var/lib/postgresql/data")
			default:
				file, content = "notes.txt", "an example volume"
			}
			dir := b.engine.VolumeDir(b.target.Name)
			if source.HostPath != "" {
				dir = source.HostPath
			}
			file = filepath.Join(dir, file)
			write := func(body string) {
				t.Helper()
				if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(content)
			m, err := b.runner.Take(t.Context(), Take{ID: backupID, Ref: ref, HostPath: source.HostPath})
			if err != nil {
				t.Fatal(err)
			}
			if (kind == "database") != (m.Database == "PostgreSQL") {
				t.Fatalf("unexpected manifest format: %+v", m)
			}
			copied, err := b.runner.Send(t.Context(), ref, m.ID, d)
			if err != nil {
				t.Fatal(err)
			}
			checkSFTPFile(t, d, copied.Key, true)
			// A different runner has only the remote copy and the recorded hash.
			b.gone(t, m.ID)
			got, err := b.runner.Fetch(t.Context(), ref, m.ID, copied.Key, m.SHA256, d)
			if err != nil || got.SHA256 != m.SHA256 || got.Size != m.Size {
				t.Fatalf("Fetch = %+v, %v", got, err)
			}
			write("changed since")
			if err := b.restore(source); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(file)
			if err != nil || !bytes.Contains(body, []byte(content)) {
				t.Fatalf("restored file = %q, %v", body, err)
			}
			if kept, err := b.runner.Delete(t.Context(), ref, m.ID, copied.Key, d); kept != "" || err != nil {
				t.Fatalf("Delete = %q, %v", kept, err)
			}
			checkSFTPFile(t, d, copied.Key, false)
			client, _, err := sftpstore.Open(t.Context(), *d.SFTP, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if f, err := client.Get(t.Context(), copied.Key+".json"); err == nil {
				_ = f.Close()
				t.Error("retention left the sealed manifest behind")
			}
		})
	}
}

func TestSFTPInterruptedCopyCanBeRetried(t *testing.T) {
	b := newBench(t)
	d, server := sftpDestination(t)
	body := make([]byte, 2<<20)
	rand.Read(body)
	b.write("notes.bin", string(body))
	m := b.take(false)
	want := d.key(m)
	server.CutWriteAt(32 << 10)
	if _, err := b.runner.Send(t.Context(), ref, m.ID, d); !errors.Is(err, sftpstore.ErrWrite) {
		t.Fatalf("interrupted Send = %v", err)
	}
	checkSFTPFile(t, d, want, false)
	if got := b.files(); !slices.Contains(got, m.ID+"."+m.Format) {
		t.Fatalf("lost the local copy: %v", got)
	}
	copied, err := b.runner.Send(t.Context(), ref, m.ID, d)
	if err != nil || copied.Key != want {
		t.Fatalf("retry Send = %+v, %v", copied, err)
	}
	checkSFTPFile(t, d, want, true)
	b.gone(t, m.ID)
	server.CutReadAt(32 << 10)
	if _, err := b.runner.Fetch(t.Context(), ref, m.ID, want, m.SHA256, d); !errors.Is(err, sftpstore.ErrRead) ||
		!strings.Contains(err.Error(), "fetch it again") {
		t.Fatalf("interrupted Fetch = %v", err)
	}
	if got := b.files(); len(got) != 0 {
		t.Fatalf("interrupted fetch left files: %v", got)
	}
	if _, err := b.runner.Fetch(t.Context(), ref, m.ID, want, m.SHA256, d); err != nil {
		t.Fatalf("retry Fetch = %v", err)
	}
}

func checkSFTPFile(t *testing.T, d *Destination, key string, exists bool) {
	t.Helper()
	client, _, err := sftpstore.Open(t.Context(), *d.SFTP, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	f, err := client.Get(t.Context(), key)
	if f != nil {
		_ = f.Close()
	}
	if (err == nil) != exists {
		t.Fatalf("Get %s = %v, want existence %t", key, err, exists)
	}
}

func TestSFTPOperationsRefuseChangedPin(t *testing.T) {
	b := newBench(t)
	d, server := sftpDestination(t)
	b.write("notes.txt", "a local backup")
	m := b.take(false)
	key := d.key(m)
	d.SFTP.Fingerprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	d.SFTP.HostKeyAlgorithm = "ssh-ed25519"
	if _, err := b.runner.Send(t.Context(), ref, m.ID, d); !errors.Is(err, sftpstore.ErrHostKeyChanged) {
		t.Fatalf("Send = %v", err)
	}
	b.gone(t, m.ID)
	if _, err := b.runner.Fetch(t.Context(), ref, m.ID, key, m.SHA256, d); !errors.Is(err, sftpstore.ErrHostKeyChanged) {
		t.Fatalf("Fetch = %v", err)
	}
	if kept, err := b.runner.Delete(t.Context(), ref, m.ID, key, d); err != nil || !strings.Contains(kept, server.Fingerprint()) {
		t.Fatalf("Delete = %q, %v", kept, err)
	}
	if got := server.AuthAttempts(); got != 0 {
		t.Fatalf("changed pin reached authentication %d times", got)
	}
}

func TestSFTPSweepsOnlyOldFailedBackupFiles(t *testing.T) {
	const user, password = "backups", "fixture-password"
	server, err := sftptest.ListenDisk(t.Context(), "127.0.0.1:0", sftptest.Login{User: user, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	host, port, err := net.SplitHostPort(server.Address())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	folder := "env_example/data"
	dir := filepath.Join(root, folder)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	failed := ids.At(ids.Backup, old, "failed")
	manifestOnly := ids.At(ids.Backup, old, "failed-manifest")
	unknown := ids.At(ids.Backup, old, "unknown")
	known := ids.At(ids.Backup, old, "known")
	recent := ids.At(ids.Backup, now, "recent")
	young := ids.At(ids.Backup, old, "young-file")
	d := DestinationFrom(map[string]string{
		"transport": "sftp", "host": host, "port": port, "user": user, "password": password,
		"path": root, "host_key_fingerprint": server.Fingerprint(),
	})
	d.sweepFolder = folder
	d.sweepFailedIDs = strings.Join([]string{failed, manifestOnly, recent, young}, "\n")
	d.sweepBefore = now.Add(-24 * time.Hour)
	for _, name := range []string{
		failed + ".tar.gz.sealed", failed + ".tar.gz.sealed.json",
		unknown + ".tar.gz.sealed", unknown + ".tar.gz.sealed.json",
		manifestOnly + ".sql.gz.sealed.json",
		known + ".tar.gz.sealed", known + ".tar.gz.sealed.json", recent + ".tar.gz.sealed",
		young + ".tar.gz.sealed", failed + ".other.sealed", "unrelated.tar.gz.sealed",
	} {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		mtime := old
		if strings.HasPrefix(name, young) {
			mtime = now
		}
		if err := os.Chtimes(file, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	client, _, err := sftpstore.Open(t.Context(), *d.SFTP, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	d.sweep(t.Context(), client, folder+"/"+known+".tar.gz.sealed")
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name())
	}
	want := []string{
		unknown + ".tar.gz.sealed", unknown + ".tar.gz.sealed.json",
		known + ".tar.gz.sealed", known + ".tar.gz.sealed.json", recent + ".tar.gz.sealed",
		young + ".tar.gz.sealed", failed + ".other.sealed", "unrelated.tar.gz.sealed",
	}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("sweep left %v, want %v", names, want)
	}
}
