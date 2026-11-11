// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// stand is a server with a fake Docker engine, the agent's own drivers, its cache
// of desired state and its journal, and a reconciler that has had no stream for an
// hour.
type stand struct {
	dir      string
	e        *dockertest.Engine
	local    resource.Local
	store    *desired.Store
	journal  *desired.Journal
	presence *presence
	held     *unfinished
	r        *reconciler
	// revision is the revision label another gives what it makes: a number,
	// "none" for no label, or empty for the one web has. adopted has it marked
	// as taken on, and otherImage made from another image than web's.
	revision            string
	adopted, otherImage bool
}

const standEnv = "env_01KAT4ZR6E3V0Y5XBKZB5D9Q6M"

func newStand(t *testing.T) *stand {
	t.Helper()
	e := dockertest.New(t)
	e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx", "-g", "daemon off;"}}}
	kept := &volume.Kept{}
	rt := &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second, Deleted: kept}
	c := container.Driver{Runtime: rt}
	drivers := resource.NewRegistry(&c, &service.Driver{Containers: c}, &volume.Driver{Runtime: &volume.Docker{Client: e.Client(), Kept: kept}},
		&network.Driver{Client: e.Client()})
	dir := t.TempDir()
	store, err := desired.Open(dir, "srv_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := desired.OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &stand{dir: dir, e: e, local: resource.Local{Drivers: drivers}, store: store, journal: journal, presence: newPresence(time.Now().Add(-time.Hour)), held: newUnfinished()}
	s.r = newReconciler(Config{Logger: slog.New(slog.DiscardHandler), Drivers: drivers, Desired: store, Journal: journal, Docker: e.Client()}, s.presence, s.held)
	return s
}

// run applies spec as a step would, with values, and keeps what it left, as the
// agent does after a step that passed.
func (s *stand) run(t *testing.T, ref resource.Ref, spec proto.Message, values map[string]string) {
	t.Helper()
	if err := s.local.Drivers.Validate(ref.Kind, spec); err != nil {
		t.Fatal(err)
	}
	steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	// As a step applies it: for the revision and change its record then says.
	ctx := resource.WithDeploy(resource.WithValues(t.Context(), values), resource.Deploy{Revision: 1, Change: "chg_1"})
	if err := s.local.Apply(ctx, steps[0], spec); err != nil {
		t.Fatal(err)
	}
	b, err := resource.Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	img, err := s.local.RunsImage(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.Keep(desired.Record{Ref: ref, Revision: 1, Change: "chg_1", Spec: append([]byte{}, b...), Values: values, Image: img}); err != nil {
		t.Fatal(err)
	}
}

// pass goes through what the agent keeps, acting.
func (s *stand) pass(t *testing.T) {
	t.Helper()
	s.r.pass(t.Context(), s.presence.away(s.r.now(), s.r.after))
}

// said is the last thing the journal says of ref.
func (s *stand) said(ref resource.Ref) desired.Entry {
	entries, _, _ := s.journal.Pending()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Ref == ref {
			return entries[i]
		}
	}
	return desired.Entry{}
}

var (
	webRef = resource.Ref{Kind: container.Kind, Scope: "srv_1", Name: "web"}
	nginx  = &resourcev1.Container{Image: "nginx:1.27"}
)

// A container that's gone is made again from its record while the control plane is
// away, and the journal says so.
func TestAContainerThatsGoneIsMadeAgain(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.e.Remove("web")
	s.pass(t)
	c := s.e.Container("web")
	if c == nil || !c.State.Running {
		t.Fatalf("after a pass the container is %+v", c)
	}
	if got := s.said(webRef); got.What != desired.Made {
		t.Errorf("the journal says %+v", got)
	}
	if marker, receipt := c.Config.Labels[container.LabelReconcile], s.said(webRef).ID; marker == "" || marker != receipt {
		t.Errorf("creation marker %q, want its durable receipt %q", marker, receipt)
	}
	// It's the deploy its record ran, for its logs and for the control plane.
	if rev, chg := c.Config.Labels[container.LabelRevision], c.Config.Labels[container.LabelChange]; rev != "1" || chg != "chg_1" {
		t.Errorf("made again labelled revision %q, change %q; want 1, chg_1", rev, chg)
	}
}

