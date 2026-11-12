// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// steps are each step's progress a watch received, in order.
func (w watched) steps() []*dockwellv1.StepProgress {
	var out []*dockwellv1.StepProgress
	for _, m := range w.msgs {
		if s := m.GetStep().GetStep(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

func (b *bench) upgrade(t *testing.T, req *dockwellv1.UpgradeServiceRequest) *dockwellv1.Change {
	t.Helper()
	res, err := b.s.UpgradeService(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("updating %s: %v", req.GetId(), err)
	}
	return res.Msg.GetChange()
}

const shopWithData = `name: shop
services:
  web:
    image: ghcr.io/acme/shop:1.4
    volumes:
      - data:/var/lib/shop
      - conf:/etc/shop:ro
volumes:
  data: {}
  conf: {}
`

// An update says what it is, from and to, backs up what the app writes to before
// anything else, leaves alone what it only reads, and runs the new image, with
// each phase it went through said as it went (ADR 0080).
func TestAnUpdateBacksUpWhatItWritesToFirst(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		b.e.Registry["ghcr.io/acme/shop:1.5"] = dockertest.Image{Size: 30 << 20, PullTime: 50e6, Config: docker.Config{Cmd: []string{"shop"}, Env: []string{"PATH=/bin"}}}
		env := b.run(t, &dockwellv1.ImportComposeRequest{Compose: shopWithData})
		web := b.service(t, "shop", "web")

		plan := b.upgrade(t, &dockwellv1.UpgradeServiceRequest{Id: web.GetId(), Image: "ghcr.io/acme/shop:1.5"})
		intent := plan.GetPlan().GetIntent()
		if intent.GetKind() != dockwellv1.IntentKind_INTENT_KIND_UPGRADE || intent.GetFromImage() != "ghcr.io/acme/shop:1.4" || intent.GetImage() != "ghcr.io/acme/shop:1.5" || intent.GetResourceId() != web.GetId() {
			t.Errorf("the plan's intent = %v", intent)
		}
		if intent.GetFromDigest() == "" {
			t.Error("the plan doesn't say which digest it leaves")
		}
		if plan.GetSummary() != "Update web to ghcr.io/acme/shop:1.5" {
			t.Errorf("the summary = %q", plan.GetSummary())
		}
		steps := plan.GetPlan().GetServers()[0].GetSteps()
		first := slices.IndexFunc(steps, func(s *dockwellv1.PlanStep) bool { return s.GetBackupFirst() != nil })
		replace := slices.IndexFunc(steps, func(s *dockwellv1.PlanStep) bool { return s.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_REPLACE })
		if first < 0 || replace < 0 || first > replace {
			t.Fatalf("the backup isn't before what it protects: %v", steps)
		}
		if got := steps[first].GetResource().GetName(); got != "data" {
			t.Errorf("it backs up %s", got)
		}
		if slices.ContainsFunc(steps, func(s *dockwellv1.PlanStep) bool {
			return s.GetBackupFirst() != nil && s.GetResource().GetName() == "conf"
		}) {
			t.Error("it backs up a volume the app only reads")
		}
		if note := noteOf(plan, "backup_first"); note == nil || !strings.Contains(note.GetMessage(), "backs up data before it changes anything") || !strings.Contains(note.GetMessage(), "puts back ghcr.io/acme/shop:1.4") {
			t.Errorf("the plan's notes = %v", plan.GetPlan().GetNotes())
		}
		if plan.GetPlan().GetDestructive() {
			t.Error("a backup first makes the plan read as destructive")
		}

		w := watch(t, b.c, plan.GetId())
		if _, err := b.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: plan.GetId()})); err != nil {
			t.Fatal(err)
		}
		seen := waitWatch(t, w)
		if last := seen.last(); last.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the update ended %v: %s", last.GetState(), last.GetError())
		}
		if got := b.e.Newest(docker.Project(env), "web").Config.Image; got != "ghcr.io/acme/shop:1.5" {
			t.Errorf("web runs %s", got)
		}
		phases := map[dockwellv1.StepPhase]bool{}
		pulled := false
		for _, u := range seen.steps() {
			phases[u.GetPhase()] = true
			pulled = pulled || u.GetPhase() == dockwellv1.StepPhase_STEP_PHASE_PULLING && u.GetTotalBytes() == 30<<20 && u.GetDoneBytes() > 0
		}
		for _, want := range []dockwellv1.StepPhase{dockwellv1.StepPhase_STEP_PHASE_BACKING_UP, dockwellv1.StepPhase_STEP_PHASE_PULLING, dockwellv1.StepPhase_STEP_PHASE_STARTING} {
			if !phases[want] {
				t.Errorf("no step said it was %v: %v", want, phases)
			}
		}
		if !pulled {
			t.Error("no step said how much of the pull was done")
		}
		res, err := backupClient(b.b, b.b.origin).ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Msg.GetBackups(); len(got) != 1 || got[0].GetVolume() != "data" || got[0].GetState() != dockwellv1.BackupState_BACKUP_STATE_DONE {
			t.Errorf("after the update, backups = %v", got)
		}
	})
}

