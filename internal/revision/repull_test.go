// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

const (
	shopTag   = "ghcr.io/acme/shop:latest"
	shopOld   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shopNewer = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func shopSpec() *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: shopTag}}}
}

func moveShop(e *dockertest.Engine, digest string) {
	e.Registry[shopTag] = dockertest.Image{Digest: digest, Config: docker.Config{Cmd: []string{"shop"}}}
}

// shop is a service on a moving tag, running the older image on its servers.
type shop struct {
	e      *Engine
	db     *store.DB
	ref    resource.Ref
	name   string
	on     map[string]*dockertest.Engine
	before map[string]string // each server's container, by ID, before the pull
}

// running is what runs of the service on a server: its newest, as a rollout moves a
// service up its numbers.
func (s *shop) running(on *dockertest.Engine) *docker.Container {
	return on.Newest(docker.Project(s.ref.Scope), s.ref.Name)
}

func runShop(t *testing.T, b store.Backend, servers ...string) *shop {
	t.Helper()
	e, on, db := setupDocker(t, b, servers...)
	env := production(t, db)
	for _, eng := range on {
		moveShop(eng, shopOld)
	}
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: "web"}
	apply(t, e, propose(t, e, "Make", Edit{Ref: ref, Spec: shopSpec(), Servers: servers}))
	s := &shop{e: e, db: db, ref: ref, name: docker.ContainerName(docker.Project(env), "web"), on: on, before: map[string]string{}}
	for id, eng := range on {
		s.before[id] = s.running(eng).ID
		moveShop(eng, shopNewer)
	}
	return s
}

func (s *shop) pull(t *testing.T) *Plan {
	t.Helper()
	return propose(t, s.e, "Deploy", Edit{Ref: s.ref, Spec: shopSpec(), Base: 1, Pull: true, Run: true})
}

// again is the control plane starting again, on the same database and servers.
func (s *shop) again() *Engine {
	return New(s.db, controlKinds(), s.e.agents, s.e.sealer, slog.New(slog.DiscardHandler))
}

// holdUntilStopped holds the first request match matches, as a control plane that
// stops is in the middle of it, and reports reaching it.
func holdUntilStopped(eng *dockertest.Engine, match func(*http.Request) bool) <-chan struct{} {
	reached := make(chan struct{}, 1)
	var once atomic.Bool
	eng.Intercept(func(r *http.Request) error {
		if match(r) && once.CompareAndSwap(false, true) {
			reached <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return errors.New("stopped")
		}
		return nil
	})
	return reached
}

// stopNow stops a control plane that's waiting on reached.
func stopNow(t *testing.T, e *Engine, eng *dockertest.Engine, reached <-chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the deploy never reached the request")
	}
	grace, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if err := e.Stop(grace); err != nil {
		t.Fatal(err)
	}
	eng.Intercept(nil)
}

// stopAt starts the change, and stops the control plane at the request match
// matches.
func (s *shop) stopAt(t *testing.T, change string, eng *dockertest.Engine, match func(*http.Request) bool) {
	t.Helper()
	reached := holdUntilStopped(eng, match)
	if err := s.e.Start(t.Context(), change, By{Actor: "person", Via: ViaPanel}); err != nil {
		t.Fatal(err)
	}
	stopNow(t, s.e, eng, reached)
	if c := stored(t, s.db, change); c.State != store.ChangeApplying {
		t.Fatalf("after the stop the change is %s", c.State)
	}
}

func (s *shop) resume(t *testing.T) {
	t.Helper()
	next := s.again()
	if err := next.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	next.Wait()
}