// A kill can land after Docker created the container but before its start or
// receipt. Recovery identifies that creation, never replaces it, and does not
// start it if a person stopped it in the meantime.
func TestAReconcileInterruptedByAKillIsAccountedFor(t *testing.T) {
	for _, tc := range []struct {
		name, marker                string
		started, stopped, connected bool
	}{
		{name: "before first start", marker: "own"},
		{name: "before first start, already connected", marker: "own", connected: true},
		{name: "before receipt, already connected", marker: "own", started: true, connected: true},
		{name: "stopped by a person", marker: "own", started: true, stopped: true},
		{name: "another container", marker: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			old := s.e.Container("web")
			s.e.Remove("web")
			e, err := s.journal.Begin(webRef, desired.Made, "Its recorded image was retained", "", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			marker := e.ID
			if tc.marker != "own" {
				marker = "another-action"
			}
			old.Config.Labels[container.LabelReconcile] = marker
			id, err := s.e.Client().CreateContainer(t.Context(), "web", &docker.CreateRequest{Config: old.Config, HostConfig: old.HostConfig})
			if err != nil {
				t.Fatal(err)
			}
			if tc.started {
				if err := s.e.Client().StartContainer(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			}
			if tc.stopped {
				s.e.Stop("web")
			}
			// Reopen only what was durable when the process died.
			s.journal, err = desired.OpenJournal(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			s.r.cfg.Journal = s.journal
			if tc.connected {
				s.presence.set(true, time.Now())
			}
			s.r.recoverActions(t.Context())
			s.pass(t)
			s.r.recoverActions(t.Context())
			c := s.e.Container("web")
			if tc.connected && !tc.started {
				entries, _, _ := s.journal.Pending()
				if c.State.Running || len(entries) != 0 || len(s.journal.Doing()) != 1 {
					t.Fatalf("recovery acted while connected: %+v, receipts %+v, pending %+v", c.State, entries, s.journal.Doing())
				}
				s.presence.set(false, time.Now().Add(-time.Hour))
				s.pass(t)
				c = s.e.Container("web")
			}
			wantRunning := tc.marker == "own" && !tc.stopped
			if c.ID != id || c.State.Running != wantRunning {
				t.Fatalf("after recovery: %+v, want container %s, running %t", c, id, wantRunning)
			}
			entries, _, _ := s.journal.Pending()
			what := desired.Made
			if tc.marker != "own" {
				what = desired.Waiting
			}
			if len(entries) != 1 || entries[0].ID != e.ID || entries[0].What != what || !entries[0].At.Equal(e.At) || len(s.journal.Doing()) != 0 {
				t.Fatalf("receipts %+v, pending %+v; want once under %+v", entries, s.journal.Doing(), e)
			}
		})
	}
}

func TestReconcileWaitsUntilItsPendingActionCanBeKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory")
	}
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.e.Remove("web")
	folder := filepath.Join(s.dir, desired.Folder)
	if err := os.Chmod(folder, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(folder, 0o700) })
	s.pass(t)
	if c := s.e.Container("web"); c != nil {
		t.Fatal("Docker was changed without a durable pending receipt")
	}
	if err := os.Chmod(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	s.pass(t)
	if c := s.e.Container("web"); c == nil || !c.State.Running {
		t.Fatalf("once its receipt can be kept: %+v", c)
	}
}

