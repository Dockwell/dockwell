// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func webAt(env, path string, servers ...string) Edit {
	var mounts []*resourcev1.Mount
	if path != "" {
		mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: path}, Target: "/data"}}
	}
	return Edit{
		Ref:     resource.Ref{Kind: service.Kind, Scope: env, Name: "web"},
		Spec:    &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27", Mounts: mounts}}},
		Servers: servers,
	}
}

// What the gate should catch, by reading the specs: these should all pass.
func TestTheGateSeesTheseMoves(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, engines, db := setupDocker(t, b, "srv_A", "srv_B")
		env := production(t, db)
		needs := func(what string, p *Plan, want bool) {
			t.Helper()
			if p.HostAccess() != want {
				t.Errorf("%s: host access %v, want %v: %+v", what, p.HostAccess(), want, p.Resources)
			}
		}
		first := propose(t, e, "Mount", webAt(env, "/srv/x", "srv_A"))
		needs("a new service with a path", first, true)
		apply(t, e, first)
		needs("a new tag only", propose(t, e, "Same", webAt(env, "/srv/x", "srv_A")), false)

		move := propose(t, e, "Move", webAt(env, "/srv/x", "srv_B"))
		needs("moved to another server", move, true)
		spread := propose(t, e, "Spread", webAt(env, "/srv/x", "srv_A", "srv_B"))
		needs("placed on one more server", spread, true)
		if len(spread.Resources[0].HostAccess) != 1 {
			t.Errorf("spread: %+v", spread.Resources[0].HostAccess)
		}

		rm := propose(t, e, "Remove", webAt(env, "", "srv_A"))
		needs("removing it", rm, false)
		apply(t, e, rm)
		c := stored(t, db, rm.Change)
		undo, err := e.Undo(t.Context(), c.ID, By{Actor: "token:x", Via: ViaAPI})
		if err != nil {
			t.Fatal(err)
		}
		needs("a token undoing the removal", undo, true)
		if err := e.Discard(t.Context(), undo.Change, By{Actor: "x", Via: ViaAPI}); err != nil {
			t.Fatal(err)
		}

		// A token's undo of an undo, and a delete then a create.
		del := propose(t, e, "Delete", Edit{Ref: webAt(env, "").Ref})
		apply(t, e, del)
		needs("a create after a delete", propose(t, e, "Again", webAt(env, "/srv/x", "srv_A")), true)
		needs("from container to service", propose(t, e, "Container", Edit{
			Ref:  resource.Ref{Kind: container.Kind, Scope: "srv_A", Name: "web2"},
			Spec: webAt(env, "/srv/x").Spec.(*resourcev1.Service).GetApp(),
		}), true)

		// Adopting: a mount made by hand.
		apply(t, e, propose(t, e, "Plain", webAt(env, "", "srv_A")))
		name := service.ContainerName(webAt(env, "").Ref)
		engines["srv_A"].Edit(name, func(c *docker.Container) {
			c.HostConfig.Binds = append(c.HostConfig.Binds, "/etc:/hand")
			c.Mounts = append(c.Mounts, docker.MountPoint{Type: "bind", Source: "/etc", Destination: "/hand", RW: true})
		})
		drift, err := e.Drift(t.Context(), "srv_A")
		if err != nil || len(drift) != 1 {
			t.Fatalf("drift = %+v, %v", drift, err)
		}
		adopt, err := e.Adopt(t.Context(), drift[0], By{Actor: "token:x", Via: ViaAPI})
		if err != nil {
			t.Fatal(err)
		}
		needs("adopting a mount made by hand", adopt, true)
		var listed int
		for _, s := range adopt.Steps() {
			listed += len(s.HostAccess)
		}
		t.Logf("adopt: %d steps, %d listed on steps, %d on the resource", len(adopt.Steps()), listed, len(adopt.Resources[0].HostAccess))
	})
}

// controlKinds is the control plane's kinds.
func controlKinds() *resource.Registry {
	return resource.NewRegistry(&container.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{})
}