func (s *shop) record(t *testing.T) (latest store.Resource, deployments []store.Deployment) {
	t.Helper()
	if err := s.db.View(t.Context(), func(tx *store.Tx) (err error) {
		if latest, err = tx.GetResource(t.Context(), string(s.ref.Kind), s.ref.Scope, s.ref.Name); err != nil {
			return err
		}
		deployments, err = tx.Deployments(t.Context(), latest.ID, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return latest, deployments
}

func (s *shop) id(t *testing.T) string {
	t.Helper()
	latest, _ := s.record(t)
	return latest.ID
}

// runs is the digest of what a server's container runs, and whether it's running.
func (s *shop) runs(t *testing.T, server string) (digest string, running bool) {
	t.Helper()
	eng := s.on[server]
	c := s.running(eng)
	if c == nil {
		t.Fatalf("%s has no container %s", server, s.name)
	}
	img, err := eng.Client().InspectImage(t.Context(), c.Image)
	if err != nil {
		t.Fatal(err)
	}
	return img.Digest(shopTag), c.State.Running
}

// wantNewerRunning checks the change ended applied, with the newer image running
// on every server, and one revision that says so.
func (s *shop) wantNewerRunning(t *testing.T, change string) {
	t.Helper()
	c := stored(t, s.db, change)
	if c.State != store.ChangeApplied {
		t.Fatalf("the change ended %s: %s", c.State, c.Error)
	}
	for _, words := range []string{"already has it", "is already the newest image"} {
		if strings.Contains(string(c.Progress), words) {
			t.Errorf("a step says %q, of what replaced what ran: %s", words, c.Progress)
		}
	}
	for server := range s.on {
		if digest, running := s.runs(t, server); digest != shopNewer || !running {
			t.Errorf("%s runs %s, running %t", server, digest, running)
		}
	}
	latest, deployments := s.record(t)
	if latest.Revision != 2 {
		t.Errorf("the latest revision is r%d, want r2: one for what was found", latest.Revision)
	}
	var progress Progress
	if err := json.Unmarshal(c.Progress, &progress); err != nil {
		t.Fatal(err)
	}
	for i := range progress.Steps {
		if st := &progress.Steps[i]; st.Did == nil || *st.Did != resource.ActionReplace {
			t.Errorf("the step on %s did %v, and it was replaced", st.Server, st.Did)
		}
	}
	seen := 0
	for i := range deployments {
		d := &deployments[i]
		switch d.Revision {
		case 1:
			if d.Digest != shopOld || d.State != store.DeploymentSuperseded {
				t.Errorf("r1 on %s reads %s with %s", d.Server, d.State, d.Digest)
			}
		case 2:
			seen++
			if d.Digest != shopNewer || d.State != store.DeploymentLive {
				t.Errorf("r2 on %s reads %s with %s", d.Server, d.State, d.Digest)
			}
		}
	}
	if seen != len(s.on) {
		t.Errorf("r2 has %d deployments: %+v", seen, deployments)
	}
}

// The requests a pull's step makes, in order, to stop at.
func atThePull(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/create")
}

func atTheCreate(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create")
}

func atStoppingTheOld(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop")
}

// diedBeforeItStarted leaves what a control plane that died at that moment leaves:
// a stop can't, as a swap under way finishes whatever stops waiting for it.
func diedBeforeItStarted(t *testing.T, s *shop, change string) {
	t.Helper()
	s.stopAfterTheStep(t, change, "srv_A")
	s.on["srv_A"].Edit(strings.TrimPrefix(s.running(s.on["srv_A"]).Name, "/"), func(c *docker.Container) { c.State = docker.State{Status: "created"} })
}

// afterThePull is the read of the tag that follows the pull. A plan reads it too,
// before anything has pulled.
func afterThePull(eng *dockertest.Engine, pulls int) func(*http.Request) bool {
	return func(r *http.Request) bool {
		return r.Method == http.MethodGet && strings.Contains(r.URL.Path, "shop:latest/json") && eng.Pulls > pulls
	}
}

// stopsAfter is an agent that does the step, then loses its control plane before
// the record hears of it.
type stopsAfter struct {
	resource.Agent
	applied chan struct{}
}

func (a stopsAfter) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	if err := a.Agent.Apply(ctx, step, desired); err != nil {
		return err
	}
	a.applied <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

// stopAfterTheStep starts the change, and stops the control plane once its step
// has run and before that's on record.
func (s *shop) stopAfterTheStep(t *testing.T, change, server string) {
	t.Helper()
	plain := s.e.agents.(onDocker)
	applied := make(chan struct{}, 1)
	held := onDocker{}
	for id, a := range plain {
		held[id] = a
	}
	held[server] = stopsAfter{Agent: plain[server], applied: applied}
	s.e.agents = held
	if err := s.e.Start(t.Context(), change, By{Actor: "person", Via: ViaPanel}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-applied:
	case <-time.After(10 * time.Second):
		t.Fatal("the step never ran")
	}
	grace, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if err := s.e.Stop(grace); err != nil {
		t.Fatal(err)
	}
	s.e.agents = plain
}

// Wherever a pull stops, starting again ends with the newer image running, on one
// revision, and nothing reading as done before it is.
func TestAPullStoppedPartwayFinishesOnce(t *testing.T) {
	stops := []struct {
		name string
		stop func(t *testing.T, s *shop, change string)
	}{
		{"during the pull", func(t *testing.T, s *shop, change string) {
			s.stopAt(t, change, s.on["srv_A"], atThePull)
		}},
		{"after the pull, before anything is on record", func(t *testing.T, s *shop, change string) {
			eng := s.on["srv_A"]
			s.stopAt(t, change, eng, afterThePull(eng, eng.Pulls))
		}},
		{"after its revision, before the container is replaced", func(t *testing.T, s *shop, change string) {
			s.stopAt(t, change, s.on["srv_A"], atTheCreate)
			if latest, _ := s.record(t); latest.Revision != 2 {
				t.Fatalf("the stop came before the revision: r%d", latest.Revision)
			}
		}},
		{"before the old one stops", func(t *testing.T, s *shop, change string) {
			s.stopAt(t, change, s.on["srv_A"], atStoppingTheOld)
		}},
		{"after the swap, before the new one starts", diedBeforeItStarted},
		{"after the container is replaced, before that's on record", func(t *testing.T, s *shop, change string) {
			s.stopAfterTheStep(t, change, "srv_A")
		}},
	}
	for _, tc := range stops {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Each(t, func(t *testing.T, b store.Backend) {
				s := runShop(t, b, "srv_A")
				plan := s.pull(t)
				tc.stop(t, s, plan.Change)
				s.resume(t)
				s.wantNewerRunning(t, plan.Change)
				if got := s.running(s.on["srv_A"]).ID; got == s.before["srv_A"] {
					t.Error("the old container still runs")
				}
			})
		})
	}
}

// Stopped again as it resumes, it still ends with one revision.
func TestAPullStoppedTwiceFinishesOnce(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A")
	plan := s.pull(t)
	eng := s.on["srv_A"]
	s.stopAt(t, plan.Change, eng, atTheCreate)

	second := s.again()
	reached := holdUntilStopped(eng, atStoppingTheOld)
	resumed := make(chan error, 1)
	go func() { resumed <- second.Resume(context.WithoutCancel(t.Context())) }()
	stopNow(t, second, eng, reached)
	if err := <-resumed; err != nil && !errors.Is(err, ErrInterrupted) {
		t.Fatal(err)
	}
	if c := stored(t, s.db, plan.Change); c.State != store.ChangeApplying {
		t.Fatalf("after the second stop the change is %s", c.State)
	}

	s.resume(t)
	s.wantNewerRunning(t, plan.Change)
}

// What's on record is what the server has, so going on needs no registry, and a
// pull that had finished isn't asked again, which could find something newer still.
func TestAPullThatHadFoundItsImageResumesWithoutItsRegistry(t *testing.T) {
	stops := []struct {
		name string
		stop func(t *testing.T, s *shop, change string)
	}{
		{"before the container is replaced", func(t *testing.T, s *shop, change string) {
			s.stopAt(t, change, s.on["srv_A"], atTheCreate)
		}},
		{"after the swap", diedBeforeItStarted},
		{"after the container is replaced", func(t *testing.T, s *shop, change string) {
			s.stopAfterTheStep(t, change, "srv_A")
		}},
	}
	for _, tc := range stops {
		t.Run(tc.name, func(t *testing.T) {
			s := runShop(t, store.SQLite, "srv_A")
			plan := s.pull(t)
			tc.stop(t, s, plan.Change)
			eng := s.on["srv_A"]
			delete(eng.Registry, shopTag)
			pulls := eng.Pulls
			s.resume(t)
			s.wantNewerRunning(t, plan.Change)
			if eng.Pulls != pulls {
				t.Errorf("resuming pulled again, %d times", eng.Pulls-pulls)
			}
		})
	}
}

// Stopped before anything is on record, there's no saying what it found, so it
// asks again. With no registry that fails as a pull does: nothing is replaced, and
// the record stays true.
func TestAPullWithNothingOnRecordResumesByAskingAgain(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A")
	plan := s.pull(t)
	eng := s.on["srv_A"]
	s.stopAt(t, plan.Change, eng, afterThePull(eng, eng.Pulls))
	delete(eng.Registry, shopTag)
	next := s.again()
	if err := next.Resume(t.Context()); err == nil {
		t.Error("resuming a pull that can't pull didn't fail")
	}
	next.Wait()

	c := stored(t, s.db, plan.Change)
	if c.State != store.ChangeFailed || !strings.Contains(c.Error, "can't pull "+shopTag+" again") {
		t.Fatalf("the change ended %s: %s", c.State, c.Error)
	}
	if got := s.running(eng); got.ID != s.before["srv_A"] || !got.State.Running {
		t.Error("a pull that failed touched what runs")
	}
	latest, deployments := s.record(t)
	if latest.Revision != 1 || len(deployments) != 1 || deployments[0].State != store.DeploymentLive {
		t.Errorf("r%d, %+v", latest.Revision, deployments)
	}
}

// With nothing newer, stopping changes nothing: the step still says so.
func TestAPullStoppedWithNothingNewerStaysAsItIs(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A")
	eng := s.on["srv_A"]
	moveShop(eng, shopOld)
	plan := s.pull(t)
	s.stopAt(t, plan.Change, eng, atThePull)
	s.resume(t)

	c := stored(t, s.db, plan.Change)
	if c.State != store.ChangeApplied || !strings.Contains(string(c.Progress), "is already the newest image") {
		t.Fatalf("the change ended %s: %s", c.State, c.Progress)
	}
	var progress Progress
	if err := json.Unmarshal(c.Progress, &progress); err != nil {
		t.Fatal(err)
	}
	if st := &progress.Steps[0]; st.Did == nil || *st.Did != resource.ActionNone {
		t.Errorf("the step did %v, and it stayed", st.Did)
	}
	if got := s.running(eng); got.ID != s.before["srv_A"] {
		t.Error("with nothing newer, the container was replaced")
	}
	if latest, _ := s.record(t); latest.Revision != 1 {
		t.Errorf("with nothing newer, the latest revision is r%d", latest.Revision)
	}
}

// Between two servers, the one that's done stays done, and the other is brought
// to the same revision.
func TestAPullStoppedBetweenServersFinishesOnce(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A", "srv_B")
	plan := s.pull(t)
	s.stopAt(t, plan.Change, s.on["srv_B"], atTheCreate)
	onA := s.running(s.on["srv_A"]).ID
	if onA == s.before["srv_A"] {
		t.Fatal("the stop came before srv_A was replaced")
	}
	pulls := s.on["srv_A"].Pulls

	s.resume(t)
	s.wantNewerRunning(t, plan.Change)
	if got := s.running(s.on["srv_A"]).ID; got != onA {
		t.Error("srv_A was replaced twice")
	}
	if s.on["srv_A"].Pulls != pulls {
		t.Error("srv_A pulled again, when its pull was on record")
	}
}

// A server that hadn't got to its pull still asks, even with its record's revision
// made, as what it runs is older than that says.
func TestAPullStoppedBeforeAServersPullStillAsks(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A", "srv_B")
	plan := s.pull(t)
	s.stopAt(t, plan.Change, s.on["srv_B"], atThePull)
	if latest, _ := s.record(t); latest.Revision != 2 {
		t.Fatalf("srv_A's pull didn't make its revision: r%d", latest.Revision)
	}
	pulls := s.on["srv_B"].Pulls
	s.resume(t)
	s.wantNewerRunning(t, plan.Change)
	if s.on["srv_B"].Pulls == pulls {
		t.Error("srv_B never pulled")
	}
}

// Each server says what it did, as one can find nothing newer while another does.
func TestAPullSaysWhatItDidOnEachServer(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A", "srv_B")
	moveShop(s.on["srv_A"], shopOld) // it has this already
	plan := s.pull(t)
	apply(t, s.e, plan)
	var progress Progress
	if err := json.Unmarshal(stored(t, s.db, plan.Change).Progress, &progress); err != nil {
		t.Fatal(err)
	}
	did := map[string]resource.Action{}
	for i := range progress.Steps {
		if st := &progress.Steps[i]; st.Did != nil {
			did[st.Server] = *st.Did
		}
	}
	if len(did) != 2 || did["srv_A"] != resource.ActionNone || did["srv_B"] != resource.ActionReplace {
		t.Errorf("the steps did %v", did)
	}
}

// What a step did goes on record as it ends, so a control plane that dies before
// the next one has lost nothing.
func TestWhatAStepDidIsOnRecordAsItEnds(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A", "srv_B")
	plan := s.pull(t)
	eng := s.on["srv_B"]
	reached := holdUntilStopped(eng, atTheCreate)
	if err := s.e.Start(t.Context(), plan.Change, By{Actor: "person", Via: ViaPanel}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the deploy never reached srv_B")
	}
	var progress Progress
	if err := json.Unmarshal(stored(t, s.db, plan.Change).Progress, &progress); err != nil {
		t.Fatal(err)
	}
	if i := progress.index("srv_A", s.id(t)); i < 0 || progress.Steps[i].Did == nil || *progress.Steps[i].Did != resource.ActionReplace {
		t.Errorf("with srv_A done, the record says %+v", progress.Steps)
	}
	done := make(chan struct{}, 1)
	done <- struct{}{}
	stopNow(t, s.e, eng, done)
}

// A resume finishes what its change asked, and that's all: what someone stopped, and
// the change didn't ask to run, stays stopped. Asking is per service, and on record.
func TestAResumeLeavesWhatSomeoneStoppedStopped(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, on, db := setupDocker(t, b, "srv_A")
		eng := on["srv_A"]
		env := production(t, db)
		moveShop(eng, shopOld)
		eng.Registry["ghcr.io/acme/shop:new"] = dockertest.Image{Digest: shopNewer, Config: docker.Config{Cmd: []string{"shop"}}}
		ref := func(name string) resource.Ref { return resource.Ref{Kind: service.Kind, Scope: env, Name: name} }
		apply(t, e, propose(t, e, "Make", Edit{Ref: ref("web"), Spec: shopSpec()}, Edit{Ref: ref("api"), Spec: shopSpec()}))
		web, api := docker.ContainerName(docker.Project(env), "web"), docker.ContainerName(docker.Project(env), "api")
		eng.Stop(web)
		stopped, before := eng.Container(web).ID, eng.Container(api).ID

		newer := shopSpec()
		newer.GetApp().Image = "ghcr.io/acme/shop:new"
		s := &shop{e: e, db: db, on: on}
		plan := propose(t, e, "Deploy api", Edit{Ref: ref("web"), Spec: shopSpec()}, Edit{Ref: ref("api"), Spec: newer, Run: true})
		s.stopAt(t, plan.Change, eng, atTheCreate)
		s.resume(t)

		if c := stored(t, db, plan.Change); c.State != store.ChangeApplied {
			t.Fatalf("the change ended %s: %s", c.State, c.Error)
		}
		if got := eng.Newest(docker.Project(env), "api"); got.ID == before || !got.State.Running {
			t.Errorf("api is %+v", got)
		}
		if got := eng.Container(web); got.ID != stopped || got.State.Running {
			t.Errorf("the resume started what someone stopped: %+v", got)
		}
	})
}