func TestAnInterruptedStartLeavesStoppedAndReplacedContainersAlone(t *testing.T) {
	for _, state := range []string{"running", "stopped", "replaced"} {
		t.Run(state, func(t *testing.T) {
			s := newStand(t)
			s.run(t, webRef, nginx, nil)
			target := s.e.Container("web").ID
			e, err := s.journal.Begin(webRef, desired.Started, "", target, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if state == "replaced" {
				s.e.Remove("web")
				s.run(t, webRef, nginx, nil)
			}
			if state != "running" {
				s.e.Stop("web")
			}
			s.journal, err = desired.OpenJournal(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			s.r.cfg.Journal = s.journal
			s.presence.set(true, time.Now())
			s.r.recoverActions(t.Context())
			entries, _, _ := s.journal.Pending()
			want := desired.Waiting
			if state == "running" {
				want = desired.Started
			}
			if len(entries) != 1 || entries[0].ID != e.ID || entries[0].What != want || len(s.journal.Doing()) != 0 {
				t.Fatalf("recovered receipts %+v, pending %+v", entries, s.journal.Doing())
			}
			if c := s.e.Container("web"); c.State.Running != (state == "running") {
				t.Fatalf("recovery changed how the container stood: %+v", c.State)
			}
		})
	}
}

func TestAnInterruptedCreationsFirstStartHasTheSameRetryLimit(t *testing.T) {
	s := newStand(t)
	e, err := s.journal.Begin(webRef, desired.Made, "", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.r.now = func() time.Time { return now }
	rv := &failingStart{}
	st := resource.RunState{Exists: true, Created: true, Reconcile: e.ID}
	for range 30 {
		s.r.finishAction(t.Context(), rv, webRef, st, true)
		now = now.Add(lastRetry)
	}
	if rv.starts != startTries {
		t.Fatalf("attempted %d starts after a kill, want %d", rv.starts, startTries)
	}
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "in 10 tries") {
		t.Errorf("the journal says %+v", got)
	}
}

// A service is made again with the values its spec uses, from its record.
func TestAServiceIsMadeAgainWithItsValues(t *testing.T) {
	s := newStand(t)
	api := resource.Ref{Kind: service.Kind, Scope: standEnv, Name: "api"}
	svc := &resourcev1.Service{
		Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}, Secrets: map[string]string{"TOKEN": "API_TOKEN"}, Revisions: map[string]int64{"TOKEN": 1},
	}
	s.run(t, api, svc, map[string]string{"TOKEN": "kept-s3cret"})
	name := service.ContainerName(api)
	s.e.Remove(name)
	s.pass(t)
	c := s.e.Container(name)
	if c == nil || !c.State.Running || !strings.Contains(strings.Join(c.Config.Env, " "), "TOKEN=kept-s3cret") {
		t.Fatalf("after a pass the service's container is %+v", c)
	}
}

// Nothing is put back while a stream is up, nor before the agent has gone without
// one for a while: then it only notes what it sees.
func TestNothingIsPutBackWhileTheControlPlaneLeads(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.e.Remove("web")
	s.presence.set(true, time.Now())
	s.r.pass(t.Context(), true)
	if s.e.Container("web") != nil {
		t.Fatal("made it again with a stream up")
	}
	// Nor is anything noted while the control plane leads.
	other := resource.Ref{Kind: container.Kind, Scope: "srv_1", Name: "other"}
	s.run(t, other, nginx, nil)
	if err := s.e.Client().StopContainer(t.Context(), "other", time.Second); err != nil {
		t.Fatal(err)
	}
	s.r.pass(t.Context(), false)
	if rec, _, _ := s.store.Read(other); rec.Stopped {
		t.Fatal("noted what it saw with a stream up")
	}
	s.presence.set(false, time.Now())
	s.pass(t)
	if s.e.Container("web") != nil {
		t.Fatal("made it again within a minute of the stream ending")
	}
	if entries, _, _ := s.journal.Pending(); len(entries) != 0 {
		t.Errorf("wrote down %+v before acting", entries)
	}
}

// A container last seen stopped isn't made again, as that would start it.
func TestAStoppedContainerThatsGoneWaits(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
		t.Fatal(err)
	}
	s.pass(t) // sees it stopped
	s.e.Remove("web")
	s.pass(t)
	if s.e.Container("web") != nil {
		t.Fatal("made a stopped container again, which starts it")
	}
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "It was stopped") {
		t.Errorf("the journal says %+v", got)
	}
}

