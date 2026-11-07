// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// followingPaths is an engine with one server whose agent follows its server paths on
// this disk, as an agent does on its own server.
func followingPaths(t *testing.T, dataDir string) (*Engine, *store.DB, *dockertest.Engine) {
	t.Helper()
	db, _ := storetest.Open(t, store.SQLite)
	if _, err := Setup(t.Context(), db, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	agents := onDocker{}
	e := dockertest.New(t)
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateServer(t.Context(), store.Server{ID: "srv_A", Name: "srv_A", CreatedAt: now, CertSerial: "srv_A", CertNotAfter: now}); err != nil {
			return err
		}
		e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
		client, kept := e.Client(), &volume.Kept{}
		paths := &container.Paths{Client: client, DataDir: dataDir}
		containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Deleted: kept, Paths: paths}}
		agents["srv_A"] = resource.Local{Drivers: resource.NewRegistry(&containers, &service.Driver{Containers: containers},
			&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: kept, Paths: paths}}, &network.Driver{Client: client})}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&container.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{})
	return New(db, kinds, agents, secretstest.Keeper(db, kinds), slog.New(slog.DiscardHandler)), db, e
}

// One folder can be written two ways, through a link and not, and a link can be any
// part of a path. What holds another's source counts however each is written, whether
// a mount names the path or a bind volume does, and whichever is in the plan: the agent
// says where every path on its server leads, for every plan that runs something, not
// only the plan's own paths. A source that's a link itself is still a file in a folder
// the holder can replace, so that counts too.
func TestAHoldWrittenThroughALinkIsCounted(t *testing.T) {
	for name, tt := range map[string]struct {
		holder, held func(disk, srv string) string
		// heldFirst says which goes first. The other is the plan, and then new code in
		// the holder is, with nothing of the held one in it.
		heldFirst bool
		// holderVolume and heldVolume mount through a bind volume.
		holderVolume, heldVolume bool
	}{
		"a source through a link, the holder first": {
			holder: func(disk, _ string) string { return disk },
			held:   func(_, srv string) string { return filepath.Join(srv, "app", "conf") },
		},
		"a source through a link, the holder second": {
			holder:    func(disk, _ string) string { return disk },
			held:      func(_, srv string) string { return filepath.Join(srv, "app", "conf") },
			heldFirst: true,
		},
		"a holder through a link": {
			holder:    func(_, srv string) string { return srv },
			held:      func(disk, _ string) string { return filepath.Join(disk, "app", "conf") },
			heldFirst: true,
		},
		"a source that's a link itself, written through another": {
			holder:    func(disk, _ string) string { return disk },
			held:      func(_, srv string) string { return filepath.Join(srv, "app", "current") },
			heldFirst: true,
		},
		"a holder's volume, the holder first": {
			holder:       func(disk, _ string) string { return disk },
			held:         func(_, srv string) string { return filepath.Join(srv, "app", "conf") },
			holderVolume: true,
		},
		"a holder's volume, the holder second": {
			holder:       func(disk, _ string) string { return disk },
			held:         func(_, srv string) string { return filepath.Join(srv, "app", "conf") },
			holderVolume: true, heldFirst: true,
		},
		"a source's volume, the holder first": {
			holder:     func(disk, _ string) string { return disk },
			held:       func(_, srv string) string { return filepath.Join(srv, "app", "conf") },
			heldVolume: true,
		},
		"a source's volume, the holder second": {
			holder:     func(disk, _ string) string { return disk },
			held:       func(_, srv string) string { return filepath.Join(srv, "app", "conf") },
			heldVolume: true, heldFirst: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			disk, srv := filepath.Join(dir, "disk"), filepath.Join(dir, "srv")
			if err := os.MkdirAll(filepath.Join(disk, "app", "conf"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(disk, srv); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(disk, "app", "current")); err != nil {
				t.Fatal(err)
			}
			e, db, _ := followingPaths(t, filepath.Join(dir, "dockwell"))
			env := production(t, db)
			// serve is a service of that name mounting p, and through a bind volume of its
			// own, made with it, where viaVolume.
			serve := func(name, p string, viaVolume bool, command ...string) []Edit {
				mount := &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/m"}
				var eds []Edit
				if viaVolume {
					vol := resource.Ref{Kind: volume.Kind, Scope: env, Name: name + "-files"}
					eds = append(eds, Edit{Ref: vol, Spec: &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": p}}, Servers: []string{"srv_A"}})
					mount = &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: vol.Name}, Target: "/m"}
				}
				return append(eds, Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
					Image: "nginx:1.27", Command: command, Mounts: []*resourcev1.Mount{mount},
				}}}})
			}
			holder, held := serve("holder", tt.holder(disk, srv), tt.holderVolume), serve("held", tt.held(disk, srv), tt.heldVolume)
			first, then := holder, held
			if tt.heldFirst {
				first, then = held, holder
			}
			apply(t, e, propose(t, e, "First", first...))
			layout := propose(t, e, "Then", then...)
			if !layout.AsGoodAsRoot() {
				t.Fatalf("the layout reads as ordinary: %+v", layout.Resources)
			}
			apply(t, e, layout)
			code := serve("holder", tt.holder(disk, srv), tt.holderVolume, "sh")
			newCode := propose(t, e, "New code", code[len(code)-1])
			if !newCode.AsGoodAsRoot() {
				t.Errorf("new code in the holder asks nothing: %+v", newCode.Resources)
			}
		})
	}
}

