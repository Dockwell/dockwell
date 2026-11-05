// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// bench is a control plane, signed in, with one server on a fake Docker.
type bench struct {
	f  *fullServer
	b  *browser
	e  *dockertest.Engine
	c  dockwellv1connect.ChangeServiceClient
	cc dockwellv1connect.ComposeServiceClient
	s  dockwellv1connect.ServiceServiceClient
}

func newBench(t *testing.T, backend store.Backend) *bench {
	t.Helper()
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	return &bench{
		f: f, b: b, e: f.addDockerServer(t),
		c:  changeClient(b, b.origin),
		cc: dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()),
		s:  dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON()),
	}
}

// file plans a Compose file, and fails the test if it can't.
func (b *bench) file(t *testing.T, req *dockwellv1.ImportComposeRequest) *dockwellv1.Change {
	t.Helper()
	res, err := b.cc.ImportCompose(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("importing %.60q: %v", req.GetCompose(), err)
	}
	return res.Msg.GetChange()
}

// done applies a change, and fails the test unless it applies.
func (b *bench) done(t *testing.T, c *dockwellv1.Change) *dockwellv1.Change {
	t.Helper()
	got := applied(t, b.c, c)
	if got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("%q ended %v: %s", c.GetSummary(), got.GetState(), got.GetError())
	}
	return got
}

// run plans a Compose file and applies it, and says what environment it landed in.
func (b *bench) run(t *testing.T, req *dockwellv1.ImportComposeRequest) string {
	t.Helper()
	c := b.file(t, req)
	b.done(t, c)
	return serviceScope(c)
}

func serviceScope(c *dockwellv1.Change) string {
	for _, r := range c.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "service" {
			return r.GetResource().GetScope()
		}
	}
	return ""
}

// projectOf is the ID of the project a change brought to life.
func projectOf(c *dockwellv1.Change) string {
	for _, r := range c.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "environment" {
			return r.GetResource().GetScope()
		}
	}
	return ""
}

// addEnvironment makes another environment of a project.
func (b *bench) addEnvironment(t *testing.T, project, name string) {
	t.Helper()
	made, err := b.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Add " + name, Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: project, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{}}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	b.done(t, made.Msg.GetChange())
}

func (b *bench) service(t *testing.T, project, name string) *dockwellv1.ServiceInfo {
	t.Helper()
	res, err := b.s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{Project: project}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Msg.GetServices() {
		if s.GetResource().GetName() == name {
			return s
		}
	}
	t.Fatalf("no service %s in %s: %v", name, project, res.Msg.GetServices())
	return nil
}

func places(c *dockwellv1.Change) []string {
	var out []string
	for _, p := range c.GetPlan().GetPlaces() {
		out = append(out, p.GetProject()+"/"+p.GetEnvironment())
	}
	return out
}

const shopFile = "name: shop\nservices:\n  web:\n    image: ghcr.io/acme/shop:1.4\n"

// A file exported from production and imported into staging goes to staging. It
// used to change production, with a plan that read as staging's.
func TestAnExportGoesToTheEnvironmentAsked(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		first := b.file(t, &dockwellv1.ImportComposeRequest{Compose: shopFile})
		b.done(t, first)
		production, project := serviceScope(first), projectOf(first)
		b.addEnvironment(t, project, "staging")
		staging := b.run(t, &dockwellv1.ImportComposeRequest{Project: "shop", Environment: "staging", Compose: shopFile})
		if staging == production {
			t.Fatal("the two environments are one")
		}

		exported, err := b.cc.ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{Environment: production}))
		if err != nil {
			t.Fatal(err)
		}
		edited := strings.Replace(exported.Msg.GetCompose(), "shop:1.4", "shop:1.5", 1)

		asked := b.file(t, &dockwellv1.ImportComposeRequest{Environment: "staging", Compose: edited})
		if got := serviceScope(asked); got != staging {
			t.Errorf("asked for staging (%s), the plan lands in %s", staging, got)
		}
		if got := places(asked); !slices.Equal(got, []string{"shop/staging"}) {
			t.Errorf("the plan says it lands in %v", got)
		}
		b.done(t, asked)
		image := func(env string) string {
			return b.e.Newest(docker.Project(env), "web").Config.Image
		}
		if got := image(production); got != "ghcr.io/acme/shop:1.4" {
			t.Errorf("production now runs %s", got)
		}
		if got := image(staging); got != "ghcr.io/acme/shop:1.5" {
			t.Errorf("staging runs %s", got)
		}

		// Leaving it out still goes back where it came from, and says so.
		back := b.file(t, &dockwellv1.ImportComposeRequest{Compose: edited})
		if serviceScope(back) != production || !slices.Equal(places(back), []string{"shop/production"}) {
			t.Errorf("with no environment asked, it lands in %s, and says %v", serviceScope(back), places(back))
		}

		// One the project doesn't have is about the environment, whichever
		// project is asked for in the fleet.
		_, err = b.cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Environment: "nope", Compose: edited}))
		if reasonOf(err) != "not_found" || !strings.Contains(err.Error(), "nope") {
			t.Errorf("an environment that isn't there = %v", err)
		}
	})
}

