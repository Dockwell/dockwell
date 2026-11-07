// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

var taken = Target{Name: "env_x-api-1", Project: "env_x", Service: "api", Scoped: true}

// takenByHand is a container someone ran, named legacy_api, and read and taken on as
// the service taken.
func takenByHand(t *testing.T) (*Docker, *dockertest.Engine, string, *resourcev1.Container) {
	t.Helper()
	e := dockertest.New(t)
	e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	e.AddLocalImage("nginx:1.26", dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}})
	c := e.Client()
	c.Adopted = &docker.Adopted{}
	d := &Docker{Client: c, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	id, err := c.CreateContainer(t.Context(), "legacy_api", &docker.CreateRequest{Config: docker.Config{Image: "nginx:1.26"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	read, err := d.ReadAdoption(t.Context(), taken, id)
	if err != nil {
		t.Fatal(err)
	}
	spec := read.Spec.(*resourcev1.Container)
	if err := d.Adopt(t.Context(), taken, id, spec, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	return d, e, id, spec
}

// Letting go of a container is the promise that it goes on running, so a Docker error
// on the way must stop there. It mustn't read as nothing taken on, which goes on to
// stop and remove what's there, with what it wrote.
func TestLettingGoNeverBecomesARemoval(t *testing.T) {
	d, e, id, _ := takenByHand(t)
	failed := false
	e.Intercept(func(r *http.Request) error {
		if !failed && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json") {
			failed = true
			return errors.New("the engine is busy")
		}
		return nil
	})
	if err := d.Remove(t.Context(), taken); err == nil {
		t.Error("removing with Docker failing said nothing")
	}
	got := e.Container("legacy_api")
	if got == nil || !got.State.Running || got.ID != id {
		t.Errorf("after an error reading what's there, the container is %+v, want it running as it was", got)
	}

	// And it can't plan one either: not knowing isn't knowing it was never taken on.
	failed = false
	if err := d.Judge(t.Context(), taken, nil, &resource.Decision{Action: resource.ActionDelete}); err == nil {
		t.Error("planning with Docker failing said nothing")
	}
}

// A step can be cut off once the agent has recorded what it took on, and before it
// ends, so planning it again has to find that done, not refuse what it did.
func TestAResumedTakeOnFindsItDone(t *testing.T) {
	d, _, id, spec := takenByHand(t)
	dec, err := d.Adoption(t.Context(), taken, id, spec)
	if err != nil {
		t.Fatalf("planning a take-on that's done: %v", err)
	}
	if dec.Adopts != id {
		t.Errorf("it plans %+v, want it to take on %s", dec, id)
	}
	if err := d.Adopt(t.Context(), taken, id, spec, resource.Decision{}); err != nil {
		t.Errorf("finishing a take-on that's done: %v", err)
	}

	// As another service, it's still refused.
	other := Target{Name: "env_x-other-1", Project: "env_x", Service: "other", Scoped: true}
	if _, err := d.Adoption(t.Context(), other, id, spec); err == nil || !strings.Contains(err.Error(), "already what the service api runs") {
		t.Errorf("taking it on as another service: %v", err)
	}
}

// A stop that fails brings back what was there, under the name it had: what's on its
// network reaches it by that, and any other reads as drift.
func TestAStopThatFailsLeavesTheNameItCameWith(t *testing.T) {
	d, e, _, _ := takenByHand(t)
	all, err := d.instances(t.Context(), taken)
	if err != nil || len(all) != 1 {
		t.Fatalf("instances = %v, %v", all, err)
	}
	o := all[0]
	if err := d.markStopping(t.Context(), taken, o); err != nil {
		t.Fatal(err)
	}
	if err := d.bringBack(t.Context(), taken, o, nil); err != nil {
		t.Fatal(err)
	}
	got := e.Container("legacy_api")
	if got == nil {
		t.Fatalf("legacy_api is gone: Docker has %v", e.Containers())
	}
	read, err := d.Client.InspectContainer(t.Context(), got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Config.Labels[docker.LabelAdopted] != "legacy_api" {
		t.Errorf("it reads as having come as %q", read.Config.Labels[docker.LabelAdopted])
	}
}

// What a spec exposes reads back as it said, on an image that exposes it too, and a
// port it publishes or its image exposes isn't read as its own, so nothing reads as
// drift.
func TestWhatASpecExposesReadsBack(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("exposing:1", dockertest.Image{Config: docker.Config{Cmd: []string{"run"}, ExposedPorts: map[string]struct{}{"80/tcp": {}, "9000/tcp": {}}}})
	d := &Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	spec := &resourcev1.Container{
		Image:  "exposing:1",
		Expose: []string{"3000/tcp", "9000/tcp"},
		Ports:  []*resourcev1.PortMapping{{HostPort: 18080, ContainerPort: 80, Protocol: resourcev1.Protocol_PROTOCOL_TCP}},
	}
	Normalize(spec)
	if err := d.Run(t.Context(), taken, spec, resource.Decision{Action: resource.ActionCreate}); err != nil {
		t.Fatal(err)
	}
	c, err := d.Client.ContainerNamed(t.Context(), taken.Name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.projected(t.Context(), taken, c)
	if err != nil {
		t.Fatal(err)
	}
	Normalize(got)
	if diff := resource.Diff(spec, got); len(diff) != 0 {
		t.Errorf("what it exposes reads back as different: %+v", diff)
	}
}

// Docker can't put a label on what's running, so what a take-on approves of where its
// paths lead goes on its record, and reads as a label from then on: one that leads
// elsewhere today doesn't read as moved on every plan after. A path that moved since
// its plan isn't what anyone approved.
func TestTakingOnKeepsWhereItsPathsLead(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	site, other, link := filepath.Join(dir, "site"), filepath.Join(dir, "other"), filepath.Join(dir, "link")
	for _, p := range []string{site, other} {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(site, link); err != nil {
		t.Fatal(err)
	}
	e := dockertest.New(t)
	e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	e.AddLocalImage("nginx:1.26", dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}})
	c := e.Client()
	c.Adopted = &docker.Adopted{}
	d := &Docker{Client: c, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Paths: &Paths{Client: c, DataDir: filepath.Join(dir, "dockwell")}}
	id, err := c.CreateContainer(t.Context(), "legacy_api", &docker.CreateRequest{
		Config:     docker.Config{Image: "nginx:1.26"},
		HostConfig: docker.HostConfig{Binds: []string{link + ":/conf"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	read, err := d.ReadAdoption(t.Context(), taken, id)
	if err != nil {
		t.Fatal(err)
	}
	spec := read.Spec.(*resourcev1.Container)
	planned := resource.Decision{PlannedLeads: map[string]string{link: site}}

	if err := d.Adopt(t.Context(), taken, id, spec, resource.Decision{}); err == nil || !strings.Contains(err.Error(), "it wasn't taken on") {
		t.Fatalf("taking it on where its plan said nothing of the link = %v", err)
	}
	if err := d.Adopt(t.Context(), taken, id, spec, planned); err != nil {
		t.Fatal(err)
	}
	got, err := c.InspectContainer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"` + link + `":"` + site + `"}`; got.Config.Labels[LabelLeads] != want {
		t.Errorf("it reads as %q, want %q", got.Config.Labels[LabelLeads], want)
	}
	if moved, err := d.LeadsMoved(t.Context(), taken, spec); err != nil || len(moved) != 0 {
		t.Errorf("what it was taken on with reads as moved: %v, %v", moved, err)
	}
	if err := d.Paths.LeadsAsMade(t.Context(), got, nil); err != nil {
		t.Errorf("starting it as it was taken on: %v", err)
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if moved, err := d.LeadsMoved(t.Context(), taken, spec); err != nil || len(moved) != 1 {
		t.Errorf("a path that moved since = %v, %v", moved, err)
	}
	if err := d.Paths.LeadsAsMade(t.Context(), got, nil); err == nil {
		t.Error("starting it over a path that moved passed")
	}
}