// A link on the way to a source moves it wherever the link is rewritten to, so what
// can write the folder that link is in holds the source, even where the source ends up
// outside that folder. Here /srv leads to /disk, the holder writes /disk/app, and
// /disk/app/y leads to /b, so a source written /srv/app/y/conf ends up at /b/conf,
// by way of a link the holder can rewrite. The narrow check's case, in both orders,
// with the held app out of the plan for the holder's new code.
func TestAHoldOfALinkOnTheWayToASourceIsCounted(t *testing.T) {
	for name, heldFirst := range map[string]bool{"the holder first": false, "the held first": true} {
		t.Run(name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			disk, srv, b := filepath.Join(dir, "disk"), filepath.Join(dir, "srv"), filepath.Join(dir, "b")
			for _, d := range []string{filepath.Join(disk, "app"), filepath.Join(b, "conf")} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(disk, srv); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(b, filepath.Join(disk, "app", "y")); err != nil {
				t.Fatal(err)
			}
			e, db, _ := followingPaths(t, filepath.Join(dir, "dockwell"))
			env := production(t, db)
			serve := func(name, p string, command ...string) Edit {
				return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
					Image: "nginx:1.27", Command: command, Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/m"}},
				}}}}
			}
			holder, held := serve("holder", filepath.Join(disk, "app")), serve("held", filepath.Join(srv, "app", "y", "conf"))
			first, then := holder, held
			if heldFirst {
				first, then = held, holder
			}
			apply(t, e, propose(t, e, "First", first))
			layout := propose(t, e, "Then", then)
			if !layout.AsGoodAsRoot() {
				t.Fatalf("the layout reads as ordinary: %+v", layout.Resources)
			}
			apply(t, e, layout)
			if newCode := propose(t, e, "New code", serve("holder", filepath.Join(disk, "app"), "sh")); !newCode.AsGoodAsRoot() {
				t.Errorf("new code in the holder asks nothing: %+v", newCode.Resources)
			}
		})
	}
}

// A link on the way to what a container someone ran by hand mounts counts as one on
// the way to what Dockwell made: the agent walks every path on its server.
func TestALinkOnTheWayToWhatsRunByHandCounts(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk, srv, b := filepath.Join(dir, "disk"), filepath.Join(dir, "srv"), filepath.Join(dir, "b")
	for _, d := range []string{filepath.Join(disk, "app"), filepath.Join(b, "conf")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(disk, srv); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(b, filepath.Join(disk, "app", "y")); err != nil {
		t.Fatal(err)
	}
	e, db, engine := followingPaths(t, filepath.Join(dir, "dockwell"))
	env := production(t, db)
	engine.AddLocalImage("alpine:3", dockertest.Image{Config: docker.Config{Cmd: []string{"sh"}}})
	c := engine.Client()
	source := filepath.Join(srv, "app", "y", "conf")
	id, err := c.CreateContainer(t.Context(), "by-hand", &docker.CreateRequest{Config: docker.Config{Image: "alpine:3"}, HostConfig: docker.HostConfig{Binds: []string{source + ":/conf"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	plan := propose(t, e, "Files", Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "files"}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: filepath.Join(disk, "app")}, Target: "/app"}},
	}}}})
	want := resource.Access{Kind: resource.AccessPath, Path: filepath.Join(disk, "app"), Holds: source, HoldsFor: "by-hand", SameAsRoot: true}
	if !plan.AsGoodAsRoot() || !slices.Contains(plan.Resources[0].HostAccess, want) {
		t.Errorf("it says %+v, want %+v", plan.Resources[0].HostAccess, want)
	}
}

