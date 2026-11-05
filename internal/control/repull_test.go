// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A tag at one image, as a registry has it.
func moveLatest(e *dockertest.Engine, digest string) {
	e.Registry[latest] = dockertest.Image{Digest: digest, Config: docker.Config{Cmd: []string{"shop"}}}
}

func noteOf(c *dockwellv1.Change, reason string) *dockwellv1.PlanNote {
	for _, n := range c.GetPlan().GetNotes() {
		if n.GetReason() == reason {
			return n
		}
	}
	return nil
}

func (b *bench) deploy(t *testing.T, id, image string) *dockwellv1.Change {
	t.Helper()
	res, err := b.s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: id, Image: image}))
	if err != nil {
		t.Fatalf("deploying %s: %v", image, err)
	}
	return res.Msg.GetChange()
}

func (b *bench) revisions(t *testing.T, id string) []*dockwellv1.ServiceRevision {
	t.Helper()
	res, err := b.s.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetRevisions()
}

func digestsOf(r *dockwellv1.ServiceRevision) []string {
	var out []string
	for _, d := range r.GetDeployments() {
		out = append(out, d.GetDigest())
	}
	return out
}

var errDiskFull = errors.New("no space left on device")

const (
	latest  = "ghcr.io/acme/shop:latest"
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// runLatest is a service on a moving tag, at digestA.
func runLatest(t *testing.T, b *bench) (name string, svc *dockwellv1.ServiceInfo) {
	t.Helper()
	moveLatest(b.e, digestA)
	env := b.run(t, &dockwellv1.ImportComposeRequest{Compose: "name: shop\nservices:\n  web:\n    image: " + latest + "\n"})
	return docker.ContainerName(docker.Project(env), "web"), b.service(t, "shop", "web")
}

// Deploying the tag a service already names is how a moving tag gets picked up:
// pull first, and replace only if that found a newer image.
func TestDeployingTheSameTagPullsAgain(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		name, web := runLatest(t, b)
		before := b.e.Container(name).ID
		pulls := b.e.Pulls

		// Planning touches nothing, and says what it means.
		plan := b.deploy(t, web.GetId(), latest)
		if b.e.Pulls != pulls {
			t.Error("planning pulled the image")
		}
		note := noteOf(plan, "pulls_again")
		if note == nil || note.GetMessage() != "Pulls "+latest+" again, and replaces web only if that's a newer image" {
			t.Errorf("the plan's notes = %v", plan.GetPlan().GetNotes())
		}
		steps := plan.GetPlan().GetServers()[0].GetSteps()
		if len(steps) != 1 || !steps[0].GetPull() || steps[0].GetDowntime().AsDuration() == 0 {
			t.Errorf("the plan's step = %v, want a pull that says what it may take", steps)
		}

		// Nothing newer: nothing happens, and it says so.
		applied := b.done(t, plan)
		if b.e.Pulls != pulls+1 {
			t.Errorf("applying pulled %d times", b.e.Pulls-pulls)
		}
		if got := b.e.Container(name); got.ID != before || !got.State.Running {
			t.Error("with nothing newer, the container was replaced")
		}
		if got := applied.GetSteps(); len(got) != 1 || !strings.Contains(got[0].GetMessage(), latest+" is already the newest image, so web stays as it is") {
			t.Errorf("the steps say %v", got)
		}
		if revs := b.revisions(t, web.GetId()); len(revs) != 1 || !slices.Equal(digestsOf(revs[0]), []string{digestA}) {
			t.Errorf("with nothing newer, the revisions are %v", revs)
		}

		// The tag moves: it's replaced, with a revision saying what ran each time.
		moveLatest(b.e, digestB)
		b.done(t, b.deploy(t, web.GetId(), latest))
		if got := b.e.Container(name); got.ID == before || !got.State.Running {
			t.Error("with a newer image, the container wasn't replaced")
		}
		revs := b.revisions(t, web.GetId())
		if len(revs) != 2 || revs[0].GetImage() != latest || !slices.Equal(digestsOf(revs[0]), []string{digestB}) || !slices.Equal(digestsOf(revs[1]), []string{digestA}) {
			t.Fatalf("the revisions are %v", revs)
		}
		// r1 reads the same and isn't, so going back to it is a real choice.
		if revs[1].GetMatchesLatest() {
			t.Error("r1 ran another image, and reads as matching the latest")
		}
		if got := b.service(t, "shop", "web").GetRevision(); got != 2 {
			t.Errorf("the service is on r%d", got)
		}
		if plain := b.file(t, &dockwellv1.ImportComposeRequest{Compose: "name: shop\nservices:\n  web:\n    image: " + latest + "\n"}); len(plain.GetPlan().GetServers()) != 0 {
			t.Errorf("re-importing the same file pulls or replaces: %v", plain.GetPlan().GetServers())
		}
	})
}