// What mounts a volume that's gone waits, and so does the volume: Docker would
// make it again empty.
func TestAMissingVolumeIsntMadeAgain(t *testing.T) {
	s := newStand(t)
	data := resource.Ref{Kind: volume.Kind, Scope: standEnv, Name: "data"}
	s.run(t, data, &resourcev1.Volume{}, nil)
	api := resource.Ref{Kind: service.Kind, Scope: standEnv, Name: "api"}
	s.run(t, api, &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}},
	}}}, nil)
	s.e.Remove(service.ContainerName(api))
	if err := s.e.Client().RemoveVolume(t.Context(), volume.TargetOf(data).Name); err != nil {
		t.Fatal(err)
	}
	s.pass(t)
	if s.e.Container(service.ContainerName(api)) != nil {
		t.Fatal("made a container whose volume is gone")
	}
	if got := s.said(api); got.What != desired.Waiting || !strings.Contains(got.Reason, "The volume data it mounts is gone") {
		t.Errorf("the journal says of api %+v", got)
	}
	if got := s.said(data); got.What != desired.Waiting || !strings.Contains(got.Reason, "doesn't make a volume again") {
		t.Errorf("the journal says of data %+v", got)
	}
}

// A tag that moved since is moved back to the image the record ran, so what's made
// again runs that image, not what the tag names now.
func TestATagThatMovedIsMovedBack(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	ran := s.e.Container("web").Image
	s.e.Registry["nginx:1.27"] = dockertest.Image{Digest: "sha256:" + strings.Repeat("7e", 32), Config: docker.Config{Cmd: []string{"nginx", "-g", "daemon off;"}, Env: []string{"NEWER=1"}}}
	if err := s.e.Client().PullImage(t.Context(), "nginx:1.27", nil); err != nil {
		t.Fatal(err)
	}
	if img, _ := s.e.Client().InspectImage(t.Context(), "nginx:1.27"); img.ID == ran {
		t.Fatal("the tag didn't move")
	}
	s.e.Remove("web")
	s.pass(t)
	c := s.e.Container("web")
	if c == nil || c.Image != ran {
		t.Fatalf("made again from %v, want %s", c, ran)
	}
	if img, _ := s.e.Client().InspectImage(t.Context(), "nginx:1.27"); img.ID != ran {
		t.Errorf("the tag names %s, want it moved back to %s", img.ID, ran)
	}
	if got := s.said(webRef); got.What != desired.Made || !strings.Contains(got.Reason, "moved it back") {
		t.Errorf("the journal says %+v; want that the tag was moved back, for Activity", got)
	}
}

// An image that's gone is pulled by its digest, never by its tag; with no digest,
// the container waits.
func TestAnImageThatsGoneIsPulledByDigestOnly(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	rec, _, _ := s.store.Read(webRef)
	if rec.Image.Digest == "" {
		t.Fatal("the record has no digest")
	}
	s.e.Remove("web")
	if err := s.e.Client().RemoveImage(t.Context(), rec.Image.ID); err != nil {
		t.Fatal(err)
	}
	// The registry's tag names something else by now.
	s.e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"other"}}}
	s.pass(t)
	c := s.e.Container("web")
	if c == nil {
		t.Fatal("not made again")
	}
	// The fake engine names each pull anew, where Docker names an image by what's
	// in it, so it's the digest that says which image this is.
	if img, err := s.e.Client().InspectImage(t.Context(), c.Image); err != nil || !slices.Contains(img.RepoDigests, rec.Image.Digest) {
		t.Fatalf("made again from %v, %v, want %s, pulled by its digest", img, err, rec.Image.Digest)
	}

	bare := newStand(t)
	bare.run(t, webRef, nginx, nil)
	rec, _, _ = bare.store.Read(webRef)
	rec.Image = resource.Image{ID: rec.Image.ID}
	if err := bare.store.Keep(rec); err != nil {
		t.Fatal(err)
	}
	bare.e.Remove("web")
	if err := bare.e.Client().RemoveImage(t.Context(), rec.Image.ID); err != nil {
		t.Fatal(err)
	}
	bare.pass(t)
	if bare.e.Container("web") != nil {
		t.Fatal("made again from an image pulled by its tag")
	}
	if got := bare.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "no digest to pull it by") {
		t.Errorf("the journal says %+v", got)
	}
}

