// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
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

// A database's major release is never a plain swap: the plan is refused before
// anything pulls, saying why, whichever way it's asked for. A patch goes ahead,
// with a dump first.
func TestADatabasesMajorReleaseIsNotAPlainSwap(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		b.e.Registry["postgres:16.4"] = dockertest.Image{Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=16"}}}
		b.e.Registry["postgres:16.5"] = dockertest.Image{Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=16"}}}
		b.e.Registry["postgres:17.2"] = dockertest.Image{Volumes: []string{"/var/lib/postgresql/data"}, Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=17"}}}
		b.run(t, &dockwellv1.ImportComposeRequest{Compose: `name: shop
services:
  db:
    image: postgres:16.4
    environment:
      POSTGRES_PASSWORD: example
    volumes:
      - pgdata:/var/lib/postgresql/data
volumes:
  pgdata: {}
`})
		db := b.service(t, "shop", "db")
		pulls := b.e.Pulls
		for _, ask := range []func() error{
			func() error {
				_, err := b.s.UpgradeService(t.Context(), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{Id: db.GetId(), Image: "postgres:17.2"}))
				return err
			},
			func() error {
				_, err := b.s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: db.GetId(), Image: "postgres:17.2"}))
				return err
			},
		} {
			err := ask()
			if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "PostgreSQL 17 can't start on the data of db, which runs PostgreSQL 16") {
				t.Errorf("moving PostgreSQL from 16 to 17 = %v", err)
			}
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