// A rollback of a moving tag goes back to what it really ran, and says so; where
// that's gone, it says that, and still rolls back.
func TestARollbackOfAMovingTagPutsBackWhatRan(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		name, web := runLatest(t, b)
		moveLatest(b.e, digestB)
		b.done(t, b.deploy(t, web.GetId(), latest))

		back, err := b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if err != nil {
			t.Fatal(err)
		}
		c := back.Msg.GetChange()
		if note := noteOf(c, "digest_pinned"); note == nil || !strings.Contains(note.GetMessage(), "as revision 1 ran it, sha256:aaaaaaaaaaaa, even if the tag has moved since") {
			t.Errorf("the plan's notes = %v", c.GetPlan().GetNotes())
		}
		b.done(t, c)
		ctr := b.e.Container(name)
		if ctr.Config.Image != latest+"@"+digestA || !ctr.State.Running {
			t.Errorf("it runs %s", ctr.Config.Image)
		}
		revs := b.revisions(t, web.GetId())
		if !slices.Equal(digestsOf(revs[0]), []string{digestA}) {
			t.Errorf("the rollback's digests are %v", digestsOf(revs[0]))
		}
		// It went back to what r1 ran, so r1 matches, and r2 doesn't.
		if !revs[2].GetMatchesLatest() || revs[1].GetMatchesLatest() {
			t.Errorf("r1 matches %t, r2 %t", revs[2].GetMatchesLatest(), revs[1].GetMatchesLatest())
		}
		_, err = b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if reasonOf(err) != "cant_undo" {
			t.Errorf("going back to what it runs = %v", err)
		}

		// Deploying the tag again leaves the pin behind, whatever the tag is now.
		b.done(t, b.deploy(t, web.GetId(), latest))
		if got := b.e.Container(name).Config.Image; got != latest {
			t.Errorf("after deploying the tag again, it runs %s", got)
		}

		// A digest names one image, so deploying it again can't change anything.
		pin := latest + "@" + digestB
		b.done(t, b.deploy(t, web.GetId(), pin))
		_, err = b.s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: web.GetId(), Image: pin}))
		if reasonOf(err) != "invalid" || !strings.Contains(err.Error(), "already runs "+pin) || strings.Contains(err.Error(), "The service") {
			t.Errorf("deploying the digest it runs = %v", err)
		}

		// The registry and the server have let go of what r1 ran.
		b.e.Forget(digestA)
		gone, err := b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if err != nil {
			t.Fatalf("rolling back with the image gone = %v", err)
		}
		c = gone.Msg.GetChange()
		if note := noteOf(c, "digest_gone"); note == nil || !strings.Contains(note.GetMessage(), "is neither on the server nor in its registry, so web runs what "+latest+" is now") {
			t.Errorf("the plan's notes = %v", c.GetPlan().GetNotes())
		}
		if noteOf(c, "digest_pinned") != nil {
			t.Error("the plan says it pins what it can't")
		}
		b.done(t, c)
		if got := b.e.Container(name).Config.Image; got != latest {
			t.Errorf("it runs %s, want the tag", got)
		}
		// Now that is what it runs, so there's nothing to go back to, which says why.
		_, err = b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if reasonOf(err) != "cant_undo" || !strings.Contains(err.Error(), "already runs what "+latest+" is now") {
			t.Errorf("going back to an image that's gone, on the tag it runs = %v", err)
		}

		// Leaving a pin for the tag it already is changes the spec, so that goes
		// ahead, though the image is the one that runs.
		pin = latest + "@" + digestB
		b.done(t, b.deploy(t, web.GetId(), pin))
		b.done(t, b.deploy(t, web.GetId(), latest))
		if got := b.e.Container(name).Config.Image; got != latest {
			t.Errorf("after leaving a pin the tag already matches, it runs %s", got)
		}
	})
}