// A container Docker couldn't start is started, as Docker leaves it with an error;
// one someone stopped isn't.
func TestWhatDockerCouldntStartIsStarted(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.e.FailedToStart("web", "error while creating mount source path '/mnt/late': no such file or directory")
	s.pass(t)
	if c := s.e.Container("web"); c == nil || !c.State.Running {
		t.Fatalf("after a pass the container is %+v", c)
	}
	if got := s.said(webRef); got.What != desired.Started {
		t.Errorf("the journal says %+v", got)
	}
	if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
		t.Fatal(err)
	}
	s.r.tries = map[resource.Ref]*tries{} // no wait between starts holds it back
	s.pass(t)
	if c := s.e.Container("web"); c.State.Running {
		t.Error("started a container someone stopped")
	}
}

// failingStart is a container that won't start.
type failingStart struct {
	resource.Reviver
	starts int
}

func (f *failingStart) Start(context.Context, resource.Ref) error {
	f.starts++
	return errors.New("port is already allocated")
}

// A start that keeps failing is tried 10 times a boot, waiting longer each time, and
// then the journal says so.
func TestAStartIsTriedTenTimes(t *testing.T) {
	s := newStand(t)
	now := time.Now()
	s.r.now = func() time.Time { return now }
	f := &failingStart{}
	for range 30 {
		s.r.start(t.Context(), f, webRef, "it can't bind its port", "container-1")
		now = now.Add(lastRetry)
	}
	if f.starts != startTries {
		t.Errorf("started %d times, want %d", f.starts, startTries)
	}
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "in 10 tries") {
		t.Errorf("the journal says %+v", got)
	}
	// Within the wait, it doesn't try again.
	g := &failingStart{}
	s.r.tries = map[resource.Ref]*tries{}
	s.r.start(t.Context(), g, webRef, "", "container-1")
	s.r.start(t.Context(), g, webRef, "", "container-1")
	if g.starts != 1 {
		t.Errorf("started %d times within the wait", g.starts)
	}
}

