// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

func TestDetectAndCheckImportFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file   string
		want   string
		wantOK bool
	}{
		{"backup.sql", ImportFormatSQL, true},
		{"backup.SQL.GZ", ImportFormatSQLGZ, true},
		{"prod.dump", ImportFormatDump, true},
		{"archive.tar", ImportFormatTar, true},
		{"bad.txt", "", false},
		{"../evil.sql", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, err := DetectImportFormat(tc.file)
		if (err == nil) != tc.wantOK || got != tc.want {
			t.Errorf("DetectImportFormat(%q) = (%q, %v), want (%q, ok=%t)", tc.file, got, err, tc.want, tc.wantOK)
		}
	}
	if err := CheckImportEngineFormat(dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL, ImportFormatDump); err != nil {
		t.Errorf("PG .dump refused: %v", err)
	}
	if err := CheckImportEngineFormat(dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL, ImportFormatTar); err != nil {
		t.Errorf("PG .tar refused: %v", err)
	}
	if err := CheckImportEngineFormat(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, ImportFormatDump); err == nil {
		t.Error("MySQL accepted .dump")
	}
	if err := CheckImportEngineFormat(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB, ImportFormatTar); err == nil {
		t.Error("MariaDB accepted .tar")
	}
	if err := CheckImportEngineFormat(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, ImportFormatSQLGZ); err != nil {
		t.Errorf("MySQL .sql.gz refused: %v", err)
	}
	if err := CheckImportEngineFormat(dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS, ImportFormatSQL); err == nil {
		t.Error("Redis accepted .sql")
	}
}

func TestStagedImportUploadResumeAndHeaders(t *testing.T) {
	t.Parallel()
	r := &Runner{Root: t.TempDir()}

	// Plain SQL chunked upload + resume + owner isolation.
	payload := []byte("CREATE TABLE t(id int);\nINSERT INTO t VALUES (1);\n")
	meta, err := r.BeginImport("session:s1", "", "svc_db", "shop", "seed.sql", "", uint64(len(payload)))
	if err != nil {
		t.Fatalf("BeginImport: %v", err)
	}
	// Another session cannot inspect or write to it.
	if _, err := r.BeginImport("session:s2", meta.ID, "", "", "", "", 0); err == nil {
		t.Fatal("other session resumed s1's upload")
	}
	// Write first half.
	half := len(payload) / 2
	sent := false
	meta, err = r.WriteImport(t.Context(), meta.ID, "session:s1", 0, func() ([]byte, error) {
		if sent {
			return nil, io.EOF
		}
		sent = true
		return payload[:half], nil
	})
	if err != nil || meta.Offset != uint64(half) || meta.Complete {
		t.Fatalf("first chunk = (%+v, %v)", meta, err)
	}
	// Wrong offset refused.
	if _, err := r.WriteImport(t.Context(), meta.ID, "session:s1", 0, func() ([]byte, error) { return nil, io.EOF }); err == nil {
		t.Fatal("offset mismatch was accepted")
	}
	// Inspect before complete refused.
	if _, err := r.InspectImport(meta.ID, "session:s1"); err == nil {
		t.Fatal("incomplete upload passed inspect")
	}
	// Resume from half.
	sent = false
	meta, err = r.WriteImport(t.Context(), meta.ID, "session:s1", uint64(half), func() ([]byte, error) {
		if sent {
			return nil, io.EOF
		}
		sent = true
		return payload[half:], nil
	})
	if err != nil || !meta.Complete || meta.Offset != uint64(len(payload)) {
		t.Fatalf("second chunk = (%+v, %v)", meta, err)
	}
	if _, err := r.InspectImport(meta.ID, "session:s1"); err != nil {
		t.Fatalf("InspectImport valid SQL: %v", err)
	}
	if err := r.CancelImport(meta.ID, "session:s1"); err != nil {
		t.Fatalf("CancelImport: %v", err)
	}
	if _, err := os.Stat(r.importDataPath(meta.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("CancelImport left part file behind: %v", err)
	}

	// Header checks: gzip in .sql rejected, invalid .dump rejected, valid .dump and .tar accepted.
	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	_, _ = gzw.Write(payload)
	_ = gzw.Close()

	badSQL, err := r.BeginImport("session:s1", "", "svc_db", "shop", "oops.sql", "", uint64(gzBuf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, badSQL.ID, "session:s1", gzBuf.Bytes())
	if _, err := r.InspectImport(badSQL.ID, "session:s1"); err == nil || !strings.Contains(err.Error(), "gzip-compressed") {
		t.Errorf("gzip bytes in .sql = %v, want gzip hint", err)
	}

	goodDump, err := r.BeginImport("session:s1", "", "svc_db", "shop", "pg.dump", "", 16)
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, goodDump.ID, "session:s1", []byte("PGDMP01234567890"))
	if _, err := r.InspectImport(goodDump.ID, "session:s1"); err != nil {
		t.Errorf("valid PGDMP .dump rejected: %v", err)
	}

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "toc.dat", Mode: 0o600, Size: 3})
	_, _ = tw.Write([]byte("toc"))
	_ = tw.Close()
	goodTar, err := r.BeginImport("session:s1", "", "svc_db", "shop", "pg.tar", "", uint64(tarBuf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, goodTar.ID, "session:s1", tarBuf.Bytes())
	if _, err := r.InspectImport(goodTar.ID, "session:s1"); err != nil {
		t.Errorf("valid .tar rejected: %v", err)
	}
}