// A newer image that doesn't run is rolled back to the image before it, which the
// tag no longer names.
func TestANewerImageThatFailsGoesBackToTheOneBefore(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		name, web := runLatest(t, b)
		b.e.Registry[latest] = dockertest.Image{Digest: digestB, Exits: true, Config: docker.Config{Cmd: []string{"shop"}}}
		got := applied(t, b.c, b.deploy(t, web.GetId(), latest))
		if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || got.GetRolledBackBy() == "" {
			t.Fatalf("the deploy ended %v, rolled back by %q: %s", got.GetState(), got.GetRolledBackBy(), got.GetError())
		}
		waitFor(t, "the rollback to end", func() bool {
			c, err := b.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: got.GetRolledBackBy()}))
			return err == nil && c.Msg.GetChange().GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED
		})
		ctr := b.e.Container(name)
		if ctr.Config.Image != latest+"@"+digestA || !ctr.State.Running {
			t.Errorf("after the rollback it runs %s, running %t", ctr.Config.Image, ctr.State.Running)
		}
		revs := b.revisions(t, web.GetId())
		if len(revs) != 3 || !slices.Equal(digestsOf(revs[1]), []string{digestB}) || !slices.Equal(digestsOf(revs[0]), []string{digestA}) {
			t.Errorf("the revisions are %v", revs)
		}
	})
}

// A pull that can't reach the image leaves what runs alone, with no revision, and
// nothing to roll back.
func TestAPullThatFailsLeavesWhatRuns(t *testing.T) {
	b := newBench(t, store.SQLite)
	name, web := runLatest(t, b)
	before := b.e.Container(name).ID
	delete(b.e.Registry, latest)
	got := applied(t, b.c, b.deploy(t, web.GetId(), latest))
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED || !strings.Contains(got.GetError(), "can't pull "+latest+" again") ||
		!strings.Contains(got.GetError(), "so nothing was replaced") {
		t.Fatalf("the deploy ended %v: %s", got.GetState(), got.GetError())
	}
	if ctr := b.e.Container(name); ctr.ID != before || !ctr.State.Running {
		t.Error("a failed pull touched what runs")
	}
	if revs := b.revisions(t, web.GetId()); len(revs) != 1 {
		t.Errorf("the revisions are %v", revs)
	}
}

// Undoing a pull that replaced what ran goes back to what ran before it.
func TestUndoingAPullPutsBackWhatRan(t *testing.T) {
	b := newBench(t, store.SQLite)
	name, web := runLatest(t, b)
	moveLatest(b.e, digestB)
	pull := b.done(t, b.deploy(t, web.GetId(), latest))
	undo, err := b.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: pull.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	b.done(t, undo.Msg.GetChange())
	if got := b.e.Container(name).Config.Image; got != latest+"@"+digestA {
		t.Errorf("after the undo it runs %s", got)
	}
}

// What was pulled reaches the record even when something stops the replace.
func TestAReplaceThatFailsKeepsWhatItFound(t *testing.T) {
	b := newBench(t, store.SQLite)
	_, web := runLatest(t, b)
	moveLatest(b.e, digestB)
	var refuse atomic.Bool
	refuse.Store(true)
	b.e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create") && refuse.CompareAndSwap(true, false) {
			return errDiskFull
		}
		return nil
	})
	got := applied(t, b.c, b.deploy(t, web.GetId(), latest))
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
		t.Fatalf("the deploy ended %v: %s", got.GetState(), got.GetError())
	}
	revs := b.revisions(t, web.GetId())
	if len(revs) < 2 || !slices.Contains(digestsOf(revs[len(revs)-2]), digestB) {
		t.Errorf("the revisions don't say what it pulled: %v", revs)
	}
}

// An image deployed before digests were kept has none on record, so what a pull
// replaces says what it was, as a failed replace has to go back to it.
func TestAPullSaysWhatItReplaces(t *testing.T) {
	b := newBench(t, store.SQLite)
	name, web := runLatest(t, b)
	b.f.execSQL(t, `UPDATE deployments SET digest = ''`)
	b.e.Registry[latest] = dockertest.Image{Digest: digestB, Exits: true, Config: docker.Config{Cmd: []string{"shop"}}}
	got := applied(t, b.c, b.deploy(t, web.GetId(), latest))
	if got.GetRolledBackBy() == "" {
		t.Fatalf("the deploy ended %v with no rollback: %s", got.GetState(), got.GetError())
	}
	waitFor(t, "the rollback to end", func() bool {
		c, err := b.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: got.GetRolledBackBy()}))
		return err == nil && c.Msg.GetChange().GetState() == dockwellv1.ChangeState_CHANGE_STATE_APPLIED
	})
	if ctr := b.e.Container(name); ctr.Config.Image != latest+"@"+digestA {
		t.Errorf("after the rollback it runs %s, want what ran before the pull", ctr.Config.Image)
	}
}