// Asked to, an update takes no backup and says so; one of a moving tag pulls it
// again and replaces only on a newer image; and one with nothing to go to says so.
func TestAnUpdateCanSkipItsBackupAndNamesWhatItCantBackUp(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		b.run(t, &dockwellv1.ImportComposeRequest{Compose: shopWithData})
		web := b.service(t, "shop", "web")

		plan := b.upgrade(t, &dockwellv1.UpgradeServiceRequest{Id: web.GetId(), Image: "ghcr.io/acme/shop:1.5", SkipBackup: true})
		if slices.ContainsFunc(plan.GetPlan().GetServers()[0].GetSteps(), func(s *dockwellv1.PlanStep) bool { return s.GetBackupFirst() != nil }) {
			t.Error("it backs up what it was told not to")
		}
		if note := noteOf(plan, "no_backup"); note == nil || !strings.Contains(note.GetMessage(), "takes no backup, as you asked") {
			t.Errorf("the plan's notes = %v", plan.GetPlan().GetNotes())
		}
		if _, err := b.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: plan.GetId()})); err != nil {
			t.Fatal(err)
		}

		// Dockwell knows of nothing newer until its checks find it, so an update with
		// no image says so.
		_, err := b.s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: web.GetId()}))
		if reasonOf(err) != "up_to_date" || !strings.Contains(err.Error(), "knows of no newer version of web yet") {
			t.Errorf("an update with nothing to go to = %v", err)
		}

		// A moving tag pulls again, as a deploy of it does.
		name, shop := runLatest(t, b)
		moveLatest(b.e, digestB)
		again := b.upgrade(t, &dockwellv1.UpgradeServiceRequest{Id: shop.GetId(), Image: latest})
		if note := noteOf(again, "pulls_again"); note == nil {
			t.Errorf("the plan's notes = %v", again.GetPlan().GetNotes())
		}
		if again.GetSummary() != "Update web to the newest "+latest {
			t.Errorf("the summary = %q", again.GetSummary())
		}
		before := b.e.Serving(name).ID
		b.done(t, again)
		if b.e.Serving(name).ID == before {
			t.Error("with a newer image, the container wasn't replaced")
		}
	})
}

// pgOf is the fake PostgreSQL of a major release, as its image says it.
func pgOf(major string) dockertest.Image {
	img := fakePostgres
	img.Config.Env = []string{"PG_MAJOR=" + major}
	return img
}