func stageBytes(t *testing.T, r *Runner, importID, owner string, data []byte) {
	t.Helper()
	sent := false
	_, err := r.WriteImport(t.Context(), importID, owner, 0, func() ([]byte, error) {
		if sent {
			return nil, io.EOF
		}
		sent = true
		return data, nil
	})
	if err != nil {
		t.Fatalf("WriteImport: %v", err)
	}
}

func TestImportDumpBacksUpFirstRollsBackOnFailureAndUndoes(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	client := e.Client()

	var mu sync.Mutex
	content := "initial-shop-data"
	var cmds []string
	e.AddLocalImage("postgres:17", dockertest.Image{
		Volumes: []string{"/var/lib/postgresql/data"},
		Config:  docker.Config{Cmd: []string{"postgres"}},
		Exec: func(x dockertest.Exec) int {
			script := strings.Join(x.Cmd, " ")
			mu.Lock()
			cmds = append(cmds, script)
			mu.Unlock()
			switch {
			case strings.Contains(script, "pg_dump "):
				mu.Lock()
				cur := content
				mu.Unlock()
				_, _ = io.WriteString(x.Stdout, cur+"\n-- PostgreSQL database dump complete\n")
				return 0
			case strings.Contains(script, "pg_restore"):
				b, _ := io.ReadAll(x.Stdin)
				mu.Lock()
				content = string(b)
				mu.Unlock()
				return 0
			case strings.Contains(script, "psql"):
				b, _ := io.ReadAll(x.Stdin)
				sql := string(b)
				if strings.Contains(sql, "SELECT 1 FROM pg_database WHERE datname = 'shop'") {
					_, _ = io.WriteString(x.Stdout, "1\n")
					return 0
				}
				if strings.Contains(sql, "pg_terminate_backend") {
					return 0
				}
				if strings.Contains(sql, "BROKEN_SQL") {
					_, _ = io.WriteString(x.Stderr, "ERROR:  syntax error at or near \"BROKEN_SQL\"\nLINE 1: BROKEN_SQL;\n")
					return 3
				}
				mu.Lock()
				content = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), "-- PostgreSQL database dump complete"))
				mu.Unlock()
				return 0
			}
			return 0
		},
	})

	ref := resource.Ref{Kind: "volume", Scope: "env_1", Name: "data"}
	volName := volume.TargetOf(ref).Name
	if _, err := client.CreateVolume(t.Context(), &docker.VolumeCreate{Name: volName}); err != nil {
		t.Fatal(err)
	}
	cid, err := client.CreateContainer(t.Context(), "dockwell-env_1-db", &docker.CreateRequest{
		Config: docker.Config{
			Image: "postgres:17",
			Labels: map[string]string{
				docker.LabelProject: "env_1",
				docker.LabelService: "db",
			},
		},
		HostConfig: docker.HostConfig{
			Mounts: []docker.Mount{{Type: "volume", Source: volName, Target: "/var/lib/postgresql/data"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), cid); err != nil {
		t.Fatal(err)
	}

	r := &Runner{Client: client, Root: t.TempDir()}

	// 1. Failing import reports the tool's last lines AND rolls back to pre-import backup.
	badSQL := []byte("BROKEN_SQL;")
	badMeta, err := r.BeginImport("session:s1", "", "svc_db", "shop", "broken.sql", "", uint64(len(badSQL)))
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, badMeta.ID, "session:s1", badSQL)
	badSrc := resource.RestoreSource{
		Change:         "chg_fail",
		ImportID:       badMeta.ID,
		ImportDatabase: "shop",
		ImportFilename: "broken.sql",
		ImportFormat:   ImportFormatSQL,
	}
	dec, left, err := r.JudgeRestore(t.Context(), ref, badSrc)
	if err != nil || !left || dec.Restore == nil || dec.Restore.ImportFile != "broken.sql" || dec.Restore.OneDatabase != "shop" {
		t.Fatalf("JudgeRestore(bad) = (%+v, %t, %v)", dec, left, err)
	}
	var progress []string
	ctx := resource.WithActivities(t.Context(), func(a resource.Activity) {
		if a.Message != "" {
			progress = append(progress, a.Message)
		}
	})
	err = r.ApplyRestore(ctx, resource.Step{Ref: ref, Decision: dec}, badSrc)
	if err == nil || !strings.Contains(err.Error(), `syntax error at or near "BROKEN_SQL"`) || !strings.Contains(err.Error(), "What was in shop before the import is back") {
		t.Fatalf("failed import error = %v, want tool last lines + rollback notice", err)
	}
	mu.Lock()
	afterFail := content
	mu.Unlock()
	if afterFail != "initial-shop-data" {
		t.Errorf("after failed import, shop content = %q, want initial-shop-data restored", afterFail)
	}
	if len(progress) < 2 {
		t.Errorf("expected progress updates, got %v", progress)
	}

	// 2. Successful .sql.gz import updates shop, removes staged file, and Undo puts initial-shop-data back.
	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	_, _ = gzw.Write([]byte("imported-new-rows"))
	_ = gzw.Close()
	goodMeta, err := r.BeginImport("session:s1", "", "svc_db", "shop", "fresh.sql.gz", "", uint64(gzBuf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, goodMeta.ID, "session:s1", gzBuf.Bytes())
	goodSrc := resource.RestoreSource{
		Change:         "chg_ok",
		ImportID:       goodMeta.ID,
		ImportDatabase: "shop",
		ImportFilename: "fresh.sql.gz",
		ImportFormat:   ImportFormatSQLGZ,
	}
	dec, left, err = r.JudgeRestore(t.Context(), ref, goodSrc)
	if err != nil || !left {
		t.Fatalf("JudgeRestore(good) = (%+v, %t, %v)", dec, left, err)
	}
	if err := r.ApplyRestore(context.Background(), resource.Step{Ref: ref, Decision: dec}, goodSrc); err != nil {
		t.Fatalf("ApplyRestore(good): %v", err)
	}
	mu.Lock()
	afterGood := content
	mu.Unlock()
	if afterGood != "imported-new-rows" {
		t.Errorf("after good import, shop content = %q, want imported-new-rows", afterGood)
	}
	if _, err := os.Stat(r.importDataPath(goodMeta.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staged import file was not cleaned up after success: %v", err)
	}

	// 3. Undo restores the pre-import backup taken before chg_ok.
	undoSrc := resource.RestoreSource{Undoes: "chg_ok", Change: "chg_undo"}
	undoDec, left, err := r.JudgeRestore(t.Context(), ref, undoSrc)
	if err != nil || !left || undoDec.Restore == nil || undoDec.Restore.OneDatabase != "shop" {
		t.Fatalf("JudgeRestore(undo) = (%+v, %t, %v)", undoDec, left, err)
	}
	if err := r.ApplyRestore(t.Context(), resource.Step{Ref: ref, Decision: undoDec}, undoSrc); err != nil {
		t.Fatalf("ApplyRestore(undo): %v", err)
	}
	mu.Lock()
	afterUndo := content
	mu.Unlock()
	if afterUndo != "initial-shop-data" {
		t.Errorf("after undo, shop content = %q, want initial-shop-data", afterUndo)
	}
}
