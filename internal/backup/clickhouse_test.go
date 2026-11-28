// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

func TestClickHouseArchiveMustContainNativeMetadata(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Name: ".backup", Mode: 0o600, Size: 50}); err != nil {
		t.Fatal(err)
	}
	partial := gzipped(t, raw.Bytes())
	if err := verifyClickHouse(bytes.NewReader(partial)); err == nil {
		t.Fatal("an archive with truncated metadata read as whole")
	}
	if _, err := tw.Write(bytes.Repeat([]byte("x"), 50)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := verifyClickHouse(bytes.NewReader(gzipped(t, raw.Bytes()))); err != nil {
		t.Fatalf("whole archive: %v", err)
	}
}

func TestClickHouseBackupStagesStreamsAndCleansUp(t *testing.T) {
	b := newBench(t)
	const image = "clickhouse/clickhouse-server:25.8.33.6-alpine"
	var took, publicMetadata atomic.Bool
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Name: ".backup", Mode: 0o600, Size: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, "data"); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	b.engine.AddLocalImage(image, dockertest.Image{Config: docker.Config{Cmd: []string{"clickhouse-server"}}, Exec: func(x dockertest.Exec) int {
		cmd := strings.Join(x.Cmd, " ")
		dir := filepath.Join(x.Volumes[clickHouseData], "backups", "dockwell-stage")
		file := filepath.Join(dir, filepath.Base(x.Cmd[len(x.Cmd)-1]))
		switch {
		case strings.Contains(cmd, "select_from_system_db_requires_grant") && publicMetadata.Load():
			return 1
		case x.Cmd[0] == "rm":
			if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
				return 1
			}
		case strings.Contains(cmd, "exec cat"):
			data, err := os.ReadFile(file)
			if err != nil {
				return 1
			}
			_, _ = x.Stdout.Write(data)
		case strings.Contains(cmd, "--multiquery"):
			sql, _ := io.ReadAll(x.Stdin)
			if strings.HasPrefix(string(sql), "SELECT") {
				_, _ = io.WriteString(x.Stdout, "{\"name\":\"events\"}\n")
			} else {
				if !strings.Contains(string(sql), "BACKUP DATABASE `events`") || !strings.Contains(string(sql), "TABLE system.users") {
					_, _ = io.WriteString(x.Stderr, "Unexpected SQL")
					return 1
				}
				_, clause, ok := strings.Cut(string(sql), " TO File('dockwell-stage/")
				archive, settings, protected := strings.Cut(clause, "') SETTINGS password='")
				password, _, _ := strings.Cut(settings, "'")
				if !ok || !protected || len(password) < 32 || !strings.HasSuffix(archive, ".zip") || os.MkdirAll(dir, 0o700) != nil {
					return 1
				}
				var encrypted bytes.Buffer
				if err := clickHouseTarToZip(t.Context(), bytes.NewReader(raw.Bytes()), &encrypted, password); err != nil {
					return 1
				}
				if err := os.WriteFile(filepath.Join(dir, archive), encrypted.Bytes(), 0o600); err != nil {
					return 1
				}
				took.Store(true)
				_, _ = io.WriteString(x.Stdout, "fixture\tBACKUP_CREATED\n")
			}
		}
		return 0
	}})
	id := b.run("db", image, clickHouseData)
	publicMetadata.Store(true)
	if _, err := b.runner.Take(t.Context(), Take{Ref: ref, ID: "bkp_public_metadata"}); err == nil ||
		!strings.Contains(err.Error(), "select_from_system_db_requires_grant") || took.Load() {
		t.Fatalf("backup ran with public metadata or gave no diagnosis: %v", err)
	}
	publicMetadata.Store(false)
	disk := b.runner.Disk
	b.runner.Disk = NoRoom
	if _, err := b.runner.Take(t.Context(), Take{Ref: ref, ID: "bkp_no_room"}); err == nil ||
		!strings.Contains(err.Error(), "free") || took.Load() {
		t.Fatalf("insufficient disk ran the backup or gave no diagnosis: %v", err)
	}
	b.runner.Disk = disk
	got := b.take(false)
	if !took.Load() || got.Method != resource.BackupDump || got.Format != formatClickHouseDump || got.Unpacked != int64(raw.Len()) {
		t.Fatalf("native backup: %+v", got)
	}
	files, err := os.ReadDir(filepath.Join(b.engine.VolumeDir(b.target.Name), "backups", "dockwell-stage"))
	if err != nil || len(files) != 0 {
		t.Fatalf("stage survived the write: %v, %v", files, err)
	}
	if err := b.client.StopContainer(t.Context(), id, 0); err != nil {
		t.Fatal(err)
	}
	took.Store(false)
	got, err = b.runner.Take(t.Context(), Take{Ref: ref, ID: "bkp_cold_clickhouse"})
	if err != nil || took.Load() || got.Method != resource.BackupCopy || got.Format != formatArchive {
		t.Fatalf("stopped ClickHouse backup: %+v, %v", got, err)
	}
}