// A resume still honours what its change asked: a service it asked to run, which was
// running when it was planned and so had no step, is started if someone stops it
// before the change ends.
func TestAResumeRunsWhatItsChangeAsked(t *testing.T) {
	e, on, db := setupDocker(t, store.SQLite, "srv_A")
	eng := on["srv_A"]
	env := production(t, db)
	moveShop(eng, shopOld)
	eng.Registry["ghcr.io/acme/shop:new"] = dockertest.Image{Digest: shopNewer, Config: docker.Config{Cmd: []string{"shop"}}}
	ref := func(name string) resource.Ref { return resource.Ref{Kind: service.Kind, Scope: env, Name: name} }
	apply(t, e, propose(t, e, "Make", Edit{Ref: ref("web"), Spec: shopSpec()}, Edit{Ref: ref("api"), Spec: shopSpec()}))
	web := docker.ContainerName(docker.Project(env), "web")

	newer := shopSpec()
	newer.GetApp().Image = "ghcr.io/acme/shop:new"
	s := &shop{e: e, db: db, on: on}
	plan := propose(t, e, "Run web, and deploy api", Edit{Ref: ref("web"), Spec: shopSpec(), Run: true}, Edit{Ref: ref("api"), Spec: newer})
	s.stopAt(t, plan.Change, eng, atTheCreate)
	eng.Stop(web)
	s.resume(t)

	if c := stored(t, db, plan.Change); c.State != store.ChangeApplied {
		t.Fatalf("the change ended %s: %s", c.State, c.Error)
	}
	if got := eng.Container(web); got == nil || !got.State.Running {
		t.Errorf("the change asked web to run, and after resuming it's %+v", got)
	}
}