// A plan names only what offers a choice.
func TestAPlanSaysWhereItLandsOnceThereIsAChoice(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		// One project with one environment: nothing to say.
		plain := b.file(t, &dockwellv1.ImportComposeRequest{Compose: "services:\n  a:\n    image: ghcr.io/acme/shop:1.4\n"})
		if got := places(plain); got != nil {
			t.Errorf("with no choice, the plan says %v", got)
		}
		b.done(t, plain)

		// The project a change makes counts as a choice, though it isn't there yet.
		shop := b.file(t, &dockwellv1.ImportComposeRequest{Compose: shopFile})
		if got := places(shop); !slices.Equal(got, []string{"shop/"}) {
			t.Errorf("a project with one environment says %v", got)
		}
		b.done(t, shop)

		b.addEnvironment(t, projectOf(shop), "staging")
		both := b.file(t, &dockwellv1.ImportComposeRequest{Project: "shop", Environment: "staging", Compose: shopFile})
		if got := places(both); !slices.Equal(got, []string{"shop/staging"}) {
			t.Errorf("a project with two environments says %v", got)
		}

		// GetChange reads it back, and it isn't what the plan is checked by.
		got, err := b.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: both.GetId()}))
		if err != nil || !slices.Equal(places(got.Msg.GetChange()), []string{"shop/staging"}) {
			t.Errorf("GetChange says %v, %v", got, err)
		}
		b.done(t, both)
	})
}

// Going back to a revision that matches the record, or deploying the image the
// record says, is how someone recovers when what runs doesn't match it.
func TestRecoveringIsNeverRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		tagged := func(tag string) *dockwellv1.ImportComposeRequest {
			return &dockwellv1.ImportComposeRequest{Compose: "name: shop\nservices:\n  web:\n    image: ghcr.io/acme/shop:" + tag + "\n"}
		}
		env := b.run(t, tagged("1.4"))
		b.run(t, tagged("1.5"))
		web := b.service(t, "shop", "web")
		rollback, err := b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if err != nil {
			t.Fatal(err)
		}
		last := b.done(t, rollback.Msg.GetChange())
		name := docker.ContainerName(docker.Project(env), "web")

		// While it runs what the record says, each would do nothing, and is refused.
		_, err = b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if reasonOf(err) != "cant_undo" || !strings.Contains(err.Error(), "already runs what revision 1 had") {
			t.Errorf("rolling back to what runs = %v", err)
		}
		// Someone removes the container by hand.
		if err := b.e.Client().RemoveContainer(t.Context(), b.e.Serving(name).ID, true); err != nil {
			t.Fatal(err)
		}
		again, err := b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
		if err != nil {
			t.Fatalf("with the container gone, rolling back to the revision that matches = %v", err)
		}
		if again.Msg.GetChange().GetReverts() != "" {
			t.Errorf("it reverts %s, which takes the undo from the change that got here", again.Msg.GetChange().GetReverts())
		}
		deploy, err := b.s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: web.GetId(), Image: "ghcr.io/acme/shop:1.4"}))
		if err != nil {
			t.Fatalf("with the container gone, deploying the same image = %v", err)
		}
		if _, err := b.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: again.Msg.GetChange().GetId()})); err != nil {
			t.Fatal(err)
		}
		b.done(t, deploy.Msg.GetChange())
		if ctr := b.e.Serving(name); ctr == nil || !ctr.State.Running || ctr.Config.Image != "ghcr.io/acme/shop:1.4" {
			t.Errorf("after deploying again, the container is %+v", ctr)
		}
		got, err := b.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: last.GetId()}))
		if err != nil || got.Msg.GetChange().GetUndoneBy() != "" {
			t.Errorf("the rollback that got here = %v, %v; want it not undone", got, err)
		}
	})
}

