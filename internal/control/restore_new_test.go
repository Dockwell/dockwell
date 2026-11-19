// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/backups"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// sqlFile is a PostgreSQL whose data is one file of SQL: a dump of the whole server
// is that file, and a load of one writes it, with the password the loader runs with.
type sqlFile struct {
	mu        sync.Mutex
	failLoads bool
	loadedAs  []string // the password each load ran with
}

func (s *sqlFile) image() dockertest.Image {
	return dockertest.Image{
		Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}},
		Exec: func(x dockertest.Exec) int {
			file := filepath.Join(x.Volumes["/var/lib/postgresql/data"], "data.sql")
			script := strings.Join(x.Cmd, " ")
			switch {
			case strings.Contains(script, "pg_dumpall"):
				data, err := os.ReadFile(file)
				if err != nil {
					return 1
				}
				_, _ = x.Stdout.Write(data)
				_, _ = io.WriteString(x.Stdout, "\n--\n-- PostgreSQL database cluster dump complete\n--\n")
			case strings.Contains(script, "-d postgres -f -"):
				data, err := io.ReadAll(x.Stdin)
				if err != nil {
					return 1
				}
				s.mu.Lock()
				defer s.mu.Unlock()
				for _, kv := range x.Env {
					if pw, ok := strings.CutPrefix(kv, "POSTGRES_PASSWORD="); ok {
						s.loadedAs = append(s.loadedAs, pw)
					}
				}
				if s.failLoads {
					// Part of it loads, then it stops, as a full disk would leave it.
					_ = os.WriteFile(file, data[:len(data)/2], 0o600)
					_, _ = io.WriteString(x.Stderr, "psql:<stdin>:9: ERROR:  could not extend file: No space left on device\n")
					return 0
				}
				if err := os.WriteFile(file, data, 0o600); err != nil {
					return 1
				}
			case strings.Contains(script, "psql -V"):
				_, _ = io.WriteString(x.Stdout, "psql (PostgreSQL) 17.11\n")
			}
			return 0
		},
	}
}

// A backup of a catalog database restores into a new database, a project of its
// own on the same server, through one change that makes it and fills its volume
// before it first starts. The database it was taken of runs on as it was: its data,
// its revision, its container and when it started, whether the restore works or
// fails partway. The new one takes copies of the original's root password, which
// the data signs in with, as its own secret, and copying each is on the activity
// log. A restore that fails removes what it made (ADR 0106).
func TestABackupRestoresIntoANewDatabase(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testABackupRestoresIntoANewDatabase)
}