// A network that's gone is made again only when no container still names it.
func TestANetworkIsMadeAgainOnlyWhenNothingNamesIt(t *testing.T) {
	s := newStand(t)
	net := resource.Ref{Kind: network.Kind, Scope: "srv_1", Name: "back"}
	s.run(t, net, &resourcev1.Network{}, nil)
	name := network.TargetOf(net).Name
	if err := s.e.Client().RemoveNetwork(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	s.pass(t)
	if _, err := s.e.Client().NetworkNamed(t.Context(), name); err != nil {
		t.Fatalf("not made again: %v", err)
	}

	s.run(t, webRef, &resourcev1.Container{Image: "nginx:1.27", Networks: []string{name}}, nil)
	if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.e.Client().DisconnectNetwork(t.Context(), name, "web"); err != nil {
		t.Skip("the fake engine can't remove a network a container names", err)
	}
	if err := s.e.Client().ConnectNetwork(t.Context(), name, "web", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.e.Client().RemoveNetwork(t.Context(), name); err != nil {
		t.Skip("the fake engine can't remove a network a container names", err)
	}
	s.pass(t)
	if got := s.said(net); got.What != desired.Waiting || !strings.Contains(got.Reason, "still name it") {
		t.Errorf("the journal says %+v", got)
	}
}

// While the agent puts a resource back, a plan or an apply of it is turned away,
// and a step under way keeps the agent off it.
func TestTheAgentsOwnWorkHoldsTheResource(t *testing.T) {
	u := newUnfinished()
	mark, ok := u.claim(webRef)
	if !ok {
		t.Fatal("couldn't mark it")
	}
	plan := &agentv1.Call{Id: 1, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Plan{Plan: &agentv1.PlanCall{
		Desired: []*agentv1.DesiredResource{{Resource: resourcepb.Ref(webRef)}},
	}}}
	if _, refused := u.begin(t.Context(), plan); refused == nil || !strings.Contains(refused.GetError().GetMessage(), "putting web back on its own") {
		t.Fatalf("a plan while the agent puts it back: %v", refused)
	}
	u.end(mark)
	if _, refused := u.begin(t.Context(), plan); refused != nil {
		t.Fatalf("a plan once it's done: %v", refused)
	}
	apply := &agentv1.Call{Id: 2, Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(webRef)}}}}
	step, _ := u.begin(t.Context(), apply)
	if _, ok := u.claim(webRef); ok {
		t.Error("marked it with a step under way")
	}
	u.end(step)
}

// noted is what the health monitor's scan tells the agent: the server's
// containers, listed now.
func (s *stand) noted(t *testing.T) {
	t.Helper()
	list, err := s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}})
	if err != nil {
		t.Fatal(err)
	}
	s.r.noted(t.Context(), time.Now(), list)
}

// What someone stops while the control plane is there, and is then pruned, isn't
// made again once it's away, as that would start it: the monitor's scan notes it
// while connected, and a step after doesn't undo that unless it started the
// container (#357's first review, H1).
func TestWhatsStoppedWhileConnectedIsntMadeAgain(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.presence.set(true, time.Now())
	if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
		t.Fatal(err)
	}
	s.noted(t)
	// A step that changes it in place keeps what was seen.
	rec, _, _ := s.store.Read(webRef)
	rec.Revision = 2
	if err := s.store.Keep(rec); err != nil {
		t.Fatal(err)
	}
	s.e.Remove("web") // docker system prune, still connected
	s.presence.set(false, time.Now().Add(-2*time.Minute))
	s.r.pass(t.Context(), false)
	s.pass(t)
	if s.e.Container("web") != nil {
		t.Fatal("made again, and started, what someone stopped while the control plane was there")
	}
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "It was stopped") {
		t.Errorf("the journal says %+v", got)
	}
}

// A look the monitor took before a step ended is old news, and a resource with a
// step under way is the step's to say (#357's first review, H1).
func TestTheMonitorsLookDoesntOverrideAStep(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	if err := s.e.Client().StopContainer(t.Context(), "web", time.Second); err != nil {
		t.Fatal(err)
	}
	list, err := s.e.Client().ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject}})
	if err != nil {
		t.Fatal(err)
	}
	read := time.Now()
	rec, _, _ := s.store.Read(webRef)
	rec.Revision, rec.Started = 2, true
	if err := s.store.Keep(rec); err != nil {
		t.Fatal(err)
	}
	s.r.noted(t.Context(), read, list)
	if got, _, _ := s.store.Read(webRef); got.Stopped {
		t.Error("a list read before the step that started it marked it stopped")
	}
	mark, ok := s.held.claim(webRef)
	if !ok {
		t.Fatal("can't hold it")
	}
	s.noted(t)
	s.held.end(mark)
	if got, _, _ := s.store.Read(webRef); got.Stopped {
		t.Error("marked stopped while a step on it was under way")
	}
	s.noted(t)
	if got, _, _ := s.store.Read(webRef); !got.Stopped {
		t.Error("the monitor's look after the step didn't mark it stopped")
	}
}