// A database's major release moves its data by a dump into a new volume (ADR 0080).
// The plan says what stops, and that the old volume stays; applying it stops the
// rest of the app, dumps the database, loads the dump into the new release on the
// new volume, and starts what stopped, with the dump on record as a backup of the
// old volume. Undoing it puts the old release back on the old volume, as it was.
// Down a release is refused before anything pulls, saying why. A patch goes ahead,
// with a dump first.
func TestADatabasesMajorReleaseMovesByADump(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		for tag, major := range map[string]string{"16.4": "16", "16.5": "16", "17.2": "17", "15.8": "15"} {
			b.e.Registry["postgres:"+tag] = pgOf(major)
		}
		b.run(t, &dockwellv1.ImportComposeRequest{Compose: `name: shop
services:
  db:
    image: postgres:16.4
    environment:
      POSTGRES_PASSWORD: example
    volumes:
      - pgdata:/var/lib/postgresql/data
  web:
    image: alpine:latest
volumes:
  pgdata: {}
`})
		db := b.service(t, "shop", "db")
		scope := db.GetResource().GetScope()
		dir := func(name string) string {
			return b.e.VolumeDir(volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: scope, Name: name}).Name)
		}
		const data = "CREATE TABLE orders (id int);\n"
		if err := os.WriteFile(filepath.Join(dir("pgdata"), "data.sql"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		project := docker.Project(scope)
		webStarts := b.e.Started(b.e.Newest(project, "web").Name)

		move := b.upgrade(t, &dockwellv1.UpgradeServiceRequest{Id: db.GetId(), Image: "postgres:17.2"})
		var moved *dockwellv1.DatabaseMove
		made := false
		for _, s := range move.GetPlan().GetServers()[0].GetSteps() {
			if s.GetBackupFirst() != nil {
				t.Errorf("a move backs its volume up again: %v", s)
			}
			if m := s.GetDatabaseMove(); m != nil {
				moved = m
			}
			if s.GetResource().GetKind() == string(volume.Kind) && s.GetResource().GetName() == "pgdata-17" && s.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_CREATE {
				made = true
			}
		}
		switch {
		case moved == nil:
			t.Fatalf("the plan moves nothing: %v", move.GetPlan().GetServers()[0].GetSteps())
		case moved.GetFromVolume() != "pgdata" || moved.GetToVolume() != "pgdata-17" || moved.GetDatabase() != "PostgreSQL" ||
			moved.GetFromSeries() != "16" || moved.GetToSeries() != "17" || !slices.Equal(moved.GetStops(), []string{"web"}) || moved.GetBackupId() == "":
			t.Errorf("the move = %v", moved)
		case !made:
			t.Errorf("the plan makes no volume pgdata-17: %v", move.GetPlan().GetServers()[0].GetSteps())
		}
		if n := noteOf(move, "database_move"); n == nil || !strings.Contains(n.GetMessage(), "pgdata stays as it was, so undoing this puts PostgreSQL 16 back on it") {
			t.Errorf("the plan's notes = %v", move.GetPlan().GetNotes())
		}
		b.done(t, move)
		now := b.e.Newest(project, "db")
		if now.Config.Image != "postgres:17.2" || !slices.ContainsFunc(now.Mounts, func(m docker.MountPoint) bool { return strings.HasSuffix(m.Name, "_pgdata-17") }) {
			t.Errorf("after the move, db runs %s on %v", now.Config.Image, now.Mounts)
		}
		for _, name := range []string{"pgdata", "pgdata-17"} {
			if got, err := os.ReadFile(filepath.Join(dir(name), "data.sql")); err != nil || !strings.Contains(string(got), data) {
				t.Errorf("%s holds %q, %v, want what the database held", name, got, err)
			}
		}
		if web := b.e.Newest(project, "web"); !web.State.Running || b.e.Started(web.Name) <= webStarts {
			t.Errorf("web didn't stop and start again for the move: running %v", web.State.Running)
		}
		backups, err := backupClient(b.b, b.b.origin).ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(backups.Msg.GetBackups(), func(k *dockwellv1.Backup) bool {
			return k.GetId() == moved.GetBackupId() && k.GetVolume() == "pgdata" && k.GetMethod() == dockwellv1.BackupMethod_BACKUP_METHOD_DUMP
		}) {
			t.Errorf("the move's dump isn't on record as a backup of pgdata: %v", backups.Msg.GetBackups())
		}

		undo, err := b.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: move.GetId()}))
		if err != nil {
			t.Fatalf("undoing the move = %v", err)
		}
		b.done(t, undo.Msg.GetChange())
		back := b.e.Newest(project, "db")
		if back.Config.Image != "postgres:16.4" || !slices.ContainsFunc(back.Mounts, func(m docker.MountPoint) bool { return strings.HasSuffix(m.Name, "_pgdata") }) {
			t.Errorf("after undoing the move, db runs %s on %v", back.Config.Image, back.Mounts)
		}

		pulls := b.e.Pulls
		_, err = b.s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: db.GetId(), Image: "postgres:15.8"}))
		if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "PostgreSQL 15 can't read the data of db, which runs PostgreSQL 16") {
			t.Errorf("moving PostgreSQL from 16 down to 15 = %v", err)
		}
		if b.e.Pulls != pulls {
			t.Error("a refused move pulled an image")
		}

		patch := b.upgrade(t, &dockwellv1.UpgradeServiceRequest{Id: db.GetId(), Image: "postgres:16.5"})
		i := slices.IndexFunc(patch.GetPlan().GetServers()[0].GetSteps(), func(s *dockwellv1.PlanStep) bool { return s.GetBackupFirst() != nil })
		if i < 0 {
			t.Fatalf("a database's update takes no backup first: %v", patch.GetPlan().GetServers()[0].GetSteps())
		}
		if got := patch.GetPlan().GetServers()[0].GetSteps()[i].GetBackupFirst().GetMethod(); got != dockwellv1.BackupMethod_BACKUP_METHOD_DUMP {
			t.Errorf("a database's backup first is a %v", got)
		}
	})
}