func testABackupRestoresIntoANewDatabase(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pg := &sqlFile{}
	e.Find = func(image string) (dockertest.Image, bool) {
		if strings.HasPrefix(image, "postgres:") {
			return pg.image(), true
		}
		return dockertest.Image{}, true
	}
	c, cat, bc := changeClient(b, b.origin), catalogClient(b), backupClient(b, b.origin)
	// A volume that wasn't installed from the catalog says what to do instead.
	volumeID, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")
	other, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID}))
	if err != nil {
		t.Fatal(err)
	}
	waitBackup(t, bc, other.Msg.GetBackup().GetId())
	if _, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: other.Msg.GetBackup().GetId(), Into: "web-restored"})); reasonOf(err) != "no_pin" {
		t.Errorf("restoring a volume with no catalog pin into a new database = %v", err)
	}

	if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: "warehouse", Companions: map[string]bool{"adminer": true}})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the install ended %v: %s", done.GetState(), done.GetError())
	}

	type state struct {
		env, volumeID, dir, container, started string
		serviceRev, secretID                   string
		secretRev                              int
		password                               string
	}
	read := func(project string) state {
		t.Helper()
		var s state
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			org, err := tx.Organization(t.Context())
			if err != nil {
				return err
			}
			p, err := tx.GetResource(t.Context(), "project", org.ID, project)
			if err != nil {
				return err
			}
			envs, err := tx.ListResources(t.Context(), p.ID, "environment")
			if err != nil || len(envs) != 1 {
				t.Fatalf("%s's environments: %v, %v", project, envs, err)
			}
			s.env = envs[0].ID
			db, err := tx.GetResource(t.Context(), "service", s.env, "db")
			if err != nil {
				return err
			}
			s.serviceRev = fmt.Sprintf("%s@%d", db.ID, db.Revision)
			vol, err := tx.GetResource(t.Context(), "volume", s.env, "data")
			if err != nil {
				return err
			}
			s.volumeID = vol.ID
			secret, err := tx.GetResource(t.Context(), "secret", s.env, "POSTGRES_PASSWORD")
			if err != nil {
				return err
			}
			s.secretID, s.secretRev = secret.ID, secret.Revision
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		s.dir = e.VolumeDir(volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: s.env, Name: "data"}).Name)
		list, err := e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject + "=" + docker.Project(s.env), docker.LabelService + "=db"}})
		if err != nil || len(list) != 1 {
			t.Fatalf("%s's db: %v, %v (of %v)", project, list, err, e.Containers())
		}
		running, err := e.Client().InspectContainer(t.Context(), list[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		s.container, s.started = running.ID, running.State.StartedAt.String()
		if !running.State.Running {
			s.started = "stopped"
		}
		for _, kv := range running.Config.Env {
			if pw, ok := strings.CutPrefix(kv, "POSTGRES_PASSWORD="); ok {
				s.password = pw
			}
		}
		return s
	}
	original := read("warehouse")
	writeData(t, original.dir, "data.sql", "CREATE TABLE orders (id int);\n")
	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: original.volumeID}))
	if err != nil {
		t.Fatal(err)
	}
	backup := waitBackup(t, bc, made.Msg.GetBackup().GetId())
	if backup.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE || backup.GetMethod() != dockwellv1.BackupMethod_BACKUP_METHOD_DUMP {
		t.Fatalf("the backup = %v", backup)
	}
	// What the original has since, which the new database mustn't.
	since := "CREATE TABLE orders (id int);\nCREATE TABLE since (id int);\n"
	writeData(t, original.dir, "data.sql", since)
	unchanged := func(when string) {
		t.Helper()
		now := read("warehouse")
		if now.container != original.container || now.started != original.started || now.serviceRev != original.serviceRev || now.secretRev != original.secretRev {
			t.Errorf("%s, the original went from %+v to %+v", when, original, now)
		}
		if got := readData(t, original.dir, "data.sql"); got != since {
			t.Errorf("%s, the original holds %q", when, got)
		}
	}

	// Refused before anything is planned.
	for _, tc := range []struct {
		into, reason string
	}{
		{"warehouse", "project_exists"},
		{"Not a name!", "invalid"},
	} {
		if _, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId(), Into: tc.into})); err == nil || tc.reason != "invalid" && reasonOf(err) != tc.reason || tc.reason == "invalid" && connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("restoring into %q = %v", tc.into, err)
		}
	}

	// A load that fails partway: the change fails, and what it made goes.
	pg.mu.Lock()
	pg.failLoads = true
	pg.mu.Unlock()
	planned, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId(), Into: "warehouse-broken"}))
	if err != nil {
		t.Fatal(err)
	}
	if end := apply(t, c, planned.Msg.GetChange()); end.GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatal("a restore whose load failed applied")
	}
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		org, err := tx.Organization(t.Context())
		if err != nil {
			return err
		}
		if p, err := tx.GetResource(t.Context(), "project", org.ID, "warehouse-broken"); err == nil && p.Spec != nil {
			t.Error("the failed restore's project is left")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unchanged("after a restore that failed")
	pg.mu.Lock()
	pg.failLoads = false
	pg.mu.Unlock()

	planned, err = bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId(), Into: "warehouse-restored"}))
	if err != nil {
		t.Fatal(err)
	}
	ch := planned.Msg.GetChange()
	if ch.GetPlan().GetDestructive() {
		t.Error("a restore into a new database says it destroys data")
	}
	var restores int
	for _, s := range ch.GetPlan().GetServers() {
		for _, step := range s.GetSteps() {
			if step.GetResource().GetScope() == original.env {
				t.Errorf("the plan touches the original's %v", step.GetResource())
			}
			if r := step.GetRestore(); r != nil {
				restores++
				if !r.GetIntoNew() || r.GetBackupId() != backup.GetId() || len(r.GetStops()) != 0 || step.GetResource().GetKind() != "service" || step.GetDestroysData() {
					t.Errorf("the restore's step = %v", step)
				}
			}
		}
	}
	if restores != 1 {
		t.Errorf("the plan restores %d times", restores)
	}
	// Adminer publishes on a port of its own, as the original's holds its.
	var moved bool
	for _, n := range ch.GetPlan().GetNotes() {
		moved = moved || n.GetReason() == "restore_port" && strings.HasPrefix(n.GetMessage(), "ADMIN_PORT is ")
	}
	if !moved {
		t.Errorf("Adminer's port didn't move: %v", ch.GetPlan().GetNotes())
	}
	if end := apply(t, c, ch); end.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the restore ended %v: %s", end.GetState(), end.GetError())
	}
	restored := read("warehouse-restored")
	if got := readData(t, restored.dir, "data.sql"); !strings.HasPrefix(got, "CREATE TABLE orders (id int);\n") || strings.Contains(got, "since") {
		t.Errorf("the new database holds %q", got)
	}
	if restored.started == "stopped" {
		t.Error("the new database isn't running")
	}
	// Its password is the original's, as the data signs in with it, in a secret of
	// its own: the same value, never the same secret.
	if restored.password == "" || restored.password != original.password || restored.secretID == original.secretID {
		t.Errorf("the new database's password secret is %s, the original's %s; the same value: %t", restored.secretID, original.secretID, restored.password == original.password)
	}
	pg.mu.Lock()
	for _, pw := range pg.loadedAs {
		if pw != original.password {
			t.Error("a load ran with another password than the original's")
		}
	}
	pg.mu.Unlock()
	unchanged("after the restore")

	// Each copy is on record: which secret, from which database, into which new one.
	var copied []store.Event
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		events, err := tx.AuditEvents(t.Context(), store.AuditFilter{Query: backups.ActionCopySecret}, store.TimelineCursor{}, 50)
		for i := range events {
			if events[i].Action == backups.ActionCopySecret {
				copied = append(copied, events[i])
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var into []string
	for i := range copied {
		ev := &copied[i]
		if ev.Target != original.secretID || !strings.Contains(ev.Detail, "POSTGRES_PASSWORD from warehouse into chg_") || strings.Contains(ev.Detail, original.password) {
			t.Errorf("the copy's event = %+v", ev)
		}
		into = append(into, ev.Detail[strings.LastIndex(ev.Detail, " ")+1:])
	}
	if strings.Join(into, ",") != "warehouse-restored,warehouse-broken" && strings.Join(into, ",") != "warehouse-broken,warehouse-restored" {
		t.Errorf("copies on record: %v", into)
	}

	// Undoing it removes the new database, and restores nothing anywhere.
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: ch.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range undo.Msg.GetChange().GetPlan().GetServers() {
		for _, step := range s.GetSteps() {
			if step.GetRestore() != nil || step.GetResource().GetScope() == original.env {
				t.Errorf("the undo's step = %v", step)
			}
		}
	}
	u := undo.Msg.GetChange()
	if !u.GetPlan().GetDestructive() {
		t.Error("removing the new database doesn't say it destroys data")
	}
	w := watch(t, c, u.GetId())
	if _, err := applyChange(t, c, &dockwellv1.ApplyChangeRequest{Id: u.GetId(), Fingerprint: u.GetPlan().GetFingerprint()}); err != nil {
		t.Fatal(err)
	}
	if end := waitWatch(t, w).last(); end.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %s", end.GetState(), end.GetError())
	}
	unchanged("after undoing the restore")
}