// What Docker couldn't start isn't taken for stopped by someone (#357's first
// review, H1).
func TestWhatDockerCouldntStartIsntTakenForStopped(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.e.FailedToStart("web", "it can't bind its port")
	s.noted(t)
	if got, _, _ := s.store.Read(webRef); got.Stopped {
		t.Error("marked stopped what Docker couldn't start")
	}
}

// A container whose image keeps its data in a volume it declares is made again on
// that volume, by name, as a replace would; with that volume gone, or one the agent
// never saw, it waits (#357's first review, H2).
func TestAnImagesVolumesAreMountedAgain(t *testing.T) {
	s := newStand(t)
	const data = "/var/lib/postgresql/data"
	s.e.Registry["postgres:17"] = dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Volumes: []string{data}}
	s.run(t, webRef, &resourcev1.Container{Image: "postgres:17"}, nil)
	vol := volumeAt(s.e.Container("web"), data)
	if vol == "" {
		t.Fatal("the image's volume isn't mounted")
	}
	s.noted(t)
	s.e.Remove("web") // docker rm -f web
	s.pass(t)
	c := s.e.Container("web")
	if c == nil {
		t.Fatalf("not made again: %+v", s.said(webRef))
	}
	if got := volumeAt(c, data); got != vol {
		t.Errorf("made again on %s at %s, want the volume it had, %s", got, data, vol)
	}

	// The volume gone too: it waits.
	s.noted(t)
	s.e.Remove("web")
	if err := s.e.Client().RemoveVolume(t.Context(), vol); err != nil {
		t.Fatal(err)
	}
	s.pass(t)
	if s.e.Container("web") != nil {
		t.Fatal("made again with its data's volume gone")
	}
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "where its image keeps its data, is gone") {
		t.Errorf("the journal says %+v", got)
	}
}

// One whose image declares a volume the agent never saw mounted waits, as making it
// again would start it empty (#357's first review, H2).
func TestAnImageVolumeNeverSeenWaits(t *testing.T) {
	s := newStand(t)
	s.e.Registry["postgres:17"] = dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}, Volumes: []string{"/var/lib/postgresql/data"}}
	s.run(t, webRef, &resourcev1.Container{Image: "postgres:17"}, nil)
	s.e.Remove("web") // before any look
	s.pass(t)
	if s.e.Container("web") != nil {
		t.Fatal("made again with an empty volume where its image keeps data")
	}
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "never saw the volume") {
		t.Errorf("the journal says %+v", got)
	}
}

func volumeAt(c *docker.Container, path string) string {
	if c == nil {
		return ""
	}
	for _, m := range c.Mounts {
		if m.Destination == path {
			return m.Name
		}
	}
	return ""
}

// A container replaced by hand under another name, on the same volume, isn't
// given a twin: two on one volume can damage its data (#357's first review, M1).
func TestOneReplacedByHandIsntGivenATwin(t *testing.T) {
	s := newStand(t)
	data := resource.Ref{Kind: volume.Kind, Scope: "srv_1", Name: "data"}
	s.run(t, data, &resourcev1.Volume{}, nil)
	mount := []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/var/lib/postgresql/data"}}
	spec := &resourcev1.Container{Image: "nginx:1.27", Mounts: mount}
	s.run(t, webRef, spec, nil)
	hand := resource.Ref{Kind: container.Kind, Scope: "srv_1", Name: "web-by-hand"}
	steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: hand, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.local.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatal(err)
	}
	s.e.Remove("web")
	s.pass(t)
	if s.e.Container("web") != nil {
		t.Fatal("made a twin of what replaced it by hand")
	}
	if got := s.said(webRef); got.What != desired.Waiting || !strings.Contains(got.Reason, "web-by-hand") {
		t.Errorf("the journal says %+v", got)
	}
}