// A plan made from a spec that has moved on would quietly undo what moved it. It
// took a race to show: the plan summarised "Publish port 10003" dropped the port
// that had just been added.
func TestPortsAddedTogetherAreNeverLost(t *testing.T) {
	b := newBench(t, store.SQLite)
	b.run(t, &dockwellv1.ImportComposeRequest{Compose: shopFile})
	web := b.service(t, "shop", "web")
	port := uint32(10000)
	stale := 0
	for range 30 {
		port += 2
		first, err := b.s.AddServicePort(t.Context(), connect.NewRequest(&dockwellv1.AddServicePortRequest{Id: web.GetId(), HostPort: port, ContainerPort: 80}))
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Go(func() { applied(t, b.c, first.Msg.GetChange()) })
		for range 40 {
			second, err := b.s.AddServicePort(t.Context(), connect.NewRequest(&dockwellv1.AddServicePortRequest{Id: web.GetId(), HostPort: port + 1, ContainerPort: 80}))
			if reasonOf(err) == "plan_stale" {
				stale++
				continue
			}
			if err != nil {
				break // the first has applied, and this port's own check ended the round
			}
			for _, r := range second.Msg.GetChange().GetPlan().GetResources() {
				for _, c := range r.GetChanges() {
					number := strconv.FormatUint(uint64(port), 10)
					if strings.Contains(c.GetFrom(), number) && !strings.Contains(c.GetTo(), number) {
						t.Errorf("the plan for %d drops %d: %v", port+1, port, r.GetChanges())
					}
				}
			}
			if _, err := b.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: second.Msg.GetChange().GetId()})); err != nil {
				t.Fatal(err)
			}
		}
		wg.Wait()
	}
	t.Logf("%d plans were refused as stale", stale)
}

// To the API and CLI an update of what runs used to read as a new app, and a
// file's name: can match a project by chance.
func TestAFileForARunningProjectSaysSo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		two := "name: shop\nservices:\n  web:\n    image: ghcr.io/acme/shop:1.4\n  worker:\n    image: ghcr.io/acme/worker:1.0\n"
		fresh := b.file(t, &dockwellv1.ImportComposeRequest{Compose: two})
		if got := fresh.GetSummary(); got != "Run shop from a Compose file" {
			t.Errorf("a new project's summary = %q", got)
		}
		for _, n := range fresh.GetPlan().GetNotes() {
			t.Errorf("a new project's plan notes %v", n)
		}
		b.done(t, fresh)

		update := func(t *testing.T, req *dockwellv1.ImportComposeRequest) *dockwellv1.PlanNote {
			t.Helper()
			c := b.file(t, req)
			if got := c.GetSummary(); got != "Update shop from a Compose file" {
				t.Errorf("an update's summary = %q", got)
			}
			notes := c.GetPlan().GetNotes()
			if len(notes) != 1 || notes[0].GetReason() != "updates_project" {
				t.Fatalf("an update's notes = %v", notes)
			}
			if _, err := b.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: c.GetId()})); err != nil {
				t.Fatal(err)
			}
			return notes[0]
		}
		// By the file's name, and by asking for it, alike.
		const edited = "name: shop\nservices:\n  web:\n    image: ghcr.io/acme/shop:1.5\n"
		note := update(t, &dockwellv1.ImportComposeRequest{Compose: edited})
		if !slices.Equal(note.GetNames(), []string{"shop", "web", "worker"}) ||
			!strings.Contains(note.GetMessage(), "shop already runs web and worker") ||
			!strings.Contains(note.GetMessage(), "give the file a project of its own") {
			t.Errorf("the note = %v", note)
		}
		if strings.Contains(note.GetMessage(), "deleted") {
			t.Errorf("nothing is deleted, and the note says %q", note.GetMessage())
		}
		if res := note.GetResource(); res.GetKind() != "project" || res.GetName() != "shop" {
			t.Errorf("the note is about %v", res)
		}
		update(t, &dockwellv1.ImportComposeRequest{Project: "shop", Compose: strings.TrimPrefix(edited, "name: shop\n")})

		// A token can delete a service with one flag, so the plan names it.
		note = update(t, &dockwellv1.ImportComposeRequest{Compose: edited, RemoveOrphans: true})
		if !strings.Contains(note.GetMessage(), "and worker, which the file doesn't name, would be deleted") {
			t.Errorf("with remove_orphans, the note = %q", note.GetMessage())
		}
	})
}