// volumeID is a volume's ID, by its environment and name.
func volumeID(t *testing.T, f *fullServer, env, name string) string {
	t.Helper()
	var id string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(volume.Kind), env, name)
		id = r.ID
		return err
	}); err != nil {
		t.Fatalf("volume %s in %s: %v", name, env, err)
	}
	return id
}

// A database moved to a new release runs on a volume of a new name, while its
// catalog file names the first. A backup taken after the move, of the new volume,
// and one from before it, of the volume the move left, both restore into a new
// database, which fills its own data volume (the review's M3).
func TestAMovedDatabaseRestoresIntoANewOne(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		catalogDir := t.TempDir()
		useCatalogDir(t, catalogDir)
		b := newBench(t, backend)
		b.e.Find = func(image string) (dockertest.Image, bool) {
			switch {
			case strings.HasPrefix(image, "postgres:16"):
				return pgOf("16"), true
			case strings.HasPrefix(image, "postgres:"):
				return pgOf("17"), true
			}
			return dockertest.Image{}, true
		}
		// Installed from an older release of the catalog's PostgreSQL, 16.
		builtIn, err := os.ReadFile("../../internal/catalog/snapshot/entries/postgres.yaml")
		if err != nil {
			t.Fatal(err)
		}
		older := regexp.MustCompile(`postgres:17\.11@sha256:[0-9a-f]{64}`).ReplaceAllString(
			regexp.MustCompile(`(?m)^release: (\d+)$`).ReplaceAllString(strings.Replace(string(builtIn), `version: "17.11"`, `version: "16.10"`, 1), "release: 1"),
			"postgres:16.10@sha256:"+strings.Repeat("4", 64))
		if !strings.Contains(older, "postgres:16.10@") {
			t.Fatal("the older release names no PostgreSQL 16")
		}
		entry := filepath.Join(catalogDir, "postgres.yaml")
		if err := os.WriteFile(entry, []byte(older), 0o600); err != nil {
			t.Fatal(err)
		}
		cat, bc := catalogClient(b.b), backupClient(b.b, b.b.origin)
		b.done(t, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: "warehouse"}))
		db := b.service(t, "warehouse", "db")
		env := db.GetResource().GetScope()
		dir := func(scope, name string) string {
			return b.e.VolumeDir(volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: scope, Name: name}).Name)
		}
		writeData(t, dir(env, "data"), "data.sql", "CREATE TABLE orders (id int);\n")
		backUp := func(volumeName string) string {
			t.Helper()
			made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID(t, b.f, env, volumeName)}))
			if err != nil {
				t.Fatal(err)
			}
			got := waitBackup(t, bc, made.Msg.GetBackup().GetId())
			if got.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE {
				t.Fatalf("the backup of %s = %v", volumeName, got)
			}
			return got.GetId()
		}
		before := backUp("data")
		// The catalog's next release is PostgreSQL 17, which moves the data by a dump
		// into a volume of a new name (ADR 0080).
		if err := os.Remove(entry); err != nil {
			t.Fatal(err)
		}
		upgraded, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "warehouse"}))
		if err != nil {
			t.Fatal(err)
		}
		b.done(t, upgraded.Msg.GetChange())
		moved := ""
		for _, name := range []string{"data-17"} {
			if _, err := os.Stat(dir(env, name)); err == nil {
				moved = name
			}
		}
		if moved == "" {
			t.Fatal("the move made no volume of a new name")
		}
		after := backUp(moved)
		for _, tc := range []struct{ backup, into string }{{after, "warehouse-after"}, {before, "warehouse-before"}} {
			planned, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: tc.backup, Into: tc.into}))
			if err != nil {
				t.Fatalf("restoring %s into %s: %v", tc.backup, tc.into, err)
			}
			b.done(t, planned.Msg.GetChange())
			restored := b.service(t, tc.into, "db").GetResource().GetScope()
			if got := readData(t, dir(restored, "data"), "data.sql"); !strings.Contains(got, "CREATE TABLE orders") {
				t.Errorf("%s holds %q", tc.into, got)
			}
		}
	})
}

