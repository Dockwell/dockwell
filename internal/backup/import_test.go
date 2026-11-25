// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestScanSQLDumpRefusesDangerousCommandsAndAllowsRealDumps(t *testing.T) {
	t.Parallel()

	pg := dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL
	my := dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB

	// Real PostgreSQL pg_dump output (including \restrict/\unrestrict, COPY FROM stdin
	// containing \! inside data rows, and string literals mentioning CREATE ROLE) passes.
	validPG := strings.Join([]string{
		`\restrict abcdef123456`,
		`-- PostgreSQL database dump`,
		`SET statement_timeout = 0;`,
		`CREATE TABLE public.notes (id int, body text);`,
		`INSERT INTO public.notes VALUES (1, 'CREATE ROLE admin; \! rm -rf /');`,
		`COPY public.notes (id, body) FROM stdin;`,
		`2	\! echo inside copy data is harmless`,
		`3	CREATE ROLE inside copy data`,
		`\.`,
		`\unrestrict abcdef123456`,
		`-- PostgreSQL database dump complete`,
	}, "\n") + "\n"
	if err := scanSQLDump(pg, strings.NewReader(validPG)); err != nil {
		t.Errorf("valid PG dump refused: %v", err)
	}

	pgRefusals := []struct {
		name     string
		sql      string
		wantLine string
		wantFact string
	}{
		{"shell escape", "SELECT 1;\n\\! id\n", "line 2", "shell command"},
		{"connect switch", "-- ok\n\n\\connect other\n", "line 3", "switches databases"},
		{"short connect", "\\c other\n", "line 1", "switches databases"},
		{"client copy", "SELECT 1;\n\\copy t TO PROGRAM 'id'\n", "line 2", `\copy`},
		{"other meta", "\\i /etc/passwd\n", "line 1", "psql meta-command"},
		{"copy to program", "SELECT 1;\nCOPY t TO PROGRAM 'id';\n", "line 2", "COPY TO PROGRAM"},
		{"copy from program", "COPY t FROM PROGRAM 'cat /etc/passwd';\n", "line 1", "COPY FROM PROGRAM"},
		{"create role", "CREATE ROLE evil SUPERUSER;\n", "line 1", "database roles"},
		{"alter user", "SELECT 1;\nALTER USER postgres PASSWORD 'x';\n", "line 2", "database roles"},
		{"drop database", "DROP DATABASE other;\n", "line 1", "modifies a database"},
		{"pg_dumpall header", "--\n-- PostgreSQL database cluster dump\n--\n", "line 2", "pg_dumpall"},
		{"mismatched unrestrict", "\\restrict k1\n\\unrestrict wrong\n", "line 2", "psql meta-command"},
	}
	for _, tc := range pgRefusals {
		err := scanSQLDump(pg, strings.NewReader(tc.sql))
		if err == nil {
			t.Errorf("PG %s: accepted, want refusal", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantLine) || !strings.Contains(err.Error(), tc.wantFact) {
			t.Errorf("PG %s: err = %q, want %q and %q", tc.name, err.Error(), tc.wantLine, tc.wantFact)
		}
	}

	// Real MariaDB/MySQL dump (including MariaDB 11 sandbox pragma, DELIMITER, versioned comments,
	// and a column literally named `system` inside CREATE TABLE) passes.
	validMy := strings.Join([]string{
		`/*M!999999\- enable the sandbox mode */`,
		`/*!999999\- enable the sandbox mode */`,
		`/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;`,
		`CREATE TABLE items (`,
		`  id INT PRIMARY KEY,`,
		`  system VARCHAR(64),`,
		`  note TEXT`,
		`);`,
		`INSERT INTO items VALUES (1, 'ok', 'USE mysql; CREATE USER x;');`,
		`DELIMITER ;;`,
		`CREATE TRIGGER t1 AFTER INSERT ON items FOR EACH ROW BEGIN SET @x = 1; END;;`,
		`DELIMITER ;`,
		`-- Dump completed on 2026-11-24`,
	}, "\n") + "\n"
	if err := scanSQLDump(my, strings.NewReader(validMy)); err != nil {
		t.Errorf("valid MySQL/MariaDB dump refused: %v", err)
	}

	myRefusals := []struct {
		name     string
		sql      string
		wantLine string
		wantFact string
	}{
		{"system shell", "SELECT 1;\nsystem id\n", "line 2", "shell command"},
		{"backslash shell", "\\! id\n", "line 1", "shell command"},
		{"source file", "source /etc/passwd\n", "line 1", "external file"},
		{"backslash source", "\\. /etc/passwd\n", "line 1", "external file"},
		{"use db", "SELECT 1;\nUSE other_db;\n", "line 2", "switches databases"},
		{"create database", "CREATE DATABASE other;\n", "line 1", "modifies a database"},
		{"create user", "CREATE USER 'evil'@'%' IDENTIFIED BY 'x';\n", "line 1", "users or roles"},
		{"versioned create user", "/*!40101 CREATE USER 'evil'@'%' */;\n", "line 1", "users or roles"},
		{"grant", "GRANT ALL ON *.* TO 'app'@'%';\n", "line 1", "privileges"},
		{"into outfile", "SELECT 1 INTO OUTFILE '/tmp/pwn';\n", "line 1", "OUTFILE"},
		{"load data", "LOAD DATA LOCAL INFILE '/etc/passwd' INTO TABLE t;\n", "line 1", "LOAD DATA"},
		{"load_file", "SELECT LOAD_FILE('/etc/passwd');\n", "line 1", "LOAD_FILE"},
		{"mysql.user write", "UPDATE `mysql`.`user` SET password = 'x';\n", "line 1", "mysql.user"},
		{"all-databases header", "--\n-- Current Database: `other`\n--\n", "line 2", "Current Database"},
	}
	for _, tc := range myRefusals {
		err := scanSQLDump(my, strings.NewReader(tc.sql))
		if err == nil {
			t.Errorf("MySQL %s: accepted, want refusal", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantLine) || !strings.Contains(err.Error(), tc.wantFact) {
			t.Errorf("MySQL %s: err = %q, want %q and %q", tc.name, err.Error(), tc.wantLine, tc.wantFact)
		}
	}
}

func TestSanitizeImportErrorDropsInputLinesAndSecrets(t *testing.T) {
	t.Parallel()
	pgRaw := strings.Join([]string{
		`psql:<stdin>:42: ERROR:  duplicate key value violates unique constraint "users_email_key"`,
		`DETAIL:  Key (email)=(alice@secret.example) already exists.`,
		`CONTEXT:  COPY users, line 3`,
		`LINE 1: INSERT INTO users VALUES ('alice@secret.example', 'super-secret-token-999');`,
		`                                                          ^`,
	}, "\n")
	pgClean := sanitizeImportError(pgRaw)
	if !strings.Contains(pgClean, "ERROR:") || !strings.Contains(pgClean, "duplicate key value") {
		t.Errorf("PG error message lost: %q", pgClean)
	}
	for _, leak := range []string{"alice@secret.example", "super-secret-token-999", "LINE 1:", "DETAIL:", "CONTEXT:"} {
		if strings.Contains(pgClean, leak) {
			t.Errorf("PG sanitized error leaked %q: %q", leak, pgClean)
		}
	}

	myRaw := `ERROR 1064 (42000) at line 12: You have an error in your SQL syntax; check the manual that corresponds to your MariaDB server version for the right syntax to use near 'INSERT INTO secrets VALUES ('my-secret-pw-42')' at line 12`
	myClean := sanitizeImportError(myRaw)
	if !strings.Contains(myClean, "ERROR 1064") || !strings.Contains(myClean, "at line 12") {
		t.Errorf("MySQL error message lost: %q", myClean)
	}
	if strings.Contains(myClean, "my-secret-pw-42") || strings.Contains(myClean, "INSERT INTO secrets") {
		t.Errorf("MySQL sanitized error leaked dump content: %q", myClean)
	}

	// Non-error raw echo is withheld rather than leaked.
	if got := sanitizeImportError("INSERT INTO t VALUES ('raw-secret');\n"); strings.Contains(got, "raw-secret") {
		t.Errorf("unrecognized stderr leaked raw content: %q", got)
	}
}

func TestStagedImportUploadResumeCancelSweepAndDiskCheck(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	var freeBytes int64 = 1 << 30
	var reserveBytes int64 = 100 << 20
	r := &Runner{
		Root: t.TempDir(),
		Now:  func() time.Time { return now },
		Disk: func() disk { return disk{free: freeBytes, reserve: reserveBytes} },
	}

	// Plain SQL chunked upload + resume + owner isolation.
	payload := []byte("CREATE TABLE t(id int);\nINSERT INTO t VALUES (1);\n")
	meta, err := r.BeginImport("user:u1", "", "svc_db", "shop", "seed.sql", "", uint64(len(payload)), 0)
	if err != nil {
		t.Fatalf("BeginImport: %v", err)
	}
	// Another user cannot resume u1's upload.
	if _, err := r.BeginImport("user:u2", meta.ID, "", "", "", "", 0, 0); err == nil {
		t.Fatal("other user resumed u1's upload")
	}
	// Write first half.
	half := len(payload) / 2
	sent := false
	meta, err = r.WriteImport(t.Context(), meta.ID, "user:u1", 0, func() ([]byte, error) {
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
	if _, err := r.WriteImport(t.Context(), meta.ID, "user:u1", 0, func() ([]byte, error) { return nil, io.EOF }); err == nil {
		t.Fatal("offset mismatch was accepted")
	}
	// Inspect before complete refused.
	if _, err := r.InspectImport(meta.ID, "user:u1"); err == nil {
		t.Fatal("incomplete upload passed inspect")
	}
	// Resume from half.
	sent = false
	meta, err = r.WriteImport(t.Context(), meta.ID, "user:u1", uint64(half), func() ([]byte, error) {
		if sent {
			return nil, io.EOF
		}
		sent = true
		return payload[half:], nil
	})
	if err != nil || !meta.Complete || meta.Offset != uint64(len(payload)) {
		t.Fatalf("second chunk = (%+v, %v)", meta, err)
	}
	if _, err := r.InspectImport(meta.ID, "user:u1"); err != nil {
		t.Fatalf("InspectImport valid SQL: %v", err)
	}
	// Admin cancel (empty owner) removes any user's upload.
	if err := r.CancelImport(meta.ID, ""); err != nil {
		t.Fatalf("CancelImport(admin): %v", err)
	}
	if _, err := os.Stat(r.importDataPath(meta.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("CancelImport left part file behind: %v", err)
	}

	// M3: Cancelling while WriteImport is actively uploading succeeds (no upload_busy)
	// and removes files once the writer exits.
	busyMeta, err := r.BeginImport("user:u1", "", "svc_db", "shop", "busy.sql", "", 1024, 0)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	writeErr := make(chan error, 1)
	go func() {
		first := true
		_, werr := r.WriteImport(t.Context(), busyMeta.ID, "user:u1", 0, func() ([]byte, error) {
			if first {
				first = false
				close(started)
				return []byte("SELECT 1;\n"), nil
			}
			<-release
			return nil, errors.New("client aborted")
		})
		writeErr <- werr
	}()
	<-started
	if err := r.CancelImport(busyMeta.ID, "user:u1"); err != nil {
		t.Fatalf("CancelImport while busy = %v, want nil", err)
	}
	close(release)
	if err := <-writeErr; err == nil {
		t.Error("WriteImport after cancel succeeded, want cancelled error")
	}
	if _, err := os.Stat(r.importDataPath(busyMeta.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("busy cancelled upload left part file behind: %v", err)
	}

	// M3: Free-space check counts staged uploads + upload + db backup + restored size + reserve.
	freeBytes = 300 << 20
	reserveBytes = 100 << 20
	// Remaining usable is 200 MiB. A 60 MiB .sql.gz needs 60 (upload) + 180 (3x restored) = 240 MiB -> refused.
	if _, err := r.BeginImport("user:u1", "", "svc_db", "shop", "big.sql.gz", "", 60<<20, 0); err == nil || !strings.Contains(err.Error(), "disk space") {
		t.Errorf("oversized .sql.gz = %v, want no_space", err)
	}
	// A 50 MiB .sql with 120 MiB target DB needs 50 + 50 + 120 = 220 MiB -> refused.
	if _, err := r.BeginImport("user:u1", "", "svc_db", "shop", "medium.sql", "", 50<<20, 120<<20); err == nil || !strings.Contains(err.Error(), "disk space") {
		t.Errorf("medium .sql with large DB = %v, want no_space", err)
	}
	// Two concurrent begins summing past free space: first 80 MiB .sql (needs 160 MiB) passes,
	// second 80 MiB .sql sees 80 MiB unwritten staged + 160 MiB = 240 MiB -> refused!
	firstStaged, err := r.BeginImport("user:u1", "", "svc_db", "shop", "first.sql", "", 80<<20, 0)
	if err != nil {
		t.Fatalf("first 80 MiB begin: %v", err)
	}
	if _, err := r.BeginImport("user:u1", "", "svc_db", "shop", "second.sql", "", 80<<20, 0); err == nil || !strings.Contains(err.Error(), "disk space") {
		t.Errorf("second parallel begin = %v, want no_space accounting for staged upload", err)
	}

	// M3: SweepImports removes idle incomplete uploads after 1 h and orphan .part files,
	// while keeping completed uploads for 24 h.
	freeBytes = 1 << 30
	doneMeta, err := r.BeginImport("user:u1", "", "svc_db", "shop", "done.sql", "", uint64(len(payload)), 0)
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, doneMeta.ID, payload)
	orphanPath := filepath.Join(r.importsDir(), "imp_orphan1234567890.part")
	if err := os.WriteFile(orphanPath, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Hour)
	r.SweepImports()
	if _, err := os.Stat(r.importDataPath(firstStaged.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("idle incomplete upload was not swept after 2h")
	}
	if _, err := os.Stat(orphanPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphan .part file was not swept")
	}
	if _, err := os.Stat(r.importDataPath(doneMeta.ID)); err != nil {
		t.Errorf("completed upload was swept too early (at 2h): %v", err)
	}
	now = now.Add(24 * time.Hour)
	r.SweepImports()
	if _, err := os.Stat(r.importDataPath(doneMeta.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("completed upload was not swept after 26h")
	}

	// Header checks: gzip in .sql rejected, valid .dump and .tar accepted.
	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	_, _ = gzw.Write(payload)
	_ = gzw.Close()

	badSQL, err := r.BeginImport("user:u1", "", "svc_db", "shop", "oops.sql", "", uint64(gzBuf.Len()), 0)
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, badSQL.ID, gzBuf.Bytes())
	if _, err := r.InspectImport(badSQL.ID, "user:u1"); err == nil || !strings.Contains(err.Error(), "gzip-compressed") {
		t.Errorf("gzip bytes in .sql = %v, want gzip hint", err)
	}

	goodDump, err := r.BeginImport("user:u1", "", "svc_db", "shop", "pg.dump", "", 16, 0)
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, goodDump.ID, []byte("PGDMP01234567890"))
	if _, err := r.InspectImport(goodDump.ID, "user:u1"); err != nil {
		t.Errorf("valid PGDMP .dump rejected: %v", err)
	}

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	_ = tw.WriteHeader(&tar.Header{Name: "toc.dat", Mode: 0o600, Size: 3})
	_, _ = tw.Write([]byte("toc"))
	_ = tw.Close()
	goodTar, err := r.BeginImport("user:u1", "", "svc_db", "shop", "pg.tar", "", uint64(tarBuf.Len()), 0)
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, goodTar.ID, tarBuf.Bytes())
	if _, err := r.InspectImport(goodTar.ID, "user:u1"); err != nil {
		t.Errorf("valid .tar rejected: %v", err)
	}
}

func stageBytes(t *testing.T, r *Runner, importID string, data []byte) {
	t.Helper()
	sent := false
	_, err := r.WriteImport(t.Context(), importID, "user:u1", 0, func() ([]byte, error) {
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

func TestImportDumpOwnerLockoutRollbackEmptyAndUndo(t *testing.T) {
	t.Parallel()
	e := dockertest.New(t)
	client := e.Client()

	var mu sync.Mutex
	content := "CREATE TABLE initial_tbl (id int);"
	ownerLockedOut := false
	lockoutCount := 0
	letInCount := 0
	emptiedCount := 0
	var loadUsers []string
	var loadSQLs []string

	e.AddLocalImage("postgres:17", dockertest.Image{
		Volumes: []string{"/var/lib/postgresql/data"},
		Config:  docker.Config{Cmd: []string{"postgres"}},
		Exec: func(x dockertest.Exec) int {
			script := strings.Join(x.Cmd, " ")
			switch {
			case len(x.Cmd) >= 2 && x.Cmd[0] == "psql" && x.Cmd[1] == "-V":
				_, _ = io.WriteString(x.Stdout, "psql (PostgreSQL) 17.6\n")
				return 0
			case strings.Contains(script, " -w ") && strings.Contains(script, "SELECT 1"):
				// trustsAnyone check: refuse passwordless network sign-in.
				_, _ = io.WriteString(x.Stderr, "psql: error: fe_sendauth: no password supplied\n")
				return 2
			case strings.Contains(script, "pg_dump "):
				mu.Lock()
				cur := content
				mu.Unlock()
				// Verify dump runs as loader/owner, never superuser postgres (H2).
				if strings.Contains(script, "-U postgres") {
					t.Errorf("pg_dump ran as superuser: %s", script)
				}
				_, _ = io.WriteString(x.Stdout, cur+"\n-- PostgreSQL database dump complete\n")
				return 0
			case len(x.Cmd) > 0 && x.Cmd[0] == "pg_restore":
				// PGArchiveToSQL: emit SQL to stdout.
				b, _ := io.ReadAll(x.Stdin)
				sql := strings.TrimPrefix(string(b), "PGDMP")
				_, _ = io.WriteString(x.Stdout, sql)
				return 0
			case strings.Contains(script, `-d "$d"`):
				// engines.Loader.Load running in throwaway container as dw_restore_* + SET ROLE "shop".
				b, _ := io.ReadAll(x.Stdin)
				sql := string(b)
				user := ""
				if len(x.Cmd) > 4 {
					user = x.Cmd[4]
				}
				mu.Lock()
				loadUsers = append(loadUsers, user)
				loadSQLs = append(loadSQLs, sql)
				if strings.Contains(sql, "$dw_empty$") {
					emptiedCount++
				}
				mu.Unlock()
				if strings.Contains(sql, "BROKEN_SQL") {
					_, _ = io.WriteString(x.Stderr, "psql:<stdin>:1: ERROR:  syntax error at or near \"BROKEN_SQL\"\nLINE 1: BROKEN_SQL 'secret-row-value';\n")
					return 3
				}
				// Extract actual table statement from loaded SQL.
				for line := range strings.SplitSeq(sql, "\n") {
					if strings.HasPrefix(line, "CREATE TABLE ") {
						mu.Lock()
						content = line
						mu.Unlock()
					}
				}
				return 0
			case strings.Contains(script, "psql"):
				b, _ := io.ReadAll(x.Stdin)
				sql := string(b)
				switch {
				case strings.Contains(sql, "SELECT 1 FROM pg_database WHERE datname = 'shop'"):
					_, _ = io.WriteString(x.Stdout, "1\n")
					return 0
				case strings.Contains(sql, "'login', r.rolcanlogin"):
					_, _ = io.WriteString(x.Stdout, `{"owner":"shop","login":true,"super":false,"loader":""}`+"\n")
					return 0
				case strings.Contains(sql, "FROM pg_extension x WHERE x.oid >= 16384"):
					// pgOwnsAll: everything belongs to owner.
					return 0
				case strings.Contains(sql, `ALTER ROLE "shop" NOLOGIN`):
					mu.Lock()
					ownerLockedOut = true
					lockoutCount++
					mu.Unlock()
					return 0
				case strings.Contains(sql, `ALTER ROLE "shop" LOGIN`):
					mu.Lock()
					ownerLockedOut = false
					letInCount++
					mu.Unlock()
					return 0
				}
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

	// 0. Dangerous dump (\!) is refused at JudgeRestore before touching the DB (H1).
	evilSQL := []byte("SELECT 1;\n\\! touch /tmp/pwned\n")
	evilMeta, err := r.BeginImport("user:u1", "", "svc_db", "shop", "evil.sql", "", uint64(len(evilSQL)), 0)
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, evilMeta.ID, evilSQL)
	if _, _, err := r.JudgeRestore(t.Context(), ref, resource.RestoreSource{
		Change: "chg_evil", ImportID: evilMeta.ID, ImportDatabase: "shop", ImportFilename: "evil.sql", ImportFormat: ImportFormatSQL,
	}); err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "shell command") {
		t.Fatalf("JudgeRestore(evil) = %v, want line 2 shell command refusal", err)
	}

	// 0b. Dangerous .dump archive (COPY TO PROGRAM) is also caught via PGArchiveToSQL + scanSQLDump (H1).
	evilDump := []byte("PGDMPSELECT 1;\nCOPY x TO PROGRAM 'id';\n")
	evilDumpMeta, err := r.BeginImport("user:u1", "", "svc_db", "shop", "evil.dump", "", uint64(len(evilDump)), 0)
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, evilDumpMeta.ID, evilDump)
	if _, _, err := r.JudgeRestore(t.Context(), ref, resource.RestoreSource{
		Change: "chg_evildump", ImportID: evilDumpMeta.ID, ImportDatabase: "shop", ImportFilename: "evil.dump", ImportFormat: ImportFormatDump,
	}); err == nil || !strings.Contains(err.Error(), "COPY TO PROGRAM") {
		t.Fatalf("JudgeRestore(evil.dump) = %v, want COPY TO PROGRAM refusal", err)
	}

	// 1. Failing import reports sanitized error (no secret row value, no LINE 1:),
	// rolls back with PgEmptying as owner, and lets the app back in (H1, H2, M1, M2, M4).
	badSQL := []byte("BROKEN_SQL 'secret-row-value';\n")
	badMeta, err := r.BeginImport("user:u1", "", "svc_db", "shop", "broken.sql", "", uint64(len(badSQL)), 0)
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, badMeta.ID, badSQL)
	badSrc := resource.RestoreSource{
		Change: "chg_fail", ImportID: badMeta.ID, ImportDatabase: "shop", ImportFilename: "broken.sql", ImportFormat: ImportFormatSQL,
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
	if err == nil || !strings.Contains(err.Error(), "syntax error at or near") || !strings.Contains(err.Error(), "What was in shop before the import is back") {
		t.Fatalf("failed import error = %v, want sanitized error + rollback notice", err)
	}
	if strings.Contains(err.Error(), "secret-row-value") || strings.Contains(err.Error(), "LINE 1:") {
		t.Errorf("failed import error leaked dump input: %v", err)
	}
	mu.Lock()
	if ownerLockedOut {
		t.Error("owner still locked out after failed import")
	}
	if lockoutCount == 0 || letInCount == 0 {
		t.Errorf("lockout=%d letIn=%d, want both > 0", lockoutCount, letInCount)
	}
	if emptiedCount == 0 {
		t.Error("rollback did not run PgEmptying before restoring backup (M2)")
	}
	afterFail := content
	mu.Unlock()
	if afterFail != "CREATE TABLE initial_tbl (id int);" {
		t.Errorf("after failed import, content = %q, want initial_tbl restored", afterFail)
	}
	if len(progress) < 2 {
		t.Errorf("expected progress updates, got %v", progress)
	}

	// 2. Successful .sql.gz import loads as owner (SET ROLE "shop" + SET lock_timeout + \restrict),
	// lets app back in, removes staged file, and Undo goes through restoreOne (emptying + owner restore).
	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	_, _ = gzw.Write([]byte("CREATE TABLE imported_tbl (id int);\n"))
	_ = gzw.Close()
	goodMeta, err := r.BeginImport("user:u1", "", "svc_db", "shop", "fresh.sql.gz", "", uint64(gzBuf.Len()), 0)
	if err != nil {
		t.Fatal(err)
	}
	stageBytes(t, r, goodMeta.ID, gzBuf.Bytes())
	goodSrc := resource.RestoreSource{
		Change: "chg_ok", ImportID: goodMeta.ID, ImportDatabase: "shop", ImportFilename: "fresh.sql.gz", ImportFormat: ImportFormatSQLGZ,
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
	stillOut := ownerLockedOut
	mu.Unlock()
	if afterGood != "CREATE TABLE imported_tbl (id int);" {
		t.Errorf("after good import, content = %q, want imported_tbl", afterGood)
	}
	if stillOut {
		t.Error("owner still locked out after successful import")
	}
	if _, err := os.Stat(r.importDataPath(goodMeta.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staged import file was not cleaned up after success: %v", err)
	}

	// Verify every load used dw_restore_* loader role (never postgres), SET ROLE "shop",
	// and lock_timeout on import loads (H1, H2, M1).
	mu.Lock()
	for i, u := range loadUsers {
		if !strings.HasPrefix(u, "dw_restore_") {
			t.Errorf("load #%d ran as %q, want dw_restore_* loader role", i, u)
		}
		if !strings.Contains(loadSQLs[i], `SET ROLE "shop";`) {
			t.Errorf("load #%d missing SET ROLE \"shop\"", i)
		}
	}
	foundTimeout := false
	for _, s := range loadSQLs {
		if strings.Contains(s, "SET lock_timeout = '30s';") && strings.Contains(s, `\restrict `) {
			foundTimeout = true
			break
		}
	}
	mu.Unlock()
	if !foundTimeout {
		t.Error("import load missing lock_timeout or \\restrict")
	}

	// 3. Undo restores the pre-import backup via restoreOne (emptying + owner restore).
	undoSrc := resource.RestoreSource{Undoes: "chg_ok", Change: "chg_undo"}
	undoDec, left, err := r.JudgeRestore(t.Context(), ref, undoSrc)
	if err != nil || !left || undoDec.Restore == nil || undoDec.Restore.OneDatabase != "shop" {
		t.Fatalf("JudgeRestore(undo) = (%+v, %t, %v)", undoDec, left, err)
	}
	beforeEmpties := emptiedCount
	if err := r.ApplyRestore(t.Context(), resource.Step{Ref: ref, Decision: undoDec}, undoSrc); err != nil {
		t.Fatalf("ApplyRestore(undo): %v", err)
	}
	mu.Lock()
	afterUndo := content
	undoEmptied := emptiedCount > beforeEmpties
	undoStillOut := ownerLockedOut
	mu.Unlock()
	if afterUndo != "CREATE TABLE initial_tbl (id int);" {
		t.Errorf("after undo, content = %q, want initial_tbl", afterUndo)
	}
	if !undoEmptied {
		t.Error("Undo did not empty the database before restoring backup (M2)")
	}
	if undoStillOut {
		t.Error("owner still locked out after Undo")
	}
}

func TestMySQLFilePasswordCredsInDatabaseKinds(t *testing.T) {
	t.Parallel()
	// L7: MySQL and MariaDB containers configured with *_ROOT_PASSWORD_FILE are recognised
	// and their commands read the _FILE variable.
	for _, tc := range []struct {
		image  string
		envVar string
		want   string
	}{
		{"mysql:8.4", "MYSQL_ROOT_PASSWORD_FILE=/run/secrets/root_pw", "MySQL"},
		{"mariadb:11", "MARIADB_ROOT_PASSWORD_FILE=/run/secrets/root_pw", "MariaDB"},
	} {
		c := &docker.Container{
			Config: docker.Config{
				Image: tc.image,
				Env:   []string{tc.envVar},
			},
			Mounts: []docker.MountPoint{{Name: "vol_1", Destination: "/var/lib/mysql"}},
		}
		k := kindOf(c, "vol_1")
		if k == nil || k.name != tc.want {
			t.Errorf("kindOf(%s with %s) = %v, want %s", tc.image, tc.envVar, k, tc.want)
			continue
		}
		for _, script := range [][]string{k.dump, k.dumpOne, k.load, k.ready} {
			joined := strings.Join(script, " ")
			if !strings.Contains(joined, "MYSQL_ROOT_PASSWORD_FILE") {
				t.Errorf("%s command missing _FILE support: %s", tc.want, joined)
			}
		}
	}
	// Also verify checkLogicalExists snippet uses _FILE.
	if !strings.Contains(myAdminShell, "MARIADB_ROOT_PASSWORD_FILE") || !strings.Contains(pgAdminShell, "POSTGRES_PASSWORD_FILE") {
		t.Errorf("admin password snippets missing _FILE support")
	}
	_ = fmt.Sprintf // keep fmt imported if needed
}
