// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"log/slog"
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

		next := New(db, controlKinds(), onDocker{"srv_A": plain}, slog.New(slog.DiscardHandler))
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
		next := New(db, controlKinds(), onDocker{"srv_A": plain}, slog.New(slog.DiscardHandler))
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