// A move whose dump won't load into the new release changes nothing: the database
// goes on as it was, on its volume, what stopped starts again, and no backup goes
// on record, as its dump went with it.
func TestAMoveThatWontLoadChangesNothing(t *testing.T) {
	b := newBench(t, store.SQLite)
	b.e.Registry["postgres:16.4"] = pgOf("16")
	broken := pgOf("17")
	load := broken.Exec
	broken.Exec = func(x dockertest.Exec) int {
		if strings.Contains(strings.Join(x.Cmd, " "), "psql") {
			_, _ = io.WriteString(x.Stderr, "ERROR:  syntax error at or near \"WAT\"\n")
			return 3
		}
		return load(x)
	}
	b.e.Registry["postgres:17.2"] = broken
	b.run(t, &dockwellv1.ImportComposeRequest{Compose: `name: shop
services:
  db:
    image: postgres:16.4
    volumes:
      - pgdata:/var/lib/postgresql/data
  web:
    image: alpine:latest
volumes:
  pgdata: {}
`})
	db := b.service(t, "shop", "db")
	scope := db.GetResource().GetScope()
	if err := os.WriteFile(filepath.Join(b.e.VolumeDir(volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: scope, Name: "pgdata"}).Name), "data.sql"), []byte("SELECT 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project := docker.Project(scope)
	before := b.e.Newest(project, "db").ID
	move := b.upgrade(t, &dockwellv1.UpgradeServiceRequest{Id: db.GetId(), Image: "postgres:17.2"})
	got := applied(t, b.c, move)
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(got.GetError(), "the dump didn't load into PostgreSQL 17") ||
		!strings.Contains(got.GetError(), "Nothing changed: db runs PostgreSQL 16 on pgdata as before") {
		t.Fatalf("the move ended %v: %s", got.GetState(), got.GetError())
	}
	if now := b.e.Newest(project, "db"); now.ID != before || !now.State.Running {
		t.Errorf("db isn't the container it was, running: %s, %v", now.Image, now.State.Running)
	}
	if !b.e.Newest(project, "web").State.Running {
		t.Error("web didn't start again")
	}
	backups, err := backupClient(b.b, b.b.origin).ListBackups(t.Context(), connect.NewRequest(&dockwellv1.ListBackupsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups.Msg.GetBackups()) != 0 {
		t.Errorf("a move that didn't happen left backups on record: %v", backups.Msg.GetBackups())
	}
}

// A move's new volume takes a name no volume of its environment has had, and a
// volume with options of its own, which Dockwell can't make one like, is refused.
func TestADatabaseMoveNamesAFreeVolumeAndRefusesOneItCantCopy(t *testing.T) {
	b := newBench(t, store.SQLite)
	b.e.Registry["postgres:16.4"] = pgOf("16")
	b.e.Registry["postgres:17.2"] = pgOf("17")
	b.run(t, &dockwellv1.ImportComposeRequest{Compose: `name: shop
services:
  db:
    image: postgres:16.4
    volumes:
      - pgdata:/var/lib/postgresql/data
  cache:
    image: postgres:16.4
    volumes:
      - scratch:/var/lib/postgresql/data
volumes:
  pgdata: {}
  pgdata-17: {}
  scratch:
    driver_opts:
      type: tmpfs
      device: tmpfs
`})
	move := b.upgrade(t, &dockwellv1.UpgradeServiceRequest{Id: b.service(t, "shop", "db").GetId(), Image: "postgres:17.2"})
	i := slices.IndexFunc(move.GetPlan().GetServers()[0].GetSteps(), func(s *dockwellv1.PlanStep) bool { return s.GetDatabaseMove() != nil })
	if i < 0 || move.GetPlan().GetServers()[0].GetSteps()[i].GetDatabaseMove().GetToVolume() != "pgdata-17-2" {
		t.Errorf("with pgdata-17 taken, the move = %v", move.GetPlan().GetServers()[0].GetSteps())
	}
	_, err := b.s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: b.service(t, "shop", "cache").GetId(), Image: "postgres:17.2"}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "scratch mounts what its driver or options say") {
		t.Errorf("moving data on a volume with options = %v", err)
	}
}