// fetcherEntry is an app that isn't a database, with one volume and a token of its
// own, as a mail fetcher or a bot is.
const fetcherEntry = `id: fetcher
name: Fetcher
summary: Fetches mail
category: productivity
version: "1.0"
release: 1
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
inputs:
  - name: FETCH_TOKEN
    label: Token
    type: INPUT_TYPE_SECRET
    generate: 32
compose: |
  services:
    app:
      image: ghcr.io/acme/fetcher:1@sha256:` + digest1 + `
      environment:
        TOKEN: ${FETCH_TOKEN}
      volumes:
        - data:/var/lib/fetcher
  volumes:
    data: {}
`

// bundledEntry is an app that ships its own PostgreSQL beside itself, with a token of
// its own, as many catalog apps do.
const bundledEntry = `id: bundled
name: Bundled
summary: Fetches mail into its own database
category: productivity
version: "1.0"
release: 1
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
inputs:
  - name: FETCH_TOKEN
    label: Token
    type: INPUT_TYPE_SECRET
    generate: 32
  - name: DB_PASSWORD
    label: Database password
    type: INPUT_TYPE_SECRET
    generate: 32
compose: |
  services:
    app:
      image: ghcr.io/acme/fetcher:1@sha256:` + digest1 + `
      environment:
        TOKEN: ${FETCH_TOKEN}
        DATABASE_URL: postgres://postgres:${DB_PASSWORD}@db:5432/postgres
    db:
      image: postgres:17@sha256:` + digest1 + `
      environment:
        POSTGRES_PASSWORD: ${DB_PASSWORD}
      volumes:
        - pgdata:/var/lib/postgresql/data
  volumes:
    pgdata: {}
`