// A restore the control plane stopped in the middle of: when it starts
// again the volume's copy is gone. Resume plans again, with no fingerprint,
// so the step that was to fail becomes an empty create.
func TestResumeKeepsARestoreARestore(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, engines, db := setupDocker(t, b, "srv_A")
		env := production(t, db)
		ref := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
		apply(t, e, propose(t, e, "Make", Edit{Ref: ref, Spec: &resourcev1.Volume{}}))
		del := propose(t, e, "Delete", Edit{Ref: ref})
		apply(t, e, del)
		undo, err := e.Undo(t.Context(), del.Change, By{Actor: "person", Via: ViaPanel})
		if err != nil {
			t.Fatal(err)
		}
		if s := undo.Steps(); len(s) != 1 || !s[0].Restores {
			t.Fatalf("the undo = %+v, want a restore", s)
		}

		plain := e.agents.(onDocker)["srv_A"]
		started := make(chan struct{}, 1)
		e.agents = onDocker{"srv_A": hooked{Agent: plain, apply: func(ctx context.Context, _ resource.Step, _ proto.Message) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}}}
		if err := e.Start(t.Context(), undo.Change, By{Actor: "person", Via: ViaPanel}); err != nil {
			t.Fatal(err)
		}
		<-started
		grace, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		if err := e.Stop(grace); err != nil {
			t.Fatal(err)
		}
		if c := stored(t, db, undo.Change); c.State != store.ChangeApplying {
			t.Fatalf("after the stop the undo is %s", c.State)
		}

		// While the control plane is down, the prune takes the kept copy.
		name := docker.ScopedName(docker.Project(env), "data")
		if err := engines["srv_A"].Client().RemoveVolume(t.Context(), name); err != nil {
			t.Fatal(err)
		}

		next := New(db, controlKinds(), onDocker{"srv_A": plain}, e.sealer, slog.New(slog.DiscardHandler))
		err = next.Resume(t.Context())
		next.Wait()
		c := stored(t, db, undo.Change)
		t.Logf("Resume = %v, the undo is %s, volume on the server: %v", err, c.State, engines["srv_A"].Volume(name) != nil)
		if engines["srv_A"].Volume(name) != nil {
			t.Errorf("the restore became an empty volume, with no one reviewing it; the undo is %s", c.State)
		}
	})
}

// A service reaches what its volumes and networks do, read-only where it
// mounts them so. The control plane reads that from their specs, so what
// adds to a service's reach through them needs a person, and what keeps it
// as it is doesn't.
// Another image is any code at all with what a service reaches, so on what's as good as
// root it needs a person. Another tag of the same repository is how CI deploys, and so
// is what a tag points at now, and those go on as they did. So does a limit, which runs
// nothing, and what isn't as good as root.
func TestANewImageOnWhatIsAsGoodAsRootNeedsAPerson(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, engines, db := setupDocker(t, b, "srv_A")
		engines["srv_A"].Registry["ghcr.io/stranger/ops:1.0"] = dockertest.Image{}
		env := production(t, db)
		app := func(image string, mutate func(*resourcev1.Container)) Edit {
			c := &resourcev1.Container{
				Image:  image,
				Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/var/run/docker.sock"}, Target: "/var/run/docker.sock"}},
			}
			if mutate != nil {
				mutate(c)
			}
			return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "agent"}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}}
		}
		apply(t, e, propose(t, e, "Run it", app("nginx:1.27", nil)))

		other := propose(t, e, "Another image", app("ghcr.io/stranger/ops:1.0", nil))
		if !other.HostAccess() {
			t.Errorf("another image asks nothing of anyone: %+v", other.Resources)
		}
		if a := other.Resources[0].HostAccess; len(a) != 1 || !a[0].AsGoodAsRoot() || a[0].Path != "/var/run/docker.sock" {
			t.Errorf("another image says it reaches %+v, want the socket", a)
		}
		if err := e.Discard(t.Context(), other.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}

		if p := propose(t, e, "Another tag", app("nginx:1.28", nil)); p.HostAccess() {
			t.Errorf("another tag needs a person: %+v", p.Resources)
		} else if err := e.Discard(t.Context(), p.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}

		again := app("nginx:1.27", nil)
		again.Pull = true
		if p := propose(t, e, "What the tag points at now", again); p.HostAccess() {
			t.Errorf("pulling its tag again needs a person: %+v", p.Resources)
		} else if err := e.Discard(t.Context(), p.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}

		// So for what Dockwell made, a new command counts as a new image does.
		if p := propose(t, e, "Another command", app("nginx:1.27", func(c *resourcev1.Container) {
			c.Command = []string{"sh", "-c", "docker ps"}
		})); !p.HostAccess() {
			t.Errorf("another command asks nothing of anyone: %+v", p.Resources)
		} else if err := e.Discard(t.Context(), p.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}

		limit := func(c *resourcev1.Container) { c.Resources = &resourcev1.Resources{Memory: "512M"} }
		if p := propose(t, e, "A limit and another tag", app("nginx:1.28", limit)); p.HostAccess() {
			t.Errorf("a limit beside another tag needs a person: %+v", p.Resources)
		} else if err := e.Discard(t.Context(), p.Change, By{Actor: "usr_test"}); err != nil {
			t.Fatal(err)
		}
		if p := propose(t, e, "A limit", app("nginx:1.27", limit)); p.HostAccess() {
			t.Errorf("a change of the same image needs a person: %+v", p.Resources)
		}
	})
}

