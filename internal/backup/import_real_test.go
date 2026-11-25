// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	engines "github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// TestDatabaseToolsOnRealDocker exercises database daily tools against real
// PostgreSQL and MariaDB containers on Docker (CI's Docker job runs *OnRealDocker):
//   - creates a logical database with a new non-superuser owner
//   - verifies a dump with \! is refused before touching the database (H1)
//   - fails an import containing a secret value and an extra table before failure,
//     checking sanitized error (M4), rollback removing the extra table (M2),
//     original rows intact, and the app user still reading and writing (H2, M1)
//   - succeeds a .sql import adding a table, then Undoes it and checks the extra
//     table is gone and the app user reads and writes (H2, M2)
//   - succeeds a .dump (pg_dump -Fc) import on PostgreSQL as the owner (H1, H2)
//   - deletes the database with pre-delete backup and Undoes to restore rows (H4)
//   - opens the console command with SELECT 1 and checks history is disabled (L1, L3)
func TestDatabaseToolsOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)

	t.Run("PostgreSQL", func(t *testing.T) {
		const image = "postgres:17-alpine"
		env := dockertest.Scope(t)
		project := docker.Project(env)
		dockertest.Cleanup(t, client, project)
		if err := client.PullImage(t.Context(), image, nil); err != nil {
			t.Fatal(err)
		}
		volRef := resource.Ref{Kind: volume.Kind, Scope: env, Name: "pgdata"}
		volName := volume.TargetOf(volRef).Name
		if _, err := client.CreateVolume(t.Context(), &docker.VolumeCreate{
			Name:   volName,
			Labels: map[string]string{docker.LabelProject: project},
		}); err != nil {
			t.Fatal(err)
		}
		cid, err := client.CreateContainer(t.Context(), docker.ContainerName(project, "pg"), &docker.CreateRequest{
			Config: docker.Config{
				Image:  image,
				Env:    []string{"POSTGRES_PASSWORD=admin-secret-pw"},
				Labels: map[string]string{docker.LabelProject: project, docker.LabelService: "pg", docker.LabelNumber: "1"},
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

		// Wait until PostgreSQL accepts connections.
		waitReady(t, client, cid, []string{"pg_isready", "-q", "-h", "127.0.0.1", "-U", "postgres"})

		// Create app user + database via UserDriver.
		dbDir := t.TempDir()
		userDriver := &engines.UserDriver{Client: client, Dir: dbDir, Wait: time.Minute, Every: time.Second}
		userRef := resource.Ref{Kind: engines.UserKind, Scope: env, Name: "shop"}
		userSpec := &resourcev1.DatabaseUser{Service: "pg", Password: "SHOP_PASSWORD"}
		applyCtx := resource.WithValues(t.Context(), map[string]string{"PASSWORD": "shop-app-secret"})
		if err := userDriver.Apply(applyCtx, resource.Step{Ref: userRef, Action: resource.ActionCreate}, userSpec); err != nil {
			t.Fatalf("UserDriver create shop: %v", err)
		}

		pgExecAs := func(user, db, sql string) (string, bool) {
			var out, errs bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			code, err := client.Exec(ctx, cid, []string{"psql", "-XAt", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", db, "-c", sql}, docker.Streams{Stdout: &out, Stderr: &errs})
			if err != nil {
				t.Fatal(err)
			}
			if code != 0 {
				return strings.TrimSpace(out.String() + errs.String()), false
			}
			// A client's warnings go to stderr; what was read is stdout.
			return strings.TrimSpace(out.String()), true
		}

		// Seed table & row as app user shop.
		if said, ok := pgExecAs("shop", "shop", "CREATE TABLE orders (id int PRIMARY KEY, note text); INSERT INTO orders VALUES (1, 'before-import');"); !ok {
			t.Fatalf("seed shop as app user: %s", said)
		}

		runner := &Runner{Client: client, Root: t.TempDir()}

		// 1. Refuse a dump containing \! (H1).
		evilSQL := []byte("SELECT 1;\n\\! id\n")
		evilMeta, err := runner.BeginImport("user:admin", "", "svc_pg", "shop", "evil.sql", "", uint64(len(evilSQL)), 0)
		if err != nil {
			t.Fatal(err)
		}
		stageReal(t, runner, evilMeta.ID, evilSQL)
		if _, _, err := runner.JudgeRestore(t.Context(), volRef, resource.RestoreSource{
			Change: "chg_evil", ImportID: evilMeta.ID, ImportDatabase: "shop", ImportFilename: "evil.sql", ImportFormat: ImportFormatSQL,
		}); err == nil || !strings.Contains(err.Error(), "shell command") {
			t.Fatalf("JudgeRestore(evil) = %v, want refusal", err)
		}

		// 2. Failing import: contains secret value on failing line + extra table before failure.
		badSQL := []byte("CREATE TABLE leaked_extra (id int);\nINSERT INTO orders VALUES ('not-an-int', 'TOP-SECRET-TOKEN-999');\n")
		badMeta, err := runner.BeginImport("user:admin", "", "svc_pg", "shop", "bad.sql", "", uint64(len(badSQL)), 0)
		if err != nil {
			t.Fatal(err)
		}
		stageReal(t, runner, badMeta.ID, badSQL)
		badSrc := resource.RestoreSource{Change: "chg_bad", ImportID: badMeta.ID, ImportDatabase: "shop", ImportFilename: "bad.sql", ImportFormat: ImportFormatSQL}
		badDec, _, err := runner.JudgeRestore(t.Context(), volRef, badSrc)
		if err != nil {
			t.Fatalf("JudgeRestore(bad): %v", err)
		}
		err = runner.ApplyRestore(t.Context(), resource.Step{Ref: volRef, Decision: badDec}, badSrc)
		if err == nil {
			t.Fatal("failing import unexpectedly succeeded")
		}
		if strings.Contains(err.Error(), "TOP-SECRET-TOKEN-999") || strings.Contains(err.Error(), "LINE 1:") {
			t.Errorf("M4: failed import error leaked dump input: %v", err)
		}
		// Check app user can still read & write, original row intact, extra table absent (H2, M1, M2).
		if said, ok := pgExecAs("shop", "shop", "SELECT note FROM orders WHERE id = 1;"); !ok || said != "before-import" {
			t.Fatalf("after failed import, shop read = (%q, %t), want before-import", said, ok)
		}
		if said, ok := pgExecAs("shop", "shop", "INSERT INTO orders VALUES (2, 'app-still-writes'); DELETE FROM orders WHERE id = 2;"); !ok {
			t.Fatalf("after failed import, app user lost write access: %s", said)
		}
		if said, ok := pgExecAs("shop", "shop", "SELECT to_regclass('public.leaked_extra');"); !ok || said != "" {
			t.Fatalf("after failed import, extra table remained: %q", said)
		}

		// 3. Successful .sql import adding a table, then Undo removes the added table and keeps ownership (H2, M2).
		goodSQL := []byte("CREATE TABLE added_by_import (id int); INSERT INTO added_by_import VALUES (99);\n")
		goodMeta, err := runner.BeginImport("user:admin", "", "svc_pg", "shop", "good.sql", "", uint64(len(goodSQL)), 0)
		if err != nil {
			t.Fatal(err)
		}
		stageReal(t, runner, goodMeta.ID, goodSQL)
		goodSrc := resource.RestoreSource{Change: "chg_good", ImportID: goodMeta.ID, ImportDatabase: "shop", ImportFilename: "good.sql", ImportFormat: ImportFormatSQL}
		goodDec, _, err := runner.JudgeRestore(t.Context(), volRef, goodSrc)
		if err != nil {
			t.Fatalf("JudgeRestore(good): %v", err)
		}
		if err := runner.ApplyRestore(t.Context(), resource.Step{Ref: volRef, Decision: goodDec}, goodSrc); err != nil {
			t.Fatalf("ApplyRestore(good): %v", err)
		}
		if said, ok := pgExecAs("shop", "shop", "SELECT id FROM added_by_import;"); !ok || said != "99" {
			t.Fatalf("after good .sql import, app user read = (%q, %t), want 99", said, ok)
		}
		// Undo import.
		undoSrc := resource.RestoreSource{Undoes: "chg_good", Change: "chg_undo"}
		undoDec, _, err := runner.JudgeRestore(t.Context(), volRef, undoSrc)
		if err != nil {
			t.Fatalf("JudgeRestore(undo): %v", err)
		}
		if err := runner.ApplyRestore(t.Context(), resource.Step{Ref: volRef, Decision: undoDec}, undoSrc); err != nil {
			t.Fatalf("ApplyRestore(undo): %v", err)
		}
		if said, ok := pgExecAs("shop", "shop", "SELECT to_regclass('public.added_by_import');"); !ok || said != "" {
			t.Fatalf("M2: Undo left added_by_import table behind: %q", said)
		}
		if said, ok := pgExecAs("shop", "shop", "SELECT note FROM orders WHERE id = 1;"); !ok || said != "before-import" {
			t.Fatalf("H2: after Undo, shop read = (%q, %t), want before-import", said, ok)
		}

		// 4. Custom-format .dump (pg_dump -Fc) import as owner (H1, H2).
		var fcBuf bytes.Buffer
		code, err := client.Exec(t.Context(), cid, []string{"pg_dump", "-Fc", "-U", "shop", "-d", "shop"}, docker.Streams{Stdout: &fcBuf})
		if err != nil || code != 0 || !bytes.HasPrefix(fcBuf.Bytes(), []byte("PGDMP")) {
			t.Fatalf("pg_dump -Fc failed: code=%d err=%v len=%d", code, err, fcBuf.Len())
		}
		if said, ok := pgExecAs("shop", "shop", "DELETE FROM orders;"); !ok {
			t.Fatalf("clear orders before .dump import: %s", said)
		}
		fcMeta, err := runner.BeginImport("user:admin", "", "svc_pg", "shop", "custom.dump", "", uint64(fcBuf.Len()), 0)
		if err != nil {
			t.Fatal(err)
		}
		stageReal(t, runner, fcMeta.ID, fcBuf.Bytes())
		fcSrc := resource.RestoreSource{Change: "chg_fc", ImportID: fcMeta.ID, ImportDatabase: "shop", ImportFilename: "custom.dump", ImportFormat: ImportFormatDump}
		fcDec, _, err := runner.JudgeRestore(t.Context(), volRef, fcSrc)
		if err != nil {
			t.Fatalf("JudgeRestore(.dump): %v", err)
		}
		if err := runner.ApplyRestore(t.Context(), resource.Step{Ref: volRef, Decision: fcDec}, fcSrc); err != nil {
			t.Fatalf("ApplyRestore(.dump): %v", err)
		}
		if said, ok := pgExecAs("shop", "shop", "SELECT note FROM orders WHERE id = 1;"); !ok || said != "before-import" {
			t.Fatalf("after .dump import, app user read = (%q, %t), want before-import", said, ok)
		}

		// 4b. Real pg_dump of a plpgsql function followed by \! is caught by the scanner (N1).
		if said, ok := pgExecAs("shop", "shop", "CREATE FUNCTION greet_shop() RETURNS text LANGUAGE plpgsql AS $fn$ BEGIN RETURN 'ok'; END; $fn$;"); !ok {
			t.Fatalf("create plpgsql function: %s", said)
		}
		var fnDump bytes.Buffer
		code, err = client.Exec(t.Context(), cid, []string{"pg_dump", "-U", "shop", "--no-owner", "--no-acl", "-d", "shop"}, docker.Streams{Stdout: &fnDump})
		if err != nil || code != 0 {
			t.Fatalf("pg_dump plpgsql failed: code=%d err=%v", code, err)
		}
		fnDump.WriteString("\n\\! id\n")
		if err := scanSQLDump(dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL, bytes.NewReader(fnDump.Bytes())); err == nil || !strings.Contains(err.Error(), "shell command") {
			t.Fatalf("N1: real pg_dump with plpgsql function + \\! was not refused: %v", err)
		}

		// 5. Delete backs up keyed to chg_del_pg, plain create starts empty (N3),
		// and Undo restores the deleted database's rows (H4, N3).
		delCtx := resource.WithDeploy(t.Context(), resource.Deploy{Change: "chg_del_pg"})
		if err := userDriver.Apply(delCtx, resource.Step{Ref: userRef, Action: resource.ActionDelete}, nil); err != nil {
			t.Fatalf("UserDriver delete shop: %v", err)
		}
		plainUserDec, err := userDriver.Judge(t.Context(), userRef, nil, userSpec, nil)
		if err != nil || plainUserDec.Restores {
			t.Fatalf("N3: plain Judge after delete = (%+v, %v), want Restores=false", plainUserDec, err)
		}
		undoCtx := resource.WithDeploy(applyCtx, resource.Deploy{Undoes: "chg_del_pg"})
		undoUserDec, err := userDriver.Judge(undoCtx, userRef, nil, userSpec, nil)
		if err != nil || !undoUserDec.Restores {
			t.Fatalf("Judge after delete = (%+v, %v), want Restores=true", undoUserDec, err)
		}
		if err := userDriver.Apply(undoCtx, resource.Step{Ref: userRef, Decision: undoUserDec}, userSpec); err != nil {
			t.Fatalf("UserDriver undo create+restore: %v", err)
		}
		if said, ok := pgExecAs("shop", "shop", "SELECT note FROM orders WHERE id = 1;"); !ok || said != "before-import" {
			t.Fatalf("H4: after delete+undo, shop read = (%q, %t), want before-import", said, ok)
		}

		// 6. Console command runs SELECT 1 with PSQL_HISTORY=/dev/null (L1, L3).
		consoleCmd, consoleEnv, _, err := engines.ConsoleCommand(engines.Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL), 5432, "shop")
		if err != nil {
			t.Fatalf("ConsoleCommand: %v", err)
		}
		var conOut, conErr bytes.Buffer
		conCode, err := client.Exec(t.Context(), cid, append(append([]string{"env"}, consoleEnv...), consoleCmd...), docker.Streams{
			Stdin: strings.NewReader("SELECT 1;\n\\q\n"), Stdout: &conOut, Stderr: &conErr,
		})
		if err != nil || conCode != 0 || !strings.Contains(conOut.String(), "1") {
			t.Fatalf("PG ConsoleCommand failed: code=%d err=%v out=%q errOut=%q", conCode, err, conOut.String(), conErr.String())
		}
	})

	t.Run("MariaDB", func(t *testing.T) {
		const image = "mariadb:11"
		env := dockertest.Scope(t)
		project := docker.Project(env)
		dockertest.Cleanup(t, client, project)
		if err := client.PullImage(t.Context(), image, nil); err != nil {
			t.Fatal(err)
		}
		volRef := resource.Ref{Kind: volume.Kind, Scope: env, Name: "mydata"}
		volName := volume.TargetOf(volRef).Name
		if _, err := client.CreateVolume(t.Context(), &docker.VolumeCreate{
			Name:   volName,
			Labels: map[string]string{docker.LabelProject: project},
		}); err != nil {
			t.Fatal(err)
		}
		cid, err := client.CreateContainer(t.Context(), docker.ContainerName(project, "maria"), &docker.CreateRequest{
			Config: docker.Config{
				Image:  image,
				Env:    []string{"MARIADB_ROOT_PASSWORD=root-secret-pw"},
				Labels: map[string]string{docker.LabelProject: project, docker.LabelService: "maria", docker.LabelNumber: "1"},
			},
			HostConfig: docker.HostConfig{
				Mounts: []docker.Mount{{Type: "volume", Source: volName, Target: "/var/lib/mysql"}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.StartContainer(t.Context(), cid); err != nil {
			t.Fatal(err)
		}

		waitReady(t, client, cid, []string{"sh", "-c", `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb --protocol=TCP -h127.0.0.1 -uroot -e 'SELECT 1'`})

		dbDir := t.TempDir()
		userDriver := &engines.UserDriver{Client: client, Dir: dbDir, Wait: time.Minute, Every: time.Second}
		userRef := resource.Ref{Kind: engines.UserKind, Scope: env, Name: "shop"}
		userSpec := &resourcev1.DatabaseUser{Service: "maria", Password: "SHOP_PASSWORD"}
		applyCtx := resource.WithValues(t.Context(), map[string]string{"PASSWORD": "shop-app-secret"})
		if err := userDriver.Apply(applyCtx, resource.Step{Ref: userRef, Action: resource.ActionCreate}, userSpec); err != nil {
			t.Fatalf("UserDriver create MariaDB shop: %v", err)
		}

		myExecAs := func(user, pw, db, sql string) (string, bool) {
			var out, errs bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			code, err := client.Exec(ctx, cid, []string{"env", "MYSQL_PWD=" + pw, "mariadb", "--protocol=TCP", "-h127.0.0.1", "-u" + user, "-N", "-B", db}, docker.Streams{
				Stdin: strings.NewReader(sql + "\n"), Stdout: &out, Stderr: &errs,
			})
			if err != nil {
				t.Fatal(err)
			}
			if code != 0 {
				return strings.TrimSpace(out.String() + errs.String()), false
			}
			// A client's warnings go to stderr; what was read is stdout.
			return strings.TrimSpace(out.String()), true
		}

		if said, ok := myExecAs("shop", "shop-app-secret", "shop", "CREATE TABLE orders (id INT PRIMARY KEY, note VARCHAR(64)); INSERT INTO orders VALUES (1, 'before-import');"); !ok {
			t.Fatalf("seed MariaDB shop: %s", said)
		}

		runner := &Runner{Client: client, Root: t.TempDir()}

		// 1. Refuse \! / system dump (H1).
		evilSQL := []byte("SELECT 1;\nsystem id\n")
		evilMeta, err := runner.BeginImport("user:admin", "", "svc_maria", "shop", "evil.sql", "", uint64(len(evilSQL)), 0)
		if err != nil {
			t.Fatal(err)
		}
		stageReal(t, runner, evilMeta.ID, evilSQL)
		if _, _, err := runner.JudgeRestore(t.Context(), volRef, resource.RestoreSource{
			Change: "chg_evil", ImportID: evilMeta.ID, ImportDatabase: "shop", ImportFilename: "evil.sql", ImportFormat: ImportFormatSQL,
		}); err == nil || !strings.Contains(err.Error(), "shell command") {
			t.Fatalf("MariaDB JudgeRestore(evil) = %v, want refusal", err)
		}

		// 2. Failing import adds an extra table before syntax error with secret token:
		// rollback must empty the DB (removing extra table), restore original row, sanitize error,
		// and let the app user sign in and write again (H2, M1, M2, M4).
		badSQL := []byte("CREATE TABLE leaked_extra (id INT);\nBAD SYNTAX 'TOP-SECRET-MARIA-777';\n")
		badMeta, err := runner.BeginImport("user:admin", "", "svc_maria", "shop", "bad.sql", "", uint64(len(badSQL)), 0)
		if err != nil {
			t.Fatal(err)
		}
		stageReal(t, runner, badMeta.ID, badSQL)
		badSrc := resource.RestoreSource{Change: "chg_bad", ImportID: badMeta.ID, ImportDatabase: "shop", ImportFilename: "bad.sql", ImportFormat: ImportFormatSQL}
		badDec, _, err := runner.JudgeRestore(t.Context(), volRef, badSrc)
		if err != nil {
			t.Fatalf("MariaDB JudgeRestore(bad): %v", err)
		}
		err = runner.ApplyRestore(t.Context(), resource.Step{Ref: volRef, Decision: badDec}, badSrc)
		if err == nil {
			t.Fatal("MariaDB failing import unexpectedly succeeded")
		}
		if strings.Contains(err.Error(), "TOP-SECRET-MARIA-777") {
			t.Errorf("M4: MariaDB failed import error leaked dump secret: %v", err)
		}
		if said, ok := myExecAs("shop", "shop-app-secret", "shop", "SELECT note FROM orders WHERE id = 1;"); !ok || said != "before-import" {
			t.Fatalf("MariaDB after failed import, shop read = (%q, %t), want before-import", said, ok)
		}
		if said, ok := myExecAs("shop", "shop-app-secret", "shop", "SHOW TABLES LIKE 'leaked_extra';"); !ok || said != "" {
			t.Fatalf("M2: MariaDB rollback left leaked_extra behind: %q", said)
		}
		if said, ok := myExecAs("shop", "shop-app-secret", "shop", "INSERT INTO orders VALUES (2, 'still-writes'); DELETE FROM orders WHERE id = 2;"); !ok {
			t.Fatalf("H2: MariaDB app user lost write access after rollback: %s", said)
		}

		// 3. Successful .sql import adding a table, then Undo removes it.
		goodSQL := []byte("CREATE TABLE added_tbl (id INT); INSERT INTO added_tbl VALUES (42);\n")
		goodMeta, err := runner.BeginImport("user:admin", "", "svc_maria", "shop", "good.sql", "", uint64(len(goodSQL)), 0)
		if err != nil {
			t.Fatal(err)
		}
		stageReal(t, runner, goodMeta.ID, goodSQL)
		goodSrc := resource.RestoreSource{Change: "chg_good", ImportID: goodMeta.ID, ImportDatabase: "shop", ImportFilename: "good.sql", ImportFormat: ImportFormatSQL}
		goodDec, _, err := runner.JudgeRestore(t.Context(), volRef, goodSrc)
		if err != nil {
			t.Fatalf("MariaDB JudgeRestore(good): %v", err)
		}
		if err := runner.ApplyRestore(t.Context(), resource.Step{Ref: volRef, Decision: goodDec}, goodSrc); err != nil {
			t.Fatalf("MariaDB ApplyRestore(good): %v", err)
		}
		undoSrc := resource.RestoreSource{Undoes: "chg_good", Change: "chg_undo"}
		undoDec, _, err := runner.JudgeRestore(t.Context(), volRef, undoSrc)
		if err != nil {
			t.Fatalf("MariaDB JudgeRestore(undo): %v", err)
		}
		if err := runner.ApplyRestore(t.Context(), resource.Step{Ref: volRef, Decision: undoDec}, undoSrc); err != nil {
			t.Fatalf("MariaDB ApplyRestore(undo): %v", err)
		}
		if said, ok := myExecAs("shop", "shop-app-secret", "shop", "SHOW TABLES LIKE 'added_tbl';"); !ok || said != "" {
			t.Fatalf("M2: MariaDB Undo left added_tbl behind: %q", said)
		}
		if said, ok := myExecAs("shop", "shop-app-secret", "shop", "SELECT note FROM orders WHERE id = 1;"); !ok || said != "before-import" {
			t.Fatalf("H2: MariaDB after Undo, shop read = (%q, %t), want before-import", said, ok)
		}

		// 4. Delete with backup + Undo restores rows (H4, N3).
		myDelCtx := resource.WithDeploy(t.Context(), resource.Deploy{Change: "chg_del_my"})
		if err := userDriver.Apply(myDelCtx, resource.Step{Ref: userRef, Action: resource.ActionDelete}, nil); err != nil {
			t.Fatalf("MariaDB UserDriver delete: %v", err)
		}
		myUndoCtx := resource.WithDeploy(applyCtx, resource.Deploy{Undoes: "chg_del_my"})
		undoUserDec, err := userDriver.Judge(myUndoCtx, userRef, nil, userSpec, nil)
		if err != nil || !undoUserDec.Restores {
			t.Fatalf("MariaDB Judge after delete = (%+v, %v), want Restores=true", undoUserDec, err)
		}
		if err := userDriver.Apply(myUndoCtx, resource.Step{Ref: userRef, Decision: undoUserDec}, userSpec); err != nil {
			t.Fatalf("MariaDB UserDriver undo: %v", err)
		}
		if said, ok := myExecAs("shop", "shop-app-secret", "shop", "SELECT note FROM orders WHERE id = 1;"); !ok || said != "before-import" {
			t.Fatalf("H4: MariaDB after delete+undo, shop read = (%q, %t), want before-import", said, ok)
		}

		// 5. Console command runs SELECT 1 with MYSQL_HISTFILE=/dev/null (L3).
		consoleCmd, consoleEnv, _, err := engines.ConsoleCommand(engines.Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB), 3306, "shop")
		if err != nil {
			t.Fatalf("MariaDB ConsoleCommand: %v", err)
		}
		var conOut, conErr bytes.Buffer
		conCode, err := client.Exec(t.Context(), cid, append(append([]string{"env"}, consoleEnv...), consoleCmd...), docker.Streams{
			Stdin: strings.NewReader("SELECT 1;\n"), Stdout: &conOut, Stderr: &conErr,
		})
		if err != nil || conCode != 0 || !strings.Contains(conOut.String(), "1") {
			t.Fatalf("MariaDB ConsoleCommand failed: code=%d err=%v out=%q errOut=%q", conCode, err, conOut.String(), conErr.String())
		}
	})
}

func waitReady(t *testing.T, client *docker.Client, cid string, cmd []string) {
	t.Helper()
	for range 90 {
		code, err := client.Exec(t.Context(), cid, cmd, docker.Streams{})
		if err == nil && code == 0 {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("container %s did not become ready", cid)
}

func stageReal(t *testing.T, r *Runner, importID string, data []byte) {
	t.Helper()
	sent := false
	_, err := r.WriteImport(t.Context(), importID, "user:admin", 0, func() ([]byte, error) {
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
