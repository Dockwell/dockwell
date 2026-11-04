// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"log/slog"
	"slices"
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
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// onDocker are agents that run the Docker drivers on a fake engine each,
// so what an agent refuses as it applies, such as a service whose volume
// isn't there yet, fails the change.
type onDocker map[string]resource.Agent

func (d onDocker) Agent(id string) (resource.Agent, bool) {
	a, ok := d[id]
	return a, ok
}

func setupDocker(t *testing.T, b store.Backend, ids ...string) (*Engine, map[string]*dockertest.Engine, *store.DB) {
	t.Helper()
	db, _ := storetest.Open(t, b)
	if _, err := Setup(t.Context(), db, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	agents, engines := onDocker{}, map[string]*dockertest.Engine{}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		for _, id := range ids {
			if err := tx.CreateServer(t.Context(), store.Server{ID: id, Name: id, CreatedAt: now, CertSerial: id, CertNotAfter: now}); err != nil {
				return err
			}
			e := dockertest.New(t)
			e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
			client, kept := e.Client(), &volume.Kept{}
			containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Deleted: kept}}
			agents[id] = resource.Local{Drivers: resource.NewRegistry(&containers, &service.Driver{Containers: containers},
				&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: kept}}, &network.Driver{Client: client})}
			engines[id] = e
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&container.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{})
	return New(db, kinds, agents, slog.New(slog.DiscardHandler)), engines, db
}

// uses is a service named web that mounts the volume data and joins the
// network back.
func uses(env string, servers ...string) Edit {
	return Edit{
		Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "web"},
		Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: "nginx:1.27", Networks: []string{"back"},
			Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}},
		}}},
		Servers: servers,
	}
}

// data is the volume data, and back the network back.
func data(env string, servers ...string) Edit {
	return Edit{Ref: resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}, Spec: &resourcev1.Volume{}, Servers: servers}
}

func back(env string, servers ...string) Edit {
	return Edit{Ref: resource.Ref{Kind: network.Kind, Scope: env, Name: "back"}, Spec: &resourcev1.Network{}, Servers: servers}
}

// stepKinds lists the kinds a plan's steps act on, in the order they run
// on a server, with their actions.
func stepKinds(p *Plan) []string {
	var out []string
	steps := p.Steps()
	for i := range steps {
		out = append(out, steps[i].Action.String()+" "+string(steps[i].Ref.Kind))
	}
	return out
}

// What a service names must be in its environment, on its servers, once
// a change applies: the change is refused when it's proposed, not when an
// agent finds out. In a change, what is named is made first and deleted
// last, whatever order the change lists it in.
func TestReferencesAreCheckedWhenProposed(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, engines, db := setupDocker(t, b, "srv_A")
		env := production(t, db)

		refuseProposal(t, e, "a service whose volume isn't there", ErrInvalid,
			"service web mounts volume data, which isn't in its environment. Add data to it, in this change or an earlier one",
			uses(env), back(env))
		refuseProposal(t, e, "a service whose network isn't there", ErrInvalid,
			"service web joins network back, which isn't in its environment",
			uses(env), data(env))

		// Listed before what it names, the service still runs after it.
		p := propose(t, e, "Run web", uses(env), data(env), back(env))
		if got, want := stepKinds(p), []string{"create volume", "create network", "create service"}; !slices.Equal(got, want) {
			t.Errorf("steps %v, want %v", got, want)
		}
		apply(t, e, p)
		if c := engines["srv_A"].Container(service.ContainerName(uses(env).Ref)); c == nil || !c.State.Running {
			t.Fatalf("web isn't running: %+v", c)
		}

		refuseProposal(t, e, "deleting a volume in use", ErrInvalid,
			"volume data can't go while service web mounts it. Change web so it doesn't, or delete it, first, in this change or an earlier one",
			del(volume.Kind, env, "data"))
		refuseProposal(t, e, "deleting a network in use", ErrInvalid,
			"network back can't go while service web joins it",
			del(network.Kind, env, "back"))
		upgrade := uses(env)
		upgrade.Spec.(*resourcev1.Service).GetApp().Image = "nginx:1.28"
		refuseProposal(t, e, "upgrading a service while deleting its volume", ErrInvalid,
			"this change deletes volume data, but service web mounts it. Keep data, or change web so it doesn't mount it",
			upgrade, del(volume.Kind, env, "data"))

		// Deleted together, the service goes before what it names,
		// whatever order the change lists them in; undoing that brings
		// them back in order.
		gone := propose(t, e, "Retire web", del(volume.Kind, env, "data"), del(network.Kind, env, "back"), del(service.Kind, env, "web"))
		if got, want := stepKinds(gone), []string{"delete service", "delete network", "delete volume"}; !slices.Equal(got, want) {
			t.Errorf("steps %v, want %v", got, want)
		}
		apply(t, e, gone)
		again := undo(t, e, gone)
		if got := stepKinds(again); got[len(got)-1] != "create service" {
			t.Errorf("undoing the retirement runs %v, want the service made last", got)
		}
		apply(t, e, again)
	})
}

// A service can't run on a server its volume or network isn't placed on.
func TestReferencesMustBeOnTheServersTooWhenProposed(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		e, _, db := setupDocker(t, b, "srv_A", "srv_B")
		env := production(t, db)
		apply(t, e, propose(t, e, "Add data and back", data(env, "srv_A"), back(env, "srv_A", "srv_B")))
		refuseProposal(t, e, "a service where its volume isn't", ErrInvalid,
			"service web runs on srv_B, but volume data isn't placed there, so web can't mount it. Place data on srv_B too, or web where data is",
			uses(env, "srv_B"))
		apply(t, e, propose(t, e, "Run web", uses(env, "srv_A")))
		refuseProposal(t, e, "moving a volume away from its service", ErrInvalid,
			"service web runs on srv_A, but volume data isn't placed there",
			data(env, "srv_B"))
	})
}