// What Dockwell didn't make can be what's held too: a container someone ran by hand
// follows its source when Docker restarts it, as any does. It's named on the plan, by
// its name in Docker, beside what Dockwell made, which isn't named again. It never
// counts as what holds, as its code isn't what a plan here chooses, and what mounts
// what's as good as root already isn't held.
func TestWhatDockwellDidntMakeCanBeHeld(t *testing.T) {
	for name, tt := range map[string]struct {
		// byHand is what the container someone ran mounts, and plan what the plan
		// mounts, read-write, each under the test's folder unless absolute.
		byHand []string
		plan   string
		// holds is how many holds the plan names: one for what was run by hand, and
		// one for a service of Dockwell's beside it, mounting srv/app/keys.
		holds int
	}{
		"a folder inside":              {byHand: []string{"srv/app/conf"}, plan: "srv", holds: 2},
		"a folder beside":              {byHand: []string{"srv-other"}, plan: "srv", holds: 1},
		"a folder inside, beside root": {byHand: []string{"srv/app/conf", "/var/run/docker.sock"}, plan: "srv", holds: 1},
		"what it writes around":        {byHand: []string{"srv"}, plan: "srv/app/data"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			at := func(p string) string {
				if filepath.IsAbs(p) {
					return p
				}
				return filepath.Join(dir, p)
			}
			var binds []string
			for _, b := range tt.byHand {
				binds = append(binds, at(b)+":/m"+strconv.Itoa(len(binds)))
			}
			e, db, engine := followingPaths(t, filepath.Join(dir, "dockwell"))
			env := production(t, db)
			serve := func(name, p string) Edit {
				return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
					Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: at(p)}, Target: "/m"}},
				}}}}
			}
			if tt.holds > 0 {
				apply(t, e, propose(t, e, "API", serve("api", "srv/app/keys")))
			}
			engine.AddLocalImage("alpine:3", dockertest.Image{Config: docker.Config{Cmd: []string{"sh"}}})
			c := engine.Client()
			id, err := c.CreateContainer(t.Context(), "by-hand", &docker.CreateRequest{Config: docker.Config{Image: "alpine:3"}, HostConfig: docker.HostConfig{Binds: binds}})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.StartContainer(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			plan := propose(t, e, "Files", serve("files", tt.plan))
			got := plan.Resources[0].HostAccess
			holds := 0
			for _, a := range got {
				if a.Holds != "" {
					holds++
				}
			}
			if holds != tt.holds {
				t.Errorf("it names %d holds, want %d: %+v", holds, tt.holds, got)
			}
			if tt.holds == 2 {
				want := resource.Access{Kind: resource.AccessPath, Path: at("srv"), Holds: at("srv/app/conf"), HoldsFor: "by-hand", SameAsRoot: true}
				if !slices.Contains(got, want) {
					t.Errorf("it says %+v, want %+v", got, want)
				}
			}
			if tt.holds == 0 && plan.AsGoodAsRoot() {
				t.Errorf("what was run by hand counts as what holds: %+v", got)
			}
		})
	}
}

// A plan made while its server is offline is made from what the control plane last
// knew, with nothing of where paths lead, so it can't see a hold through a link. It
// can't apply: not while the server is away, and not once it's back, as the plan made
// then sees the hold, and isn't the plan that was reviewed.
func TestAPlanMadeOfflineCantApply(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk, srv := filepath.Join(dir, "disk"), filepath.Join(dir, "srv")
	if err := os.MkdirAll(filepath.Join(disk, "app", "conf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(disk, srv); err != nil {
		t.Fatal(err)
	}
	e, db, _ := followingPaths(t, filepath.Join(dir, "dockwell"))
	env := production(t, db)
	serve := func(name, p string) Edit {
		return Edit{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: name}, Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: "nginx:1.27", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: p}, Target: "/m"}},
		}}}}
	}
	apply(t, e, propose(t, e, "Held", serve("held", filepath.Join(srv, "app", "conf"))))

	agents := e.agents.(onDocker) //nolint:forcetypeassert // followingPaths'
	away := agents["srv_A"]
	delete(agents, "srv_A")
	offline := propose(t, e, "Holder", serve("holder", disk))
	if len(offline.Servers) != 1 || !offline.Servers[0].Offline {
		t.Fatalf("servers: %+v", offline.Servers)
	}
	if err := e.Apply(t.Context(), offline.Change); !errors.Is(err, ErrOffline) {
		t.Errorf("applying it with the server away = %v, want ErrOffline", err)
	}
	agents["srv_A"] = away
	if err := e.Apply(t.Context(), offline.Change); !errors.Is(err, ErrPlanChanged) {
		t.Errorf("applying it once the server is back = %v, want ErrPlanChanged", err)
	}
	again := propose(t, e, "Holder", serve("holder", disk))
	if !again.AsGoodAsRoot() {
		t.Errorf("planned again, the hold reads as ordinary: %+v", again.Resources)
	}
}