// What a change's own step made and didn't get as far as starting isn't a stop by
// hand: the change resumes it, though nothing asked it to run but replacing it.
func TestAChangeThatDiedBeforeItsContainerStartedStartsIt(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A")
	eng := s.on["srv_A"]
	eng.Registry["ghcr.io/acme/shop:new"] = dockertest.Image{Digest: shopNewer, Config: docker.Config{Cmd: []string{"shop"}}}
	newer := shopSpec()
	newer.GetApp().Image = "ghcr.io/acme/shop:new"
	plan := propose(t, s.e, "Change the image", Edit{Ref: s.ref, Spec: newer, Base: 1})
	diedBeforeItStarted(t, s, plan.Change)
	if got := s.running(eng); got.State.Running {
		t.Fatal("the stop left it running")
	}
	s.resume(t)
	if c := stored(t, s.db, plan.Change); c.State != store.ChangeApplied {
		t.Fatalf("the change ended %s: %s", c.State, c.Error)
	}
	if got := s.running(eng); got == nil || !got.State.Running || got.Config.Image != "ghcr.io/acme/shop:new" {
		t.Errorf("after the resume, it's %+v", got)
	}
}

// Asked to run on two servers, each says what it does there: one starts, and the
// other replaces only if that's newer, so the plan doesn't say that of both.
func TestAPlanSaysWhichServerItStartsOn(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A", "srv_B")
	s.on["srv_A"].Stop(s.name)
	plan := s.pull(t)
	var said []string
	for _, n := range plan.Notes {
		if n.Reason == "starts_stopped" {
			said = append(said, n.Message)
		}
		if n.Reason == "pulls_again" {
			t.Errorf("the plan says %q", n.Message)
		}
	}
	if len(said) != 1 || said[0] != "web is stopped on srv_A, so this starts it" {
		t.Errorf("the plan says %v", said)
	}
}