// A mount that can write the folder another's source is in can turn that source into a
// link, and Docker follows it when what mounts it restarts by itself, where no agent is
// asked. So the mount that can do that counts as root, however the two came to be on
// one server, in either order, in one service or two, and a volume's path counts too.
// What reads, or sits beside rather than inside, or is on another server, doesn't.
func TestAMountThatCanMoveAnothersSourceIsAsGoodAsRoot(t *testing.T) {
	bind := func(p string) *resourcev1.Mount {
		return &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/m" + strings.ReplaceAll(p, "/", "-")}
	}
	readOnly := func(p string) *resourcev1.Mount {
		m := bind(p)
		m.ReadOnly = true
		return m
	}
	for name, tt := range map[string]struct {
		first, then []*resourcev1.Mount
		root        bool
	}{
		"a folder, then what can write around it": {first: []*resourcev1.Mount{bind("/srv/app/conf")}, then: []*resourcev1.Mount{bind("/srv")}, root: true},
		"what can write, then a folder inside it": {first: []*resourcev1.Mount{bind("/srv")}, then: []*resourcev1.Mount{bind("/srv/app/conf")}, root: true},
		"a read-only source still moves":          {first: []*resourcev1.Mount{bind("/srv")}, then: []*resourcev1.Mount{readOnly("/srv/app/conf")}, root: true},
		"a holder that only reads":                {first: []*resourcev1.Mount{readOnly("/srv")}, then: []*resourcev1.Mount{bind("/srv/app/conf")}},
		"the same folder":                         {first: []*resourcev1.Mount{bind("/srv/app")}, then: []*resourcev1.Mount{bind("/srv/app")}},
		"folders side by side":                    {first: []*resourcev1.Mount{bind("/srv/app")}, then: []*resourcev1.Mount{bind("/srv/application")}},
		"a volume, which is nobody's path":        {first: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}}, then: []*resourcev1.Mount{bind("/srv")}},
	} {
		t.Run(name, func(t *testing.T) {
			e, _, db := setupDocker(t, store.SQLite, "srv_A")
			env := production(t, db)
			edit := func(name string, mounts []*resourcev1.Mount) Edit {
				return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27", Mounts: mounts}}}}
			}
			apply(t, e, propose(t, e, "The first", Edit{Ref: resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}, Spec: &resourcev1.Volume{}, Servers: []string{"srv_A"}}, edit("first", tt.first)))
			plan := propose(t, e, "The other", edit("other", tt.then))
			if got := plan.AsGoodAsRoot(); got != tt.root {
				t.Fatalf("as good as root = %v, want %v: %+v", got, tt.root, plan.Resources)
			}
			if !tt.root {
				return
			}
			held := slices.ContainsFunc(plan.Resources[0].HostAccess, func(a resource.Access) bool { return a.Holds != "" })
			if !held {
				t.Errorf("the plan doesn't say what holds what: %+v", plan.Resources)
			}
			// Once it's so, it stays so without asking again, for what it doesn't touch.
			apply(t, e, plan)
			again := propose(t, e, "A limit", Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "other"}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27", Mounts: tt.then, Resources: &resourcev1.Resources{Memory: "512M"}}}}})
			if again.AsGoodAsRoot() {
				t.Errorf("a limit on it asks: %+v", again.Resources)
			}
		})
	}
}

// One service can do it to itself: a folder and a folder inside it, then a restart.
func TestOneServiceCanMoveItsOwnSource(t *testing.T) {
	e, _, db := setupDocker(t, store.SQLite, "srv_A")
	env := production(t, db)
	plan := propose(t, e, "Both", Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "web"}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "nginx:1.27", Mounts: []*resourcev1.Mount{
			{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/site"}, Target: "/site"},
			{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/site/app.conf"}, Target: "/etc/app.conf", ReadOnly: true},
		},
	}}}})
	if !plan.AsGoodAsRoot() {
		t.Errorf("it asks nothing more: %+v", plan.Resources)
	}
}