// An app that isn't a database isn't restored as a new copy of itself, whoever asks
// and however: not one with a volume of its own, nor one that ships its own database,
// whose backup is a database's. A second copy would act on whatever its token
// reaches, beside the first. It says why, with what to do instead, and copies
// nothing (the review's M2, the re-check's N2).
func TestAnAppIsntRestoredAsANewCopyOfItself(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		dir := t.TempDir()
		useCatalogDir(t, dir)
		for name, entry := range map[string]string{"fetcher.yaml": fetcherEntry, "bundled.yaml": bundledEntry} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(entry), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		pullsAnything(e)
		for _, ref := range []string{"postgres:17", "postgres:17@sha256:" + digest1} {
			e.Registry[ref] = (&sqlFile{}).image()
		}
		c, cat, bc := changeClient(b, b.origin), catalogClient(b), backupClient(b, b.origin)
		for _, tc := range []struct{ entry, project, volume string }{
			{"fetcher", "mail", "data"},
			{"bundled", "inbox", "pgdata"},
		} {
			if done := apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: tc.entry, Project: tc.project})); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
				t.Fatalf("installing %s ended %v: %s", tc.entry, done.GetState(), done.GetError())
			}
			var env string
			if err := f.store.View(t.Context(), func(tx *store.Tx) error {
				org, err := tx.Organization(t.Context())
				if err != nil {
					return err
				}
				p, err := tx.GetResource(t.Context(), "project", org.ID, tc.project)
				if err != nil {
					return err
				}
				envs, err := tx.ListResources(t.Context(), p.ID, "environment")
				if err == nil && len(envs) > 0 {
					env = envs[0].ID
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			writeData(t, e.VolumeDir(volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: env, Name: tc.volume}).Name), "data.sql", "CREATE TABLE mail (id int);\n")
			made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID(t, f, env, tc.volume), Stop: true}))
			if err != nil {
				t.Fatal(err)
			}
			backup := waitBackup(t, bc, made.Msg.GetBackup().GetId())
			if backup.GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE {
				t.Fatalf("the backup of %s = %v", tc.entry, backup)
			}
			_, err = bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backup.GetId(), Into: tc.project + "-copy"}))
			if reasonOf(err) != "not_a_database" || !strings.Contains(err.Error(), "Replace its data instead") {
				t.Errorf("restoring %s into a new copy of itself = %v", tc.entry, err)
			}
		}
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			events, err := tx.AuditEvents(t.Context(), store.AuditFilter{Query: backups.ActionCopySecret}, store.TimelineCursor{}, 10)
			if len(events) != 0 {
				t.Errorf("copies on record: %v", events)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}