// A port something else holds used to plan as normal: applying stopped and
// removed what ran, failed to start it, and rolled it back.
func TestAPortSomethingElseHoldsIsRefused(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		one := b.run(t, &dockwellv1.ImportComposeRequest{Compose: "name: one\nservices:\n  holder:\n    image: ghcr.io/acme/shop:1.4\n    ports: [\"8080:80\"]\n  beside:\n    image: ghcr.io/acme/shop:1.4\n"})
		two := b.run(t, &dockwellv1.ImportComposeRequest{Compose: "name: two\nservices:\n  asker:\n    image: ghcr.io/acme/shop:1.4\n"})
		asker := b.service(t, "two", "asker")
		askerName := docker.ContainerName(docker.Project(two), "asker")
		before := b.e.Serving(askerName).ID
		refused := func(t *testing.T, err error, words ...string) {
			t.Helper()
			if reasonOf(err) != "agent_failed" {
				t.Fatalf("got %v, want it refused at plan time", err)
			}
			for _, w := range words {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%q doesn't say %q", err.Error(), w)
				}
			}
			if got := b.e.Serving(askerName); got == nil || got.ID != before || !got.State.Running {
				t.Errorf("planning touched what runs: %+v", got)
			}
		}
		add := func(id string, port uint32) error {
			_, err := b.s.AddServicePort(t.Context(), connect.NewRequest(&dockwellv1.AddServicePortRequest{Id: id, HostPort: port, ContainerPort: 80}))
			return err
		}

		// By name, in another environment and in its own.
		refused(t, add(asker.GetId(), 8080), "holder, in another environment, publishes port 8080, so asker can't", "Choose another port")
		refused(t, add(b.service(t, "one", "beside").GetId(), 8080), "holder publishes port 8080, so beside can't")
		if err := add(b.service(t, "one", "beside").GetId(), 8080); strings.Contains(err.Error(), "another environment") {
			t.Errorf("in its own environment, it says %q", err.Error())
		}
		// A Compose file, and a new app, alike.
		_, err := b.cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: "name: three\nservices:\n  c:\n    image: ghcr.io/acme/shop:1.4\n    ports: [\"8080:80\"]\n"}))
		refused(t, err, "holder, in another environment, publishes port 8080")
		_, err = b.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "New", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: two, Name: "fresh"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
				Image: "ghcr.io/acme/shop:1.4", Ports: []*resourcev1.PortMapping{{HostPort: 8080, ContainerPort: 80}},
			}}}}},
		}}}))
		refused(t, err, "holder, in another environment, publishes port 8080, so fresh can't")

		// A container Dockwell didn't make.
		client := b.e.Client()
		id, err := client.CreateContainer(t.Context(), "by-hand", &docker.CreateRequest{
			Config:     docker.Config{Image: "ghcr.io/acme/shop:1.4"},
			HostConfig: docker.HostConfig{PortBindings: map[string][]docker.PortBinding{"80/tcp": {{HostPort: "9090", HostIP: "127.0.0.1"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.StartContainer(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		refused(t, add(asker.GetId(), 9090), "The container by-hand, which Dockwell didn't make, publishes port 9090")

		// Addresses that can't clash, and another protocol, aren't refused.
		free := func(t *testing.T, req *dockwellv1.AddServicePortRequest) {
			t.Helper()
			req.Id, req.ContainerPort = asker.GetId(), 80
			res, err := b.s.AddServicePort(t.Context(), connect.NewRequest(req))
			if err != nil {
				t.Errorf("%v = %v, want it planned", req, err)
				return
			}
			if _, err := b.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: res.Msg.GetChange().GetId()})); err != nil {
				t.Fatal(err)
			}
		}
		free(t, &dockwellv1.AddServicePortRequest{HostPort: 9090, HostIp: "127.0.0.2"})
		free(t, &dockwellv1.AddServicePortRequest{HostPort: 9090, Protocol: resourcev1.Protocol_PROTOCOL_UDP})
		free(t, &dockwellv1.AddServicePortRequest{HostPort: 9191})
		refused(t, add(asker.GetId(), 9090), "by-hand") // every address includes that one

		// A stopped container holds nothing.
		if err := client.StopContainer(t.Context(), id, time.Second); err != nil {
			t.Fatal(err)
		}
		free(t, &dockwellv1.AddServicePortRequest{HostPort: 9090})

		// A plan isn't refused for what it leaves as it is: re-importing a file
		// whose port is taken by nothing new changes nothing, and a service keeps
		// its own port as it is replaced.
		reimport := b.file(t, &dockwellv1.ImportComposeRequest{Compose: "name: one\nservices:\n  holder:\n    image: ghcr.io/acme/shop:1.5\n    ports: [\"8080:80\"]\n  beside:\n    image: ghcr.io/acme/shop:1.4\n"})
		b.done(t, reimport)

		// Nor for what is around what it leaves as it is: a stopped service whose
		// port something took since is no one's plan to refuse until it starts.
		stale := "name: five\nservices:\n  still:\n    image: ghcr.io/acme/shop:1.4\n    ports: [\"6000:80\"]\n"
		five := b.run(t, &dockwellv1.ImportComposeRequest{Compose: stale})
		if err := client.StopContainer(t.Context(), b.e.Newest(docker.Project(five), "still").ID, time.Second); err != nil {
			t.Fatal(err)
		}
		other, err := client.CreateContainer(t.Context(), "took-it", &docker.CreateRequest{
			Config:     docker.Config{Image: "ghcr.io/acme/shop:1.4"},
			HostConfig: docker.HostConfig{PortBindings: map[string][]docker.PortBinding{"80/tcp": {{HostPort: "6000"}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.StartContainer(t.Context(), other); err != nil {
			t.Fatal(err)
		}
		again, err := b.cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: stale}))
		if err != nil {
			t.Errorf("re-importing what doesn't change, with its port taken since = %v", err)
		} else if _, err := b.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: again.Msg.GetChange().GetId()})); err != nil {
			t.Fatal(err)
		}
		// Asked to run, it's about to start, so that's refused, by name.
		_, err = b.s.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: b.service(t, "five", "still").GetId(), Image: "ghcr.io/acme/shop:1.4"}))
		if err == nil || !strings.Contains(err.Error(), "took-it") {
			t.Errorf("deploying a stopped service, with its port taken since = %v", err)
		}

		// What the plan itself moves off a port is free for another in it, and
		// two wanting one in the same plan can't both have it.
		moved := b.file(t, &dockwellv1.ImportComposeRequest{Compose: "name: one\nservices:\n  holder:\n    image: ghcr.io/acme/shop:1.5\n    ports: [\"8081:80\"]\n  beside:\n    image: ghcr.io/acme/shop:1.4\n    ports: [\"8080:80\"]\n"})
		if _, err := b.c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: moved.GetId()})); err != nil {
			t.Fatal(err)
		}
		_, err = b.cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Compose: "name: four\nservices:\n  a:\n    image: ghcr.io/acme/shop:1.4\n    ports: [\"7000:80\"]\n  b:\n    image: ghcr.io/acme/shop:1.4\n    ports: [\"7000:80\"]\n"}))
		refused(t, err, "also publishes port 7000 in this same change")
		if b.e.Newest(docker.Project(one), "holder") == nil {
			t.Error("the holder is gone")
		}
	})
}