// What can move another's source is what #190's token would need: new code in it now
// asks for a person, as new code in anything as good as root does. Another tag doesn't,
// and neither does new code in what it could move, which is out of the token's reach
// while the other's code is.
func TestWhatCanMoveAnothersSourceKeepsAskingForNewCode(t *testing.T) {
	e, _, db := setupDocker(t, store.SQLite, "srv_A")
	env := production(t, db)
	app := func(image string, command []string, p string) *resourcev1.Service {
		return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: image, Command: command, Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/m"}},
		}}}
	}
	ref := func(name string) resource.Ref { return resource.Ref{Kind: service.Kind, Scope: env, Name: name} }
	apply(t, e, propose(t, e, "Both", Edit{Ref: ref("holder"), Spec: app("nginx:1.27", nil, "/srv")}, Edit{Ref: ref("held"), Spec: app("nginx:1.27", nil, "/srv/app/conf")}))

	if p := propose(t, e, "New code", Edit{Ref: ref("holder"), Spec: app("nginx:1.27", []string{"sh"}, "/srv")}); !p.AsGoodAsRoot() {
		t.Errorf("new code in what can move a source asks nothing: %+v", p.Resources)
	} else if err := e.Discard(t.Context(), p.Change, By{Actor: "usr_test"}); err != nil {
		t.Fatal(err)
	}
	if p := propose(t, e, "Another tag", Edit{Ref: ref("holder"), Spec: app("nginx:1.28", nil, "/srv")}); p.AsGoodAsRoot() {
		t.Errorf("another tag asks: %+v", p.Resources)
	} else if err := e.Discard(t.Context(), p.Change, By{Actor: "usr_test"}); err != nil {
		t.Fatal(err)
	}
	if p := propose(t, e, "New code in what's moved", Edit{Ref: ref("held"), Spec: app("nginx:1.27", []string{"sh"}, "/srv/app/conf")}); p.AsGoodAsRoot() {
		t.Errorf("new code in what can be moved asks: %+v", p.Resources)
	}
}

// Servers have paths of their own, so a mount on one holds nothing on another.
func TestAMountOnAnotherServerHoldsNothing(t *testing.T) {
	e, _, db := setupDocker(t, store.SQLite, "srv_A", "srv_B")
	env := production(t, db)
	edit := func(name, server, p string) Edit {
		return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name}, Servers: []string{server}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/m"}},
		}}}}
	}
	apply(t, e, propose(t, e, "On A", edit("held", "srv_A", "/srv/app/conf")))
	if p := propose(t, e, "On B", edit("holder", "srv_B", "/srv")); p.AsGoodAsRoot() {
		t.Errorf("a mount on another server holds it: %+v", p.Resources)
	}
}

// Where a path leads counts, as the same folder can be written two ways: one that goes
// through a link to a bigger disk, say.
func TestWhatsInsideGoesByWhereAPathLeads(t *testing.T) {
	leads := map[string]string{"/srv/data": "/mnt/disk/data"}
	for _, tt := range []struct {
		holder, source string
		want           bool
	}{
		{"/srv", "/srv/app", true},
		{"/srv/", "/srv/app/", true},
		{"/srv/data", "/mnt/disk/data/conf", true},
		{"/mnt/disk", "/srv/data", true},
		{"/srv", "/srv", false},
		{"/srv/", "/srv", false},
		{"/srv/app", "/srv/application", false},
		{"/srv/data", "/mnt/disk/other", false},
	} {
		if got := strictlyInside(tt.holder, tt.source, leads); got != tt.want {
			t.Errorf("strictlyInside(%s, %s) = %v, want %v", tt.holder, tt.source, got, tt.want)
		}
	}
}