// A later change that got there first leaves this one nothing to do.
func TestAPullALaterChangeMovedOnDoesNothingMore(t *testing.T) {
	s := runShop(t, store.SQLite, "srv_A")
	first := s.pull(t)
	eng := s.on["srv_A"]
	s.stopAt(t, first.Change, eng, atThePull)

	next := s.again()
	s.e = next
	apply(t, next, s.pull(t))
	onLater := s.running(eng).ID
	pulls := eng.Pulls
	moveShop(eng, "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	if err := next.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	next.Wait()

	if c := stored(t, s.db, first.Change); c.State != store.ChangeApplied {
		t.Errorf("the first change ended %s: %s", c.State, c.Error)
	}
	if got := s.running(eng).ID; got != onLater || eng.Pulls != pulls {
		t.Errorf("the first change went on: replaced %t, %d more pulls", got != onLater, eng.Pulls-pulls)
	}
	if latest, _ := s.record(t); latest.Revision != 2 {
		t.Errorf("the latest revision is r%d", latest.Revision)
	}
}

// Only a server can say what its tag names now, so one that isn't connected holds
// up a pull as it does a change to its spec: by name, and nothing goes on record.
func TestAPullIsRefusedForAServerThatIsntConnected(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := runShop(t, b, "srv_A", "srv_B")
		all := s.e.agents.(onDocker)
		s.e.agents = onDocker{"srv_A": all["srv_A"]}

		plan := s.pull(t)
		var offline *ServerPlan
		for i := range plan.Servers {
			if plan.Servers[i].Server == "srv_B" {
				offline = &plan.Servers[i]
			}
		}
		if offline == nil || !offline.Offline || len(offline.Steps) != 1 || !offline.Steps[0].Pull {
			t.Fatalf("the plan doesn't name srv_B: %+v", plan.Servers)
		}

		err := s.e.ApplyBy(t.Context(), plan.Change, By{Actor: "person", Via: ViaPanel})
		if !errors.Is(err, ErrOffline) || !strings.Contains(err.Error(), "srv_B isn't connected") || !strings.Contains(err.Error(), "once its agent reconnects") {
			t.Fatalf("applying = %v", err)
		}
		if c := stored(t, s.db, plan.Change); c.State != store.ChangePlanned {
			t.Errorf("the refused change is %s", c.State)
		}
		if got := s.running(s.on["srv_A"]).ID; got != s.before["srv_A"] {
			t.Error("srv_A was replaced by a change that couldn't apply")
		}
		if latest, _ := s.record(t); latest.Revision != 1 {
			t.Errorf("the latest revision is r%d, on a change that couldn't apply", latest.Revision)
		}

		// As it says: once it's back, its server can say what a plan made without it
		// couldn't, as how a service rolls can need a person, so that's reviewed again.
		s.e.agents = all
		if err := s.e.ApplyBy(t.Context(), plan.Change, By{Actor: "person", Via: ViaPanel}); !errors.Is(err, ErrPlanChanged) {
			t.Fatalf("applying what was planned without srv_B = %v", err)
		}
		again := s.pull(t)
		if err := s.e.ApplyBy(t.Context(), again.Change, By{Actor: "person", Via: ViaPanel}); err != nil {
			t.Fatal(err)
		}
		s.e.Wait()
		s.wantNewerRunning(t, again.Change)
	})
}