// Listing services reads every sealed spec, so it can't read a key for each.
func TestListingServicesReadsInFewQueries(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		logger := slog.New(slog.DiscardHandler)
		h := services.New(changes.New(b.f.engine, b.f.store, logger), b.f.engine, b.f.store, b.f.keeper, logger)
		read := func() int64 {
			t.Helper()
			var n atomic.Int64
			res, err := h.ListServices(store.CountingQueries(t.Context(), &n), connect.NewRequest(&dockwellv1.ListServicesRequest{Project: "shop"}))
			if err != nil || len(res.Msg.GetServices()) == 0 {
				t.Fatalf("ListServices = %v, %v", res, err)
			}
			return n.Load()
		}
		b.run(t, &dockwellv1.ImportComposeRequest{Compose: shopFile})
		few := read()
		var file strings.Builder
		file.WriteString("name: shop\nservices:\n")
		for i := range 8 {
			fmt.Fprintf(&file, "  s%d:\n    image: ghcr.io/acme/shop:1.4\n", i)
		}
		b.run(t, &dockwellv1.ImportComposeRequest{Compose: file.String()})
		if many := read(); many > few+1 {
			// A server's name reads once, and a deployment's, by service.
			t.Errorf("ListServices takes %d queries for 9 services, and %d for one", many, few)
		}
	})
}