// Each server pulls for itself, so one can find a newer image and another not.
// The revision that makes holds both, with what each ran.
func TestAPullOnTwoServersRecordsBoth(t *testing.T) {
	b := newBench(t, store.SQLite)
	other := b.f.addDockerServer(t)
	for _, e := range []*dockertest.Engine{b.e, other} {
		moveLatest(e, digestA)
	}
	var ids []string
	res, err := dockwellv1connect.NewServerServiceClient(b.b, b.b.origin, connect.WithProtoJSON()).ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Msg.GetServers() {
		ids = append(ids, s.GetId())
	}
	b.run(t, &dockwellv1.ImportComposeRequest{Compose: "name: shop\nservices:\n  web:\n    image: " + latest + "\n", Servers: ids})
	web := b.service(t, "shop", "web")
	// Only one server's registry has moved, whichever it pulls first.
	moveLatest(other, digestB)
	b.done(t, b.deploy(t, web.GetId(), latest))
	revs := b.revisions(t, web.GetId())
	if len(revs) != 2 {
		t.Fatalf("the revisions are %v", revs)
	}
	got := digestsOf(revs[0])
	slices.Sort(got)
	if !slices.Equal(got, []string{digestA, digestB}) {
		t.Errorf("r2's digests are %v, want one from each server", got)
	}
	for _, d := range revs[0].GetDeployments() {
		if d.GetState() != dockwellv1.DeploymentState_DEPLOYMENT_STATE_LIVE {
			t.Errorf("r2 on %s is %v", d.GetServerName(), d.GetState())
		}
	}
	// It ran different images, so a rollback to it can't pin one, and says so.
	moveLatest(b.e, digestB)
	b.done(t, b.deploy(t, web.GetId(), "ghcr.io/acme/shop:1.5"))
	back, err := b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 2}))
	if err != nil {
		t.Fatal(err)
	}
	if note := noteOf(back.Msg.GetChange(), "digest_gone"); note == nil || !strings.Contains(note.GetMessage(), "ran different images") {
		t.Errorf("the plan's notes = %v", back.Msg.GetChange().GetPlan().GetNotes())
	}
}

// What a file would be refused for whatever its variables hold comes before
// asking for them, through the API as a form reads it.
func TestCheckComposeRefusesBeforeAskingForValues(t *testing.T) {
	b := newBench(t, store.SQLite)
	res, err := b.cc.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{
		Compose: "services:\n  web:\n    image: nginx:${TAG}\n    network_mode: host\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	r := res.Msg.GetRefusal()
	if !strings.Contains(r.GetMessage(), "network_mode host") || r.GetLine() != 4 || r.GetEndLine() != 4 {
		t.Errorf("the refusal = %v", r)
	}
	if got := res.Msg.GetVariables(); len(got) != 1 || got[0].GetName() != "TAG" {
		t.Errorf("the variables = %v", got)
	}
	near, err := b.cc.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: "services:\n  web:\n    image nginx\n    user: x\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if r := near.Msg.GetRefusal(); r.GetLine() != 3 || r.GetEndLine() != 4 {
		t.Errorf("the refusal = %v, want lines 3 to 4", r)
	}
}

// A revision with the image that runs, on the same tag, is nothing to go back to,
// and pinning it for the going back would replace what runs for nothing.
func TestGoingBackToWhatRunsIsRefusedHoweverItIsWritten(t *testing.T) {
	b := newBench(t, store.SQLite)
	_, web := runLatest(t, b)
	b.done(t, b.deploy(t, web.GetId(), "ghcr.io/acme/shop:1.5"))
	b.done(t, b.deploy(t, web.GetId(), latest))
	_, err := b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
	if reasonOf(err) != "cant_undo" {
		t.Errorf("going back to r1, which is what runs = %v", err)
	}
}