// What Docker's restart policy drifted to shouldn't stop a deploy of the same tag
// pulling, as its plan says, and it puts that right whatever the pull finds.
func TestAPullWithARestartPolicyThatDrifted(t *testing.T) {
	drifted := func(t *testing.T, s *shop) *dockertest.Engine {
		t.Helper()
		eng := s.on["srv_A"]
		if err := eng.Client().UpdateRestartPolicy(t.Context(), s.before["srv_A"], docker.RestartPolicy{Name: "always"}); err != nil {
			t.Fatal(err)
		}
		return eng
	}
	wantPlan := func(t *testing.T, plan *Plan) {
		t.Helper()
		steps := plan.Steps()
		if len(steps) != 1 || !steps[0].Pull || steps[0].Action != resource.ActionReplace || steps[0].Rollout == nil || !steps[0].Rollout.Rolling {
			t.Fatalf("the plan's steps are %+v", steps)
		}
		if len(steps[0].Changes) != 1 || !strings.HasSuffix(steps[0].Changes[0].Path, "restart") {
			t.Errorf("the plan's changes are %+v", steps[0].Changes)
		}
	}

	t.Run("and a newer image", func(t *testing.T) {
		s := runShop(t, store.SQLite, "srv_A")
		eng := drifted(t, s)
		plan := s.pull(t)
		wantPlan(t, plan)
		pulls := eng.Pulls
		apply(t, s.e, plan)
		if eng.Pulls == pulls {
			t.Error("the plan said it pulls, and it didn't")
		}
		s.wantNewerRunning(t, plan.Change)
		if got := s.running(eng).HostConfig.RestartPolicy.Name; got == "always" {
			t.Errorf("the restart policy is still %s", got)
		}
	})

	t.Run("and nothing newer", func(t *testing.T) {
		s := runShop(t, store.SQLite, "srv_A")
		eng := drifted(t, s)
		moveShop(eng, shopOld)
		plan := s.pull(t)
		wantPlan(t, plan)
		pulls := eng.Pulls
		apply(t, s.e, plan)
		if eng.Pulls == pulls {
			t.Error("the plan said it pulls, and it didn't")
		}
		got := s.running(eng)
		if got.ID != s.before["srv_A"] {
			t.Error("with nothing newer, a change Docker can make in place replaced the container")
		}
		if got.HostConfig.RestartPolicy.Name == "always" {
			t.Errorf("the restart policy is still %s", got.HostConfig.RestartPolicy.Name)
		}
		c := stored(t, s.db, plan.Change)
		if c.State != store.ChangeApplied || !strings.Contains(string(c.Progress), "so only web's restart policy or limits changed") {
			t.Errorf("the change ended %s: %s", c.State, c.Progress)
		}
		if latest, _ := s.record(t); latest.Revision != 1 {
			t.Errorf("the latest revision is r%d, with nothing about the service changed", latest.Revision)
		}
	})
}