// What a service uses is in what the API says of it, and a rollback brings back
// which it uses, with each at its latest value.
func TestARollbackKeepsWhatAServiceUsesAtItsLatestValue(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		b := newBench(t, backend)
		const value, newer = "first-secret-value", "second-secret-value"
		propose := func(summary string, edits ...*dockwellv1.Edit) *dockwellv1.Change {
			t.Helper()
			res, err := b.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: summary, Edits: edits}))
			if err != nil {
				t.Fatalf("%s: %v", summary, err)
			}
			return res.Msg.GetChange()
		}
		app := func(tag string) *dockwellv1.Edit {
			return &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Name: "web"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
					Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/shop:" + tag}},
					Secrets:   map[string]string{"API_KEY": ""},
					Variables: map[string]string{"LEVEL": "LOG_LEVEL"},
				}}},
			}
		}
		b.done(t, propose("Run web", secretSpec("API_KEY", value), variableSpec("LOG_LEVEL", "info"), app("1.4")))
		web := b.service(t, "", "web")
		if got := web.GetSpec().GetSecrets(); got["API_KEY"] != "API_KEY" || len(got) != 1 {
			t.Errorf("the service's secrets = %v", got)
		}
		if got := web.GetSpec().GetVariables(); got["LEVEL"] != "LOG_LEVEL" {
			t.Errorf("the service's variables = %v", got)
		}
		b.done(t, propose("Deploy", app("1.5")))

		// A new value restarts it, and that revision differs only in what it pins.
		b.done(t, propose("Rotate", secretSpec("API_KEY", newer)))
		list, err := b.s.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: web.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		revs := list.Msg.GetRevisions()
		if len(revs) != 3 || revs[0].GetImage() != "ghcr.io/acme/shop:1.5" || revs[2].GetImage() != "ghcr.io/acme/shop:1.4" {
			t.Fatalf("the revisions read %v", revs)
		}
		if !revs[1].GetMatchesLatest() {
			t.Errorf("r2 has the same image, and a change would pin it to the same value, so it matches")
		}
		_, err = b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: revs[1].GetNumber()}))
		if reasonOf(err) != "cant_undo" {
			t.Errorf("going back to a revision with only an older value = %v, want it refused: it would change nothing", err)
		}

		back, err := b.s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: revs[2].GetNumber()}))
		if err != nil {
			t.Fatal(err)
		}
		var note *dockwellv1.PlanNote
		for _, n := range back.Msg.GetChange().GetPlan().GetNotes() {
			if n.GetReason() == "values_latest" {
				note = n
			}
		}
		if note == nil || !slices.Equal(note.GetNames(), []string{"API_KEY", "LEVEL"}) || !strings.Contains(note.GetMessage(), "not what they held then") {
			t.Errorf("the plan's note = %v", note)
		}
		b.done(t, back.Msg.GetChange())
		web = b.service(t, "", "web")
		if got := resource.BareImage(web.GetSpec().GetApp().GetImage()); got != "ghcr.io/acme/shop:1.4" {
			t.Errorf("it runs %s", got)
		}
		secret := listed(t, variablesClient(b.b, b.b.origin), "API_KEY")
		if got := web.GetSpec().GetRevisions()["API_KEY"]; got != secret.GetRevision() {
			t.Errorf("it pins API_KEY at r%d, and the latest is r%d", got, secret.GetRevision())
		}
		if strings.Contains(web.String(), value) || strings.Contains(web.String(), newer) {
			t.Error("the API says a secret's value")
		}
	})
}
