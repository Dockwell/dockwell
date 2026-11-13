// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/resource/service"
)

func svc(c *resourcev1.Container) *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}
}

func port(host, inside uint32) *resourcev1.PortMapping {
	return &resourcev1.PortMapping{HostPort: host, ContainerPort: inside}
}

func volumeAt(name, target string) *resourcev1.Mount {
	return &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: name}, Target: target}
}

// Each thing an upgrade can meet: what the old file gave, what runs, what the new file
// gives, and what comes of it.
func TestAnUpgradeMergesEachField(t *testing.T) {
	base := func() *resourcev1.Container {
		return &resourcev1.Container{
			Image: "app:1@sha256:a", Ports: []*resourcev1.PortMapping{port(8081, 8080)},
			Mounts: []*resourcev1.Mount{volumeAt("data", "/data")}, Env: map[string]string{"MODE": "web"},
			Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
		}
	}
	with := func(edit func(*resourcev1.Container)) *resourcev1.Container {
		c := base()
		edit(c)
		return c
	}
	newImage := func(c *resourcev1.Container) { c.Image = "app:2@sha256:b" }
	for name, tc := range map[string]struct {
		ours, next func(*resourcev1.Container)
		want       func(*resourcev1.Container)
		clashes    []clash
	}{
		"nothing a person did, and the image moves": {
			ours: func(*resourcev1.Container) {}, next: newImage, want: newImage,
		},
		"limiting a port to loopback is the same port, and stays": {
			ours: func(c *resourcev1.Container) { c.Ports[0].HostIp = "127.0.0.1" }, next: newImage,
			want: func(c *resourcev1.Container) { newImage(c); c.Ports[0].HostIp = "127.0.0.1" },
		},
		"a port a person removed stays removed": {
			ours: func(c *resourcev1.Container) { c.Ports = nil }, next: newImage,
			want: func(c *resourcev1.Container) { newImage(c); c.Ports = nil },
		},
		"a port a person added stays, on one the file has too": {
			ours: func(c *resourcev1.Container) { c.Ports = append(c.Ports, port(8082, 8080), port(5432, 5432)) }, next: newImage,
			want: func(c *resourcev1.Container) {
				newImage(c)
				c.Ports = append(c.Ports, port(8082, 8080), port(5432, 5432))
			},
		},
		"a tmpfs a person added stays beside one the release adds": {
			ours: func(c *resourcev1.Container) { c.Tmpfs = []*resourcev1.Tmpfs{{Target: "/mine", Size: "1M"}} },
			next: func(c *resourcev1.Container) { c.Tmpfs = []*resourcev1.Tmpfs{{Target: "/theirs", Size: "2M"}} },
			want: func(c *resourcev1.Container) {
				c.Tmpfs = []*resourcev1.Tmpfs{{Target: "/theirs", Size: "2M"}, {Target: "/mine", Size: "1M"}}
			},
		},
		"more shared memory from a release reaches what a person left": {
			ours: func(*resourcev1.Container) {}, next: func(c *resourcev1.Container) { c.ShmSize = "1G" },
			want: func(c *resourcev1.Container) { c.ShmSize = "1G" },
		},
		"a port the release closes goes": {
			ours: func(*resourcev1.Container) {}, next: func(c *resourcev1.Container) { c.Ports = nil },
			want: func(c *resourcev1.Container) { c.Ports = nil },
		},
		"a port the release adds comes": {
			ours: func(*resourcev1.Container) {}, next: func(c *resourcev1.Container) { c.Ports = append(c.Ports, port(9090, 9090)) },
			want: func(c *resourcev1.Container) { c.Ports = append(c.Ports, port(9090, 9090)) },
		},
		"a port a person changed goes with the release that drops it, and says so": {
			ours: func(c *resourcev1.Container) { c.Ports[0].HostIp = "127.0.0.1" },
			next: func(c *resourcev1.Container) { c.Ports = nil },
			want: func(c *resourcev1.Container) { c.Ports = nil }, clashes: []clash{{what: "port 8080/tcp", gone: true}},
		},
		"data a person moved stays where they moved it": {
			ours: func(c *resourcev1.Container) {
				c.Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/pg"}, Target: "/data"}}
			},
			next: newImage,
			want: func(c *resourcev1.Container) {
				newImage(c)
				c.Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/pg"}, Target: "/data"}}
			},
		},
		"what a person tuned stays": {
			ours: func(c *resourcev1.Container) {
				c.Resources = &resourcev1.Resources{Memory: "512M"}
				c.Command = []string{"serve", "--tuned"}
				c.User = "1000"
				c.ExternalNetworks = []string{"shared"}
				c.Labels = map[string]string{"team": "a"}
				c.Restart = resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS
				c.Healthcheck = &resourcev1.Healthcheck{Test: []string{"CMD", "true"}}
				c.Env["MODE"] = "worker"
				c.Env["MINE"] = "yes"
			},
			next: newImage,
			want: func(c *resourcev1.Container) {
				newImage(c)
				c.Resources = &resourcev1.Resources{Memory: "512M"}
				c.Command = []string{"serve", "--tuned"}
				c.User = "1000"
				c.ExternalNetworks = []string{"shared"}
				c.Labels = map[string]string{"team": "a"}
				c.Restart = resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS
				c.Healthcheck = &resourcev1.Healthcheck{Test: []string{"CMD", "true"}}
				c.Env["MODE"] = "worker"
				c.Env["MINE"] = "yes"
			},
		},
		"a variable the release stops setting goes": {
			ours: func(*resourcev1.Container) {}, next: func(c *resourcev1.Container) { delete(c.Env, "MODE") },
			want: func(c *resourcev1.Container) { c.Env = nil },
		},
		"where both changed a field, a person's stays, and it says so": {
			ours: func(c *resourcev1.Container) { c.Command = []string{"mine"} },
			next: func(c *resourcev1.Container) { c.Command = []string{"theirs"} },
			want: func(c *resourcev1.Container) { c.Command = []string{"mine"} }, clashes: []clash{{what: "command"}},
		},
		"where both changed an image, the release's wins, as that's the upgrade": {
			ours: func(c *resourcev1.Container) { c.Image = "app:mine" }, next: newImage, want: newImage,
		},
		"where both changed a variable, a person's stays, by its name alone": {
			ours: func(c *resourcev1.Container) { c.Env["MODE"] = "mine" },
			next: func(c *resourcev1.Container) { c.Env["MODE"] = "theirs" },
			want: func(c *resourcev1.Container) { c.Env["MODE"] = "mine" }, clashes: []clash{{what: "env MODE"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, clashes := mergeService(svc(base()), svc(with(tc.ours)), svc(with(tc.next)))
			if want := svc(with(tc.want)); !proto.Equal(got, want) {
				t.Errorf("merged to %v, want %v", got, want)
			}
			if !slices.Equal(clashes, tc.clashes) {
				t.Errorf("clashes = %+v, want %+v", clashes, tc.clashes)
			}
		})
	}
}

// What a service uses merges the same way, and its revisions are the store's.
func TestAnUpgradeMergesWhatAServiceUses(t *testing.T) {
	base := svc(&resourcev1.Container{Image: "a"})
	base.Secrets = map[string]string{"PASSWORD": "PASSWORD"}
	ours := proto.CloneOf(base)
	ours.Secrets["MINE"] = "MINE"
	ours.Revisions = map[string]int64{"PASSWORD": 1}
	got, clashes := mergeService(base, ours, proto.CloneOf(base))
	if len(clashes) != 0 || len(got.GetSecrets()) != 2 || got.GetSecrets()["MINE"] != "MINE" || got.GetRevisions() != nil {
		t.Errorf("merged to %v, clashes %+v", got, clashes)
	}
}

func item(name string) compose.Item[*resourcev1.Service] {
	return compose.Item[*resourcev1.Service]{Name: name, Spec: svc(&resourcev1.Container{Image: name, Networks: []string{"default"}})}
}

func stack(names ...string) *compose.Stack {
	s := &compose.Stack{}
	for _, n := range names {
		s.Services = append(s.Services, item(n))
	}
	return s
}

func running(names ...string) runs {
	r := runs{services: map[string]*resourcev1.Service{}, volumes: map[string]bool{}, networks: map[string]bool{}}
	for _, n := range names {
		r.services[n] = item(n).Spec
	}
	return r
}

// An upgrade deletes only what the old file named and the new one doesn't, and what a
// person took away stays away.
func TestAnUpgradeTouchesOnlyWhatItsFileNamed(t *testing.T) {
	old, next := stack("web", "gone", "deleted"), stack("web", "deleted", "added")
	deletes, _, err := mergeStack("env", old, running("web", "gone", "mine"), next)
	if err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 1 || deletes[0].Kind != service.Kind || deletes[0].Name != "gone" {
		t.Errorf("deletes %+v, want gone alone", deletes)
	}
	var names []string
	for _, s := range next.Services {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"web", "added"}) {
		t.Errorf("it runs %v: a service a person deleted is back, or one it adds isn't there", names)
	}
}