// A volume's server path counts as a mount's does, on either side.
func TestAVolumeCanHoldAnothersSource(t *testing.T) {
	e, _, db := setupDocker(t, store.SQLite, "srv_A")
	env := production(t, db)
	nas := resource.Ref{Kind: volume.Kind, Scope: env, Name: "nas"}
	serve := func(name string, mount *resourcev1.Mount) Edit {
		return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: "nginx:1.27", Mounts: []*resourcev1.Mount{mount},
		}}}}
	}
	apply(t, e, propose(t, e, "Share",
		Edit{Ref: nas, Spec: &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": "/srv"}}, Servers: []string{"srv_A"}},
		serve("reader", &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/app/conf"}, Target: "/conf"})))
	p := propose(t, e, "Write the share", serve("writer", &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: "nas"}, Target: "/nas"}))
	if !p.AsGoodAsRoot() {
		t.Errorf("a volume that comes to hold a source asks nothing: %+v", p.Resources)
	}
}

func TestAServiceReachesWhatItsVolumesAndNetworksDo(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setupDocker(t, b, "srv_A")
		env := production(t, db)
		apply(t, e, propose(t, e, "Share", Edit{
			Ref:  resource.Ref{Kind: volume.Kind, Scope: env, Name: "nas"},
			Spec: &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": "/srv/nas"}},
		}))
		apply(t, e, propose(t, e, "LAN", Edit{
			Ref:  resource.Ref{Kind: network.Kind, Scope: env, Name: "lan"},
			Spec: &resourcev1.Network{Driver: "macvlan", Options: map[string]string{"parent": "eth0"}},
		}))
		app := func(name string, readOnly bool, nets ...string) Edit {
			return Edit{
				Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name},
				Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
					Image: "nginx:1.27", Networks: nets,
					Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "nas"}, Target: "/nas", ReadOnly: readOnly}},
				}}},
			}
		}
		needs := func(what string, p *Plan, want bool) {
			t.Helper()
			if p.HostAccess() != want {
				t.Errorf("%s: host access %v, want %v: %+v", what, p.HostAccess(), want, p.Resources)
			}
		}
		first := propose(t, e, "Web", app("web", true))
		needs("a new service that mounts it", first, true)
		want := resource.Access{Kind: resource.AccessPath, Path: "/srv/nas", ReadOnly: true}
		if got := first.Resources[0].HostAccess; len(got) != 1 || got[0] != want {
			t.Errorf("it reaches %+v, want %+v", got, want)
		}
		if got := first.Steps()[0].HostAccess; len(got) != 1 || got[0] != want {
			t.Errorf("its step lists %+v, want %+v", got, want)
		}
		apply(t, e, first)

		same := app("web", true)
		same.Spec.(*resourcev1.Service).GetApp().Image = "nginx:1.28"
		needs("another image", propose(t, e, "Tag", same), false)
		needs("read-write where it was read-only", propose(t, e, "Write", app("web", false)), true)
		needs("a second service", propose(t, e, "Thief", app("thief", true)), true)
		needs("joining the LAN", propose(t, e, "Join", app("web", true, "lan")), true)

		wrote := propose(t, e, "Write", app("web", false, "lan"))
		apply(t, e, wrote)
		needs("read-only where it was read-write", propose(t, e, "Less", app("web", true, "lan")), false)
		needs("leaving the LAN", propose(t, e, "Leave", app("web", false)), false)
	})
}

// A restore of an image's data that Resume reaches once the copy is gone
// fails too, rather than making the service with its data empty.
func TestResumeKeepsARestoreOfAnImagesData(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, engines, db := setupDocker(t, b, "srv_A")
		env := production(t, db)
		fake := engines["srv_A"]
		const data = "/var/lib/postgresql/data"
		fake.Registry["postgres:17"] = dockertest.Image{Volumes: []string{data}, Config: docker.Config{Cmd: []string{"postgres"}}}
		ref := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}
		spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "postgres:17"}}}
		apply(t, e, propose(t, e, "Make", Edit{Ref: ref, Spec: spec}))
		name := service.ContainerName(ref)
		var kept string
		for _, m := range fake.Container(name).Mounts {
			if m.Destination == data {
				kept = m.Name
			}
		}
		del := propose(t, e, "Delete", Edit{Ref: ref})
		if s := del.Steps(); len(s) != 1 || !s[0].Destroys {
			t.Fatalf("the delete = %+v", s)
		}
		apply(t, e, del)
		undo, err := e.Undo(t.Context(), del.Change, By{Actor: "person", Via: ViaPanel})
		if err != nil {
			t.Fatal(err)
		}
		if s := undo.Steps(); len(s) != 1 || !s[0].Restores {
			t.Fatalf("the undo = %+v, want a restore", s)
		}
		plain := e.agents.(onDocker)["srv_A"]
		started := make(chan struct{}, 1)
		e.agents = onDocker{"srv_A": hooked{Agent: plain, apply: func(ctx context.Context, _ resource.Step, _ proto.Message) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}}}
		if err := e.Start(t.Context(), undo.Change, By{Actor: "person", Via: ViaPanel}); err != nil {
			t.Fatal(err)
		}
		<-started
		grace, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		if err := e.Stop(grace); err != nil {
			t.Fatal(err)
		}
		if err := fake.Client().RemoveVolume(t.Context(), kept); err != nil {
			t.Fatal(err)
		}
		next := New(db, controlKinds(), onDocker{"srv_A": plain}, e.sealer, slog.New(slog.DiscardHandler))
		err = next.Resume(t.Context())
		next.Wait()
		if c := fake.Container(name); c != nil {
			t.Errorf("Resume made the service with its data empty: %+v, %v", c.Mounts, err)
		}
		if c := stored(t, db, undo.Change); c.State != store.ChangeFailed || !strings.Contains(c.Error, "is gone") {
			t.Errorf("the undo is %s: %q, want it failed, saying the data is gone", c.State, c.Error)
		}
	})
}