// A service of a catalog install updates its install, with what the release leaves
// and goes to, and takes no image of its own.
func TestAnUpdateOfAnInstallsServiceUpgradesTheInstall(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		u := startUpgrading(t, backend, testEntry(1, "1.0", firstRelease, ""))
		u.release(testEntry(2, "2.0", secondRelease, ""))
		s := dockwellv1connect.NewServiceServiceClient(u.b, u.b.origin, connect.WithProtoJSON())
		list, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var web string
		for _, svc := range list.Msg.GetServices() {
			if svc.GetResource().GetName() == "web" {
				web = svc.GetId()
			}
		}

		_, err = s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: web, Image: "ghcr.io/acme/shop:1.5"}))
		if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "installed from the catalog") {
			t.Errorf("an image for an install's service = %v", err)
		}

		res, err := s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: web}))
		if err != nil {
			t.Fatal(err)
		}
		up := res.Msg.GetChange()
		intent := up.GetPlan().GetIntent()
		if intent.GetKind() != dockwellv1.IntentKind_INTENT_KIND_UPGRADE || intent.GetFromRelease() != "1.0" || intent.GetRelease() != "2.0" || intent.GetResourceId() != u.install().GetProjectId() {
			t.Errorf("the plan's intent = %v", intent)
		}
		if !slices.ContainsFunc(up.GetPlan().GetServers()[0].GetSteps(), func(s *dockwellv1.PlanStep) bool { return s.GetBackupFirst() != nil }) {
			t.Error("an install's update takes no backup first")
		}
		u.applied(up)
		if got := u.web(); got.Config.Image != secondRelease {
			t.Errorf("web runs %s", got.Config.Image)
		}
	})
}

// An update that names no image takes what ADR 0079's keeper found, the first a
// person didn't skip: postgres:16.3 goes to 16.4; with 16.4 skipped, 17.2 is next,
// a major release, whose move by a dump this database's data, in its image's own
// volume, can't take; and snoozed, it knows of none (ADR 0080).
func TestAnUpdateWithNoImageTakesWhatTheKeeperFound(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: updatesRegistry(t)}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	e.Registry["postgres:16.3"] = dockertest.Image{Digest: pg163, Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=16"}}}
	e.Registry["postgres:16.4"] = dockertest.Image{Digest: pg164, Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=16"}}}
	c := changeClient(b, b.origin)
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Run a database", Edits: []*dockwellv1.Edit{serviceEdit("db", "postgres:16.3", map[string]string{"POSTGRES_PASSWORD": "example"})},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := applied(t, c, made.Msg.GetChange()); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the deploy ended %v: %s", got.GetState(), got.GetError())
	}
	s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	u := dockwellv1connect.NewServiceUpdateServiceClient(b, b.origin, connect.WithProtoJSON())
	if _, err := s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: "db"})); reasonOf(err) != "up_to_date" {
		t.Errorf("before any check, an update with no image = %v", err)
	}
	if _, err := u.CheckServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.CheckServiceUpdatesRequest{Server: "hz-1"})); err != nil {
		t.Fatal(err)
	}

	res, err := s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: "db"}))
	if err != nil {
		t.Fatal(err)
	}
	if intent := res.Msg.GetChange().GetPlan().GetIntent(); intent.GetImage() != "postgres:16.4" || intent.GetFromImage() != "postgres:16.3" {
		t.Errorf("with no image, the update goes %s to %s", intent.GetFromImage(), intent.GetImage())
	}
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: res.Msg.GetChange().GetId()})); err != nil {
		t.Fatal(err)
	}

	if _, err := u.SkipServiceUpdate(t.Context(), connect.NewRequest(&dockwellv1.SkipServiceUpdateRequest{Id: "db", Version: "16.4"})); err != nil {
		t.Fatal(err)
	}
	_, err = s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: "db"}))
	if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "PostgreSQL 17 moves the data of db, which runs PostgreSQL 16, by a dump into a new volume, and its data isn't in a volume of its environment") {
		t.Errorf("with 16.4 skipped, the update to 17.2 = %v", err)
	}

	if _, err := u.SnoozeServiceUpdates(t.Context(), connect.NewRequest(&dockwellv1.SnoozeServiceUpdatesRequest{Id: "db", Duration: "7d"})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: "db"})); reasonOf(err) != "up_to_date" {
		t.Errorf("snoozed, an update with no image = %v", err)
	}
}