// A record with no image ID, as a fill for what's gone writes, takes the tag only
// at the digest it records; a tag moved since is pulled by the digest (#357's first
// review, M2).
func TestATagIsTakenOnlyAtItsDigest(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	rec, _, _ := s.store.Read(webRef)
	if rec.Image.Digest == "" {
		t.Fatal("the record has no digest")
	}
	rec.Image = resource.Image{Digest: rec.Image.Digest}
	if err := s.store.Keep(rec); err != nil {
		t.Fatal(err)
	}
	s.e.Remove("web")
	s.e.Registry["nginx:1.27"] = dockertest.Image{Digest: "sha256:" + strings.Repeat("7e", 32), Config: docker.Config{Cmd: []string{"nginx"}, Env: []string{"NEWER=1"}}}
	if err := s.e.Client().PullImage(t.Context(), "nginx:1.27", nil); err != nil {
		t.Fatal(err)
	}
	moved, _ := s.e.Client().InspectImage(t.Context(), "nginx:1.27")
	s.pass(t)
	c := s.e.Container("web")
	if c == nil {
		t.Fatalf("not made: %+v", s.said(webRef))
	}
	if c.Image == moved.ID || slices.Contains(c.Config.Env, "NEWER=1") {
		t.Errorf("made from the image the tag moved to, %s", c.Image)
	}
}

// A Docker that hangs holds a resource no longer than the agent's own deadline, and
// the control plane, turned away meanwhile, is told what to do if it doesn't clear
// (#357's first review, M3).
func TestAHungDockerHoldsAResourceForALimitedTime(t *testing.T) {
	was := makeTimeout
	makeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { makeTimeout = was })
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.e.Remove("web")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s.e.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create") {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		return nil
	})
	done := make(chan struct{})
	go func() { s.pass(t); close(done) }()
	time.Sleep(100 * time.Millisecond)
	plan := &agentv1.Call{Id: 1, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Plan{Plan: &agentv1.PlanCall{
		Desired: []*agentv1.DesiredResource{{Resource: resourcepb.Ref(webRef)}},
	}}}
	if _, refused := s.held.begin(t.Context(), plan); !strings.Contains(refused.GetError().GetMessage(), "restart the agent") {
		t.Errorf("the control plane is turned away with %q; want what to do if it doesn't clear", refused.GetError().GetMessage())
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a hung create held the resource past the agent's deadline")
	}
	if s.held.busy(webRef) {
		t.Error("the resource is still held")
	}
}

// A pull by digest stops as a stream opens: the control plane has the say then, and
// stopping it destroys nothing (#357's first review, M3).
func TestAPullStopsAsAStreamOpens(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	rec, _, _ := s.store.Read(webRef)
	s.e.Remove("web")
	if err := s.e.Client().RemoveImage(t.Context(), rec.Image.ID); err != nil {
		t.Fatal(err)
	}
	s.e.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/images/create") {
			<-r.Context().Done()
		}
		return nil
	})
	done := make(chan struct{})
	go func() { s.pass(t); close(done) }()
	time.Sleep(100 * time.Millisecond)
	s.presence.set(true, time.Now())
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a pull went on after a stream opened")
	}
}

// With Docker not answering while away, the journal says so once, for the server,
// not once for each resource (#357's first review, a low).
func TestDockerDownIsOneWait(t *testing.T) {
	s := newStand(t)
	s.run(t, webRef, nginx, nil)
	s.run(t, resource.Ref{Kind: container.Kind, Scope: "srv_1", Name: "api"}, nginx, nil)
	s.e.Intercept(func(*http.Request) error { return errors.New("Docker is down") })
	s.pass(t)
	s.pass(t)
	entries, _, _ := s.journal.Pending()
	if len(entries) != 1 || entries[0].Ref != (resource.Ref{}) || !strings.Contains(entries[0].Reason, "Docker doesn't answer") {
		t.Errorf("the journal says %+v; want one wait for the server", entries)
	}
}