// A release that adds a name a person's app has can't take it, and can't leave it.
func TestAnUpgradeRefusesANameAPersonHas(t *testing.T) {
	if _, _, err := mergeStack("env", stack("web"), running("web", "cache"), stack("web", "cache")); err == nil {
		t.Error("a release took the name of a person's service")
	}
}

// A network a release drops goes, unless something that stays is on it.
func TestAnUpgradeKeepsANetworkInUse(t *testing.T) {
	old := stack("web")
	old.Networks = []compose.Item[*resourcev1.Network]{{Name: "default", Spec: &resourcev1.Network{}}, {Name: "back", Spec: &resourcev1.Network{}}}
	now := running("web", "mine")
	now.networks["default"], now.networks["back"] = true, true
	deletes, _, err := mergeStack("env", old, now, stack("web"))
	if err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 1 || deletes[0].Name != "back" {
		t.Errorf("deletes %+v, want back alone, as mine and web are on default", deletes)
	}
}

// pgOn is PostgreSQL of image with its data in volume at target.
func pgOn(image, volume, target string) *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: image, Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: volume}, Target: target}},
	}}}
}

// H3: an upgrade of an install whose database a move took to a new volume keeps it
// there, as a person's change: the file still names the old volume, which holds
// what was there before the move. PostgreSQL 18 keeps it a level up, at another
// target, and that holds too. The image follows the release, as an upgrade's does:
// one that's still on 16 is then refused, as 16 can't read 17's data.
func TestAnUpgradeKeepsAMovedDatabaseOnItsNewVolume(t *testing.T) {
	for _, c := range []struct {
		ours   *resourcev1.Service
		volume string
		target string
	}{
		{pgOn("postgres:17.2", "pgdata-17", "/var/lib/postgresql/data"), "pgdata-17", "/var/lib/postgresql/data"},
		{pgOn("postgres:18.1", "pgdata-18", "/var/lib/postgresql"), "pgdata-18", "/var/lib/postgresql"},
	} {
		base := pgOn("postgres:16.4", "pgdata", "/var/lib/postgresql/data")
		next := pgOn("postgres:16.6", "pgdata", "/var/lib/postgresql/data")
		merged, _ := mergeService(base, c.ours, next)
		mounts := merged.GetApp().GetMounts()
		if len(mounts) != 1 || mounts[0].GetVolume() != c.volume || mounts[0].GetTarget() != c.target {
			t.Errorf("with %s, the upgrade mounts %v", c.volume, mounts)
		}
		if merged.GetApp().GetImage() != next.GetApp().GetImage() {
			t.Errorf("with %s, the upgrade runs %s", c.volume, merged.GetApp().GetImage())
		}
	}
}