// A rollback planned while its agent can't plan comes from the last known
// state, which can't say what the server kept. What the failed change's
// steps took out of use it must bring back, so when Resume reaches the
// server, a copy that is gone fails, and one that's there comes back.
func TestARollbackWaitingForAServerRestores(t *testing.T) {
	for name, gone := range map[string]bool{"the copy is there": false, "the copy is gone": true} {
		t.Run(name, func(t *testing.T) {
			storetest.Each(t, func(t *testing.T, b store.Backend) {
				e, engines, db := setupDocker(t, b, "srv_A")
				env := production(t, db)
				fake := engines["srv_A"]
				vol := func(n string) resource.Ref { return resource.Ref{Kind: volume.Kind, Scope: env, Name: n} }
				apply(t, e, propose(t, e, "Make", Edit{Ref: vol("a"), Spec: &resourcev1.Volume{}}, Edit{Ref: vol("b"), Spec: &resourcev1.Volume{}}))
				del := propose(t, e, "Delete", Edit{Ref: vol("a")}, Edit{Ref: vol("b")})

				// Deletes run last first, so b goes, and a fails, and then
				// the agent can't plan the rollback.
				var broken atomic.Bool
				plain := e.agents.(onDocker)["srv_A"]
				e.agents = onDocker{"srv_A": hooked{
					Agent: plain,
					apply: func(_ context.Context, step resource.Step, _ proto.Message) error {
						if step.Ref.Name == "a" && step.Action == resource.ActionDelete {
							broken.Store(true)
							return errors.New("the step failed")
						}
						return nil
					},
					plan: func(context.Context, []resource.Desired) error {
						if broken.Load() {
							return errors.New("the agent can't plan now")
						}
						return nil
					},
				}}
				if err := e.Apply(t.Context(), del.Change); err == nil {
					t.Fatal("the delete applied")
				}
				e.Wait()
				rollback := ""
				records, err := e.Changes(t.Context(), store.ChangePage{}, 10)
				if err != nil {
					t.Fatal(err)
				}
				for i := range records {
					if records[i].Reverts == del.Change {
						rollback = records[i].ID
					}
				}
				r, err := e.Change(t.Context(), rollback)
				if err != nil {
					t.Fatal(err)
				}
				var restores bool
				if s := r.Plan.step("srv_A", vol("b")); s != nil {
					restores = s.Restores
				}
				if !restores {
					t.Fatalf("the rollback's plan for b, from the last known state = %+v, want it to restore", r.Plan.Servers)
				}
				name := docker.ScopedName(docker.Project(env), "b")
				if gone {
					if err := fake.Client().RemoveVolume(t.Context(), name); err != nil {
						t.Fatal(err)
					}
				}
				broken.Store(false)
				_ = e.Resume(t.Context())
				e.Wait()
				c := stored(t, db, rollback)
				switch {
				case gone && (fake.Volume(name) != nil || c.State != store.ChangeFailed):
					t.Errorf("with the copy gone, the rollback is %s, and the volume is there: %v", c.State, fake.Volume(name) != nil)
				case !gone && (fake.Volume(name) == nil || c.State != store.ChangeApplied):
					t.Errorf("with the copy there, the rollback is %s: %s, and the volume is there: %v", c.State, c.Error, fake.Volume(name) != nil)
				}
			})
		})
	}
}
