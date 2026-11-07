// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

const secretValue = "hunter2-hunter2"

// runByHand runs a container on the engine as someone at the server would: on a
// network of their own, with a port, a volume and a value in its environment.
func runByHand(t *testing.T, e *dockertest.Engine, name string, edit func(*docker.CreateRequest)) string {
	t.Helper()
	c := e.Client()
	e.AddLocalImage("nginx:1.26", dockertest.Image{Volumes: []string{"/cache"}, Config: docker.Config{Cmd: []string{"nginx"}, Env: []string{"PATH=/bin"}}})
	e.AddLocalImage("nginx:1.27", dockertest.Image{Volumes: []string{"/cache"}, Config: docker.Config{Cmd: []string{"nginx"}, Env: []string{"PATH=/bin"}}})
	req := &docker.CreateRequest{
		Config: docker.Config{Image: "nginx:1.26", Env: []string{"PATH=/bin", "DB_PASSWORD=" + secretValue}},
		HostConfig: docker.HostConfig{
			Binds:         []string{"appdata:/data"},
			PortBindings:  map[string][]docker.PortBinding{"80/tcp": {{HostPort: "8088"}}},
			RestartPolicy: docker.RestartPolicy{Name: "always"},
		},
	}
	if edit != nil {
		edit(req)
	}
	id, err := c.CreateContainer(t.Context(), name, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func adopt(t *testing.T, c dockwellv1connect.ContainerServiceClient, id, as string) *dockwellv1.Change {
	t.Helper()
	res, err := c.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: id, Service: as}))
	if err != nil {
		t.Fatalf("AdoptContainer: %v", err)
	}
	return res.Msg.GetChange()
}

// mutations are what was asked of Docker since the first seen requests that isn't a
// read, as a restart, a stop, a rename or a remove would be.
func mutations(e *dockertest.Engine, from int) []string {
	return slices.DeleteFunc(slices.Clone(e.Seen()[from:]), func(r string) bool { return strings.HasPrefix(r, "GET ") })
}

// The point of the issue: someone takes on what they already run, with nothing about
// it moving, and from then on changing its image is an ordinary plan that keeps its
// volumes and its environment, and can be undone.
func TestTakingOnAContainerThenChangingItsImage(t *testing.T) { storetest.Each(t, testTakingOn) }

func testTakingOn(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := containerClient(b, b.origin)
	svc := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	lc := logClient(b, b.origin)

	e.AddNetwork(docker.Network{ID: "appnet", Name: "appnet", Driver: "bridge"})
	id := runByHand(t, e, "legacy_api", func(r *docker.CreateRequest) {
		r.HostConfig.NetworkMode = "appnet"
		r.NetworkingConfig.EndpointsConfig = map[string]*docker.EndpointSettings{"appnet": {}}
	})
	before := e.Container("legacy_api")
	e.Print("legacy_api", docker.Stdout, "said before it was taken on")

	seen := len(e.Seen())
	planned := adopt(t, cc, "legacy_api", "api")
	if s := planned.String(); strings.Contains(s, secretValue) {
		t.Fatalf("the plan shows a value of the environment: %s", s)
	}
	if !planned.GetPlan().GetHostAccess() {
		t.Errorf("taking on a container brings in its environment, so a person approves it: %v", planned.GetPlan())
	}
	steps := planned.GetPlan().GetServers()[0].GetSteps()
	if len(steps) != 1 || steps[0].GetAdopts() != id || steps[0].GetDowntime() != nil {
		t.Fatalf("steps = %v, want one that takes on %s with no downtime", steps, id)
	}
	applyConfirmed(t, c, planned)
	if got := mutations(e, seen); len(got) != 0 {
		t.Errorf("taking it on asked Docker to %v", got)
	}
	after := e.Container("legacy_api")
	if after == nil || after.ID != id || after.State.StartedAt != before.State.StartedAt || after.RestartCount != before.RestartCount {
		t.Fatalf("it should run as it did: %+v, then %+v", before.State, after)
	}

	// It is the service's now: no drift, its logs read as the service's, and the list
	// of what Dockwell didn't make doesn't have it.
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Errorf("drift after taking it on = %v, %v", drift.Msg.GetDrift(), err)
	}
	list, err := cc.ListContainers(t.Context(), connect.NewRequest(&dockwellv1.ListContainersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(list.Msg.GetContainers()); len(got) != 0 || list.Msg.GetOwnContainers() != 1 {
		t.Errorf("the list has %v and left out %d, want none and one", got, list.Msg.GetOwnContainers())
	}
	r := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "api", Tail: 100})
	r.waitLines("said before it was taken on")
	r.cancel()

	// Changing its image replaces it, with what it had.
	deployed, err := svc.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "api", Image: "nginx:1.27"}))
	if err != nil {
		t.Fatal(err)
	}
	second := apply(t, c, deployed.Msg.GetChange())
	if second.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the image change ended %v: %s", second.GetState(), second.GetError())
	}
	if s := second.String(); strings.Contains(s, secretValue) {
		t.Fatalf("the image change shows a value of the environment: %s", s)
	}
	if e.Container("legacy_api") != nil {
		t.Errorf("the container it replaced is still there: %v", e.Containers())
	}
	project := docker.Project(planned.GetPlan().GetResources()[0].GetResource().GetScope())
	now := e.Newest(project, "api")
	if now == nil || now.Config.Image != "nginx:1.27" {
		t.Fatalf("Docker runs %+v", now)
	}
	check := func(when string, ctr *docker.Container) {
		t.Helper()
		if !slices.Contains(ctr.Config.Env, "DB_PASSWORD="+secretValue) {
			t.Errorf("%s, its environment is %v", when, ctr.Config.Env)
		}
		var data, cache string
		for _, m := range ctr.Mounts {
			switch m.Destination {
			case "/data":
				data = m.Name
			case "/cache":
				cache = m.Name
			}
		}
		if data != "appdata" {
			t.Errorf("%s, /data is %q, want the volume it came with: %+v", when, data, ctr.Mounts)
		}
		for _, m := range before.Mounts {
			if m.Destination == "/cache" && m.Name != cache {
				t.Errorf("%s, the volume its image keeps at /cache is %q, want %q", when, cache, m.Name)
			}
		}
		if got := ctr.HostConfig.PortBindings["80/tcp"]; len(got) != 1 || got[0].HostPort != "8088" {
			t.Errorf("%s, its port is %+v", when, ctr.HostConfig.PortBindings)
		}
		if ctr.HostConfig.RestartPolicy.Name != "always" {
			t.Errorf("%s, its restart policy is %+v", when, ctr.HostConfig.RestartPolicy)
		}
		if ep := ctr.NetworkSettings.Networks["appnet"]; ep == nil || !slices.Contains(ep.Aliases, "legacy_api") {
			t.Errorf("%s, others can't reach it by the name it came with: %+v", when, ctr.NetworkSettings.Networks)
		}
	}
	check("after the image change", now)
	if e.Volume(project+"_appdata") != nil {
		t.Errorf("the change made a volume of its own in place of the one it came with")
	}

	// And it can be undone.
	undone, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: second.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	back := apply(t, c, undone.Msg.GetChange())
	if back.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %s", back.GetState(), back.GetError())
	}
	now = e.Newest(project, "api")
	if now == nil || now.Config.Image != "nginx:1.26" {
		t.Fatalf("after the undo Docker runs %+v", now)
	}
	check("after the undo", now)

	// The value ran, and is nowhere it could be read from at rest, in any encoding.
	for table, rows := range storetest.Dump(t, f.db) {
		for _, row := range rows {
			for _, enc := range encodings(secretValue) {
				if strings.Contains(row, enc) {
					t.Errorf("%s holds a value of the environment, as %q", table, enc)
				}
			}
		}
	}
}

// Undoing it, like deleting the service, hands the container back: Dockwell never
// made it, so nothing stops, and it is on the list again.
func TestLettingAContainerGo(t *testing.T) { storetest.Each(t, testLettingGo) }

func testLettingGo(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := containerClient(b, b.origin)

	id := runByHand(t, e, "by-hand", nil)
	before := e.Container("by-hand")
	planned := adopt(t, cc, "by-hand", "api")
	applyConfirmed(t, c, planned)

	seen := len(e.Seen())
	undone, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: planned.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	steps := undone.Msg.GetChange().GetPlan().GetServers()[0].GetSteps()
	if len(steps) != 1 || !steps[0].GetReleases() || steps[0].GetDestroysData() || steps[0].GetDowntime() != nil {
		t.Fatalf("the undo's steps = %v, want one that lets it go, with nothing lost", steps)
	}
	back := apply(t, c, undone.Msg.GetChange())
	if back.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the undo ended %v: %s", back.GetState(), back.GetError())
	}
	if got := mutations(e, seen); len(got) != 0 {
		t.Errorf("letting it go asked Docker to %v", got)
	}
	after := e.Container("by-hand")
	if after == nil || after.ID != id || after.State.StartedAt != before.State.StartedAt {
		t.Fatalf("it should run as it did: %+v", after)
	}
	list, err := cc.ListContainers(t.Context(), connect.NewRequest(&dockwellv1.ListContainersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(list.Msg.GetContainers()); !slices.Equal(got, []string{"by-hand"}) {
		t.Errorf("the list has %v", got)
	}
}

// What a replace would drop without a word is refused by name, and what a plan would
// show is too, as a Compose file leaves a value in a command that named it. A person
// reads why, and nothing changed.
func TestWhatCantBeTakenOnSaysWhy(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)

	runByHand(t, e, "in-word", func(r *docker.CreateRequest) {
		r.Cmd = []string{"nginx", "--password=" + secretValue}
	})
	runByHand(t, e, "own-rootfs", func(r *docker.CreateRequest) {
		r.HostConfig.ReadonlyRootfs = true
		r.HostConfig.PortBindings = nil
		r.HostConfig.Binds = nil
	})
	// A name that's taken would move the service that has it onto this server.
	apply(t, changeClient(b, b.origin), propose(t, changeClient(b, b.origin), "Run web", runImage("ghcr.io/acme/shop:1.4")))
	runByHand(t, e, "alongside", func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	if _, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "alongside", Service: "web"})); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("taking on as a name that's taken: %v", err)
	}

	// One thing is it and that, and more are them and those.
	runByHand(t, e, "two-things", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
		r.HostConfig.ReadonlyRootfs = true
	})
	e.Reports("two-things", "HostConfig", map[string]any{"CpuQuota": 50000})
	_, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "two-things"}))
	if err == nil || !strings.Contains(err.Error(), "would drop them without a word") || !strings.Contains(err.Error(), "without those") {
		t.Errorf("taking on what has two things: %v", err)
	}
	_, err = cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "own-rootfs"}))
	if err == nil || !strings.Contains(err.Error(), "without that, then take it on") {
		t.Errorf("taking on what has one thing: %v", err)
	}

	for name, says := range map[string]string{
		"in-word":    "so a plan would show it to anyone",
		"own-rootfs": "would drop it without a word",
	} {
		_, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: name}))
		if err == nil || !strings.Contains(err.Error(), says) || !strings.Contains(err.Error(), "It stays as it is") {
			t.Errorf("taking on %s: %v, want it to say %q", name, err, says)
		}
		if err != nil && strings.Contains(err.Error(), secretValue) {
			t.Errorf("the refusal shows a value: %v", err)
		}
	}
}

// What a container runs with that no spec holds is what a change of image would
// drop, so it's refused by name, and the words say what to do. Each of these is what
// Docker reports for a flag a person can pass, and none is among what a spec holds. A
// setting Docker adds later is refused too, as nothing here can know what it does.
func TestWhatNoSpecHoldsIsRefusedByName(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)
	for name, c := range map[string]struct {
		section string
		keys    map[string]any
		says    string
	}{
		"with-gpus":     {"HostConfig", map[string]any{"DeviceRequests": []any{map[string]any{"Driver": "nvidia", "Count": -1}}}, "gpus"},
		"with-logging":  {"HostConfig", map[string]any{"LogConfig": map[string]any{"Type": "fluentd"}}, "logging: fluentd"},
		"with-publish":  {"HostConfig", map[string]any{"PublishAllPorts": true}, "publish_all_ports"},
		"with-tmpfs":    {"HostConfig", map[string]any{"Tmpfs": map[string]any{"/cache": "size=1048576,mode=1777"}}, "tmpfs option mode on /cache"},
		"with-rslave":   {"HostConfig", map[string]any{"Binds": []any{"/srv/data:/data:rslave"}}, "rslave on /data"},
		"with-quota":    {"HostConfig", map[string]any{"CpuQuota": 50000}, "cpu_quota"},
		"with-swap":     {"HostConfig", map[string]any{"Memory": 1 << 30, "MemorySwap": 1 << 30}, "memswap_limit"},
		"with-oom":      {"HostConfig", map[string]any{"OomKillDisable": true}, "oom_kill_disable"},
		"with-userns":   {"HostConfig", map[string]any{"UsernsMode": "host"}, "userns_mode"},
		"with-new":      {"HostConfig", map[string]any{"SomethingNew": "on"}, "HostConfig.SomethingNew"},
		"with-mac":      {"Config", map[string]any{"MacAddress": "02:42:ac:11:00:99"}, "mac_address"},
		"with-new-spec": {"Config", map[string]any{"AlsoNew": 1}, "Config.AlsoNew"},
	} {
		runByHand(t, e, name, func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
		e.Reports(name, c.section, c.keys)
		_, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: name}))
		if err == nil || !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), "It stays as it is") {
			t.Errorf("taking on %s: %v, want it to say %q", name, err, c.says)
		}
	}

	// A fixed address is on the network, not in either.
	runByHand(t, e, "with-ip", func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	e.Reports("with-ip", "NetworkSettings", map[string]any{"Networks": map[string]any{
		"bridge": map[string]any{"IPAMConfig": map[string]any{"IPv4Address": "172.17.0.99"}},
	}})
	if _, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "with-ip"})); err == nil ||
		!strings.Contains(err.Error(), "ipv4_address on bridge") {
		t.Errorf("taking on a fixed address: %v", err)
	}

	// What Docker fills in for anyone isn't a reason, nor what a spec holds.
	runByHand(t, e, "plain", func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil })
	e.Reports("plain", "HostConfig", map[string]any{
		"ConsoleSize": []any{0, 0}, "CgroupnsMode": "private", "IpcMode": "private", "ShmSize": 64 << 20, "Runtime": "runc",
		"MaskedPaths": []any{"/proc/kcore"}, "ReadonlyPaths": []any{"/proc/bus"}, "OomKillDisable": false, "Memory": 1 << 30, "MemorySwap": 2 << 30,
	})
	adopt(t, cc, "plain", "plain")
}

// A container can run with what's fine to Docker and no spec can say. That's refused by
// what it is, in words a person can act on, and isn't left to fail validation later.
func TestWhatASpecCantSayIsSaidBeforeItsPlanned(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)
	plain := func(edit func(*docker.CreateRequest)) func(*docker.CreateRequest) {
		return func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
			edit(r)
		}
	}
	zero := 0
	for name, c := range map[string]struct {
		edit func(*docker.CreateRequest)
		says string
	}{
		"picks-a-port": {func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings = map[string][]docker.PortBinding{"80/tcp": {{HostPort: ""}}}
		}, "a server port Docker picks for 80/tcp"},
		"a-port-range": {func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings = map[string][]docker.PortBinding{"80/tcp": {{HostPort: "8000-8010"}}}
		}, "a server port Docker picks for 80/tcp"},
		"sctp": {func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings = map[string][]docker.PortBinding{"5000/sctp": {{HostPort: "5000"}}}
		}, "the sctp port 5000/sctp"},
		"stops-at-once": {func(r *docker.CreateRequest) { r.StopTimeout = &zero }, "isn't a time for a container to stop in"},
	} {
		runByHand(t, e, name, plain(c.edit))
		_, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: name}))
		if err == nil || !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), "It stays as it is") {
			t.Errorf("taking on %s: %v, want it to say %q", name, err, c.says)
		}
	}

	// What's written as Docker's CLI writes it isn't a change, then or as it applies.
	c := changeClient(b, b.origin)
	runByHand(t, e, "by-cli", plain(func(r *docker.CreateRequest) {
		r.HostConfig.CapAdd, r.HostConfig.CapDrop = []string{"CAP_NET_BIND_SERVICE"}, []string{"cap_mknod"}
		r.HostConfig.Binds = []string{"/srv/by-cli/:/data/"}
	}))
	applyConfirmed(t, c, adopt(t, cc, "by-cli", "by-cli"))
}

// What a spec holds comes along, in either way Docker says a tmpfs, and a replace
// gives it back: a change of image doesn't lose /dev/shm or what's kept in memory.
func TestShmSizeAndTmpfsComeAlong(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)
	c := changeClient(b, b.origin)
	services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	runByHand(t, e, "headless", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
		r.HostConfig.ShmSize = 1 << 30
		r.HostConfig.Tmpfs = map[string]string{"/scratch": "size=1048576"}
	})
	planned := adopt(t, cc, "headless", "headless")
	if text := protojson.Format(planned); !strings.Contains(text, "shm_size") {
		t.Errorf("the plan doesn't carry shm_size: %s", text)
	}
	applyConfirmed(t, c, planned)
	res, err := services.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "headless", Image: "nginx:1.27"}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, res.Msg.GetChange())
	if e.Container("headless") != nil {
		t.Error("the container taken on is still there after a change of image")
	}
	var replaced *docker.Container
	for _, name := range e.Containers() {
		if got := e.Container(name); got != nil && got.Config.Labels[docker.LabelService] == "headless" {
			replaced = got
		}
	}
	if replaced == nil {
		t.Fatalf("nothing runs as headless: %v", e.Containers())
	}
	if replaced.HostConfig.ShmSize != 1<<30 || replaced.HostConfig.Tmpfs["/scratch"] != "size=1048576" {
		t.Errorf("its replacement runs with %d of /dev/shm and tmpfs %v", replaced.HostConfig.ShmSize, replaced.HostConfig.Tmpfs)
	}
}

// A port it exposes and doesn't publish is how what's on its network finds it: Traefik
// routes to the one exposed port when no label says. A replace keeps it, and the one
// after, and what its image exposes isn't anyone's to say.
func TestAnExposedPortComesAlong(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)
	c := changeClient(b, b.origin)
	services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	e.AddLocalImage("exposing:1", dockertest.Image{Config: docker.Config{Cmd: []string{"run"}, ExposedPorts: map[string]struct{}{"80/tcp": {}}}})
	e.AddLocalImage("exposing:2", dockertest.Image{Config: docker.Config{Cmd: []string{"run"}, ExposedPorts: map[string]struct{}{"80/tcp": {}}}})
	e.AddLocalImage("exposing:3", dockertest.Image{Config: docker.Config{Cmd: []string{"run"}}})
	runByHand(t, e, "behind-traefik", func(r *docker.CreateRequest) {
		r.Image = "exposing:1"
		r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
		// docker compose sends expose: as it was written, and Docker keeps it so.
		r.ExposedPorts = map[string]struct{}{"3000": {}, "4000-4001/udp": {}}
	})
	planned := adopt(t, cc, "behind-traefik", "api")
	if text := protojson.Format(planned); !strings.Contains(text, "3000/tcp") {
		t.Errorf("the plan doesn't say what it exposes: %s", text)
	}
	applyConfirmed(t, c, planned)
	for _, image := range []string{"exposing:2", "exposing:3"} {
		res, err := services.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "api", Image: image}))
		if err != nil {
			t.Fatal(err)
		}
		apply(t, c, res.Msg.GetChange())
		var replaced *docker.Container
		for _, name := range e.Containers() {
			if got := e.Container(name); got != nil && got.Config.Labels[docker.LabelService] == "api" && got.Config.Image == image {
				replaced = got
			}
		}
		if replaced == nil {
			t.Fatalf("nothing runs %s: %v", image, e.Containers())
		}
		for _, port := range []string{"3000/tcp", "4000/udp", "4001/udp"} {
			if _, on := replaced.Config.ExposedPorts[port]; !on {
				t.Errorf("on %s it exposes %v, and lost %s", image, replaced.Config.ExposedPorts, port)
			}
		}
	}
	file, err := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON()).ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(file.Msg.GetCompose(), "3000/tcp") {
		t.Errorf("an export leaves out what it exposes: %s", file.Msg.GetCompose())
	}
}

// A value of the environment is sealed, and a spec's other fields aren't, so a value
// anywhere else in one would show in the plan, and reach a read-only token and an
// export. Wherever it is, it's refused, and the refusal doesn't say it. A path isn't a
// secret, and TZ=Asia/Shanghai beside a mount of its zoneinfo is ordinary.
func shortIn(name string) func(*docker.CreateRequest) {
	return func(r *docker.CreateRequest) {
		r.Env = append(r.Env, name+"=ab1")
		r.Cmd = []string{"run", "--with", "ab1"}
	}
}

func TestAValueOfTheEnvironmentShowsNowhere(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)
	plainly := func(edit func(*docker.CreateRequest)) func(*docker.CreateRequest) {
		return func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
			edit(r)
		}
	}
	for name, c := range map[string]struct {
		edit func(*docker.CreateRequest)
		says string
	}{
		"in-label": {func(r *docker.CreateRequest) {
			r.Labels = map[string]string{"backup.password": secretValue}
		}, "the value of DB_PASSWORD in its label backup.password"},
		"in-label-name": {func(r *docker.CreateRequest) {
			r.Labels = map[string]string{"backup." + secretValue: "on"}
		}, "the value of DB_PASSWORD in its labels"},
		// Saying which label would say the value.
		"in-both": {func(r *docker.CreateRequest) {
			r.Labels = map[string]string{"backup." + secretValue: secretValue}
		}, "the value of DB_PASSWORD in its labels"},
		// A short value is no less a password. What its name says is what shows it.
		"short-password": {func(r *docker.CreateRequest) {
			r.Env = append(r.Env, "MYSQL_ROOT_PASSWORD=s3cr7")
			r.Healthcheck = &docker.Healthcheck{Test: []string{"CMD", "mysqladmin", "ping", "-ps3cr7"}}
			r.Labels = map[string]string{"backup.pw": "s3cr7"}
		}, "the value of MYSQL_ROOT_PASSWORD in its health check"},
		"short-pass":    {shortIn("db_pass"), "the value of db_pass in its command"},
		"short-secret":  {shortIn("APP_SECRET"), "the value of APP_SECRET in its command"},
		"short-token":   {shortIn("Api_Token"), "the value of Api_Token in its command"},
		"short-key":     {shortIn("STRIPE_KEY"), "the value of STRIPE_KEY in its command"},
		"short-cred":    {shortIn("CREDENTIALS"), "the value of CREDENTIALS in its command"},
		"in-user":       {func(r *docker.CreateRequest) { r.User = secretValue }, "the value of DB_PASSWORD in its user"},
		"in-entrypoint": {func(r *docker.CreateRequest) { r.Entrypoint = []string{"run", secretValue} }, "the value of DB_PASSWORD in its entrypoint"},
		"in-check": {func(r *docker.CreateRequest) {
			r.Healthcheck = &docker.Healthcheck{Test: []string{"CMD", "curl", "-u", secretValue}}
		}, "the value of DB_PASSWORD in its health check"},
	} {
		runByHand(t, e, name, plainly(c.edit))
		_, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: name}))
		if err == nil || !strings.Contains(err.Error(), c.says) || !strings.Contains(err.Error(), "a plan would show") {
			t.Errorf("taking on %s: %v, want it to say %q", name, err, c.says)
		}
		if err != nil && strings.Contains(err.Error(), secretValue) {
			t.Errorf("the refusal for %s shows the value: %v", name, err)
		}
	}

	// Its whole spec holds no value, on a container that passes. A short value whose
	// name says nothing could be anywhere by chance, and an image's own isn't a secret.
	runByHand(t, e, "with-zone", plainly(func(r *docker.CreateRequest) {
		r.Env = append(r.Env, "TZ=Asia/Shanghai", "DATA_DIR=/data/app", "MODE=app")
		r.Labels = map[string]string{"tier": "app"}
		r.WorkingDir = "/data/app"
		r.HostConfig.Binds = []string{"/usr/share/zoneinfo/Asia/Shanghai:/etc/localtime:ro"}
	}))
	change := adopt(t, cc, "with-zone", "zone")
	if text := protojson.Format(change); strings.Contains(text, secretValue) {
		t.Errorf("the plan shows a value: %s", text)
	}
}

// A Compose project usually has something else running it. Once Dockwell replaces the
// container, that can start a second copy on the same folders, so the plan says so,
// and what to do first. It warns and doesn't refuse: a project made by hand is fine.
func TestAPlanSaysWhatElseManagesAContainer(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)
	compose := func(project, dir string) func(*docker.CreateRequest) {
		return func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
			if project != "" {
				r.Labels = map[string]string{docker.LabelProject: project, docker.LabelService: "app", docker.LabelWorkingDir: dir}
			}
		}
	}
	managed := func(c *dockwellv1.Change) *dockwellv1.PlanNote {
		for _, n := range c.GetPlan().GetNotes() {
			if n.GetReason() == "managed_elsewhere" {
				return n
			}
		}
		return nil
	}

	// What a person is told to do is what 1Panel's source says is safe: its uninstall
	// runs docker compose down, then deletes the app's folder, with no way to keep it,
	// and a stop stops this same container.
	const folder = "/opt/1panel/apps/mysql/blog"
	runByHand(t, e, "1Panel-blog-Ab3x", compose("blog", folder))
	planned := adopt(t, cc, "1Panel-blog-Ab3x", "blog")
	n := managed(planned)
	if n == nil || !slices.Equal(n.GetNames(), []string{"1Panel", "1Panel-blog-Ab3x", "blog", folder}) {
		t.Fatalf("taking on what 1Panel made says %v", n)
	}
	advice := func(what string, n *dockwellv1.PlanNote) {
		t.Helper()
		for _, says := range []string{
			"1Panel still manages 1Panel-blog-Ab3x",
			"Until Dockwell changes its image, anything done to it in 1Panel acts on this container",
			// Its status sync reports a container it can't find as an error, which
			// invites just what would start a second copy.
			"After that, 1Panel shows it in error", "Leave that as it is",
			"Start, Rebuild, Upgrade or editing its settings there starts a second copy",
			"Don't uninstall it in 1Panel", "that deletes " + folder, "which holds what this service mounts",
		} {
			if !strings.Contains(n.GetMessage(), says) {
				t.Errorf("%s: the note %q doesn't say %q", what, n.GetMessage(), says)
			}
		}
		// Each of these would lose someone's data, or stop what they took on.
		for _, never := range []string{"keep its data", "Stop it in", "remove it there", "Restart"} {
			if strings.Contains(n.GetMessage(), never) {
				t.Errorf("%s: the note %q says %q", what, n.GetMessage(), never)
			}
		}
	}
	advice("taking it on", n)

	// The first plan that replaces it says it again, when it starts to matter, and
	// the next has nothing left to say.
	applyConfirmed(t, changeClient(b, b.origin), planned)
	services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	deploy := func(image string) *dockwellv1.Change {
		t.Helper()
		res, err := services.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "blog", Image: image}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetChange()
	}
	first := deploy("nginx:1.27")
	n = managed(first)
	if n == nil {
		t.Fatalf("the first plan that replaces it says nothing of 1Panel: %v", first.GetPlan().GetNotes())
	}
	advice("replacing it", n)
	apply(t, changeClient(b, b.origin), first)
	e.AddLocalImage("nginx:1.28", dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}})
	if n := managed(deploy("nginx:1.28")); n != nil {
		t.Errorf("a plan after the first still says %v", n)
	}

	// Another tool's remove hasn't been read, so it isn't told to use it, or not.
	runByHand(t, e, "coolify-web", func(r *docker.CreateRequest) {
		compose("web", "/data/coolify/applications/x")(r)
		r.Labels["coolify.managed"] = "true"
	})
	n = managed(adopt(t, cc, "coolify-web", "web"))
	if n == nil || n.GetNames()[0] != "Coolify" || strings.Contains(n.GetMessage(), "keep its data") ||
		!strings.Contains(n.GetMessage(), "find out what that does to /data/coolify/applications/x") {
		t.Errorf("taking on what Coolify made says %v", n)
	}

	// With no way to tell, it says what to look for, and asks nothing of a project
	// made by hand.
	runByHand(t, e, "shop-app-1", compose("shop", "/srv/shop"))
	n = managed(adopt(t, cc, "shop-app-1", "shop"))
	if n == nil || n.GetNames()[0] != "" || !strings.Contains(n.GetMessage(), "If you ran docker compose yourself") ||
		strings.Contains(n.GetMessage(), "keep its data") || !strings.Contains(n.GetMessage(), "find out what its remove does to /srv/shop") {
		t.Errorf("taking on a Compose project nothing names says %v", n)
	}

	runByHand(t, e, "by-hand", compose("", ""))
	if n := managed(adopt(t, cc, "by-hand", "by-hand")); n != nil {
		t.Errorf("a container of no Compose project says %v", n)
	}
}

// A container others lean on can't be replaced: a sidecar's app has only loopback once
// the container that held its network has gone. So it's refused, by the name of what
// leans on it, and how, when it's taken on and when a plan would replace it.
func TestWhatOthersLeanOnStaysAsItIs(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)
	c := changeClient(b, b.origin)
	services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	plain := func(r *docker.CreateRequest) { r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil }
	ts := runByHand(t, e, "ts", plain)
	refuses := func(id, says string) {
		t.Helper()
		_, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: id}))
		if err == nil || !strings.Contains(err.Error(), says) || !strings.Contains(err.Error(), "It stays as it is") {
			t.Errorf("taking on %s: %v, want it to say %q", id, err, says)
		}
	}
	for name, c := range map[string]struct {
		keys map[string]any
		says string
	}{
		"by-network": {map[string]any{"NetworkMode": "container:" + ts}, "by-network shares its network"},
		"by-pid":     {map[string]any{"PidMode": "container:" + ts}, "by-pid shares its processes"},
		"by-ipc":     {map[string]any{"IpcMode": "container:" + ts}, "by-ipc shares its shared memory"},
		"by-link":    {map[string]any{"Links": []any{"/ts:/by-link/ts"}}, "by-link links to it"},
		"by-volumes": {map[string]any{"VolumesFrom": []any{"ts:ro"}}, "by-volumes mounts its volumes"},
	} {
		runByHand(t, e, name, plain)
		e.Reports(name, "HostConfig", c.keys)
		refuses("ts", c.says)
		e.Remove(name)
	}

	// Something that came to lean on it since keeps it from being replaced.
	applyConfirmed(t, c, adopt(t, cc, "ts", "held"))
	runByHand(t, e, "app", plain)
	e.Reports("app", "HostConfig", map[string]any{"NetworkMode": "container:" + ts})
	deploy := func() (*connect.Response[dockwellv1.DeployServiceResponse], error) {
		return services.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "held", Image: "nginx:1.27"}))
	}
	if _, err := deploy(); err == nil || !strings.Contains(err.Error(), "app shares its network") {
		t.Errorf("replacing what app shares the network of: %v", err)
	}
	if e.Container("ts") == nil {
		t.Fatal("a refused plan took ts away")
	}
	e.Remove("app")
	if _, err := deploy(); err != nil {
		t.Errorf("with nothing leaning on it: %v", err)
	}
}

// A step that fails partway leaves no project half taken on: what it had taken on it
// lets go of, with nothing touched, and the project it made goes.
func TestAFailedStepLeavesNoProjectHalfTakenOn(t *testing.T) {
	for _, failAt := range []int{2, 4} {
		t.Run(strconv.Itoa(failAt), func(t *testing.T) {
			f := startFull(t, store.SQLite, nil)
			b := signedIn(t, f)
			e := f.addDockerServer(t)
			e.AddNetwork(docker.Network{ID: "shopnet", Name: "shop_default", Driver: "bridge"})
			c := changeClient(b, b.origin)
			cc := containerClient(b, b.origin)
			imports := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
			composeContainer(t, e, "shop", "db", "shop-db-1", nil)
			web := composeContainer(t, e, "shop", "web", "shop-web-1", nil)
			res, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "shop"}))
			if err != nil {
				t.Fatal(err)
			}
			planned := res.Msg.GetChange()
			from := len(e.Seen())
			var seen atomic.Int32
			e.Intercept(func(r *http.Request) error {
				if strings.HasSuffix(r.URL.Path, "/containers/"+web+"/json") && int(seen.Add(1)) == failAt {
					return errors.New("the engine failed this one")
				}
				return nil
			})
			w := watch(t, c, planned.GetId())
			_, err = c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: planned.GetId(), Fingerprint: planned.GetPlan().GetFingerprint()}))
			if err == nil {
				waitWatch(t, w)
			}
			e.Intercept(nil)
			if got := mutations(e, from); len(got) != 0 {
				t.Errorf("a failed take-on asked Docker for %v", got)
			}
			var list *connect.Response[dockwellv1.ListContainersResponse]
			for range 100 {
				if list, err = cc.ListContainers(t.Context(), connect.NewRequest(&dockwellv1.ListContainersRequest{})); err != nil {
					t.Fatal(err)
				}
				if len(list.Msg.GetContainers()) == 2 {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if got := list.Msg.GetContainers(); len(got) != 2 {
				t.Errorf("after a failed step, what Dockwell didn't make is %v, want both back", got)
			}
			var projects []string
			err = f.store.View(t.Context(), func(tx *store.Tx) error {
				tree, err := scope.Load(t.Context(), tx)
				for _, p := range tree.Projects {
					projects = append(projects, p.Name)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(projects, []string{"default"}) {
				t.Errorf("after a failed step the projects are %v", projects)
			}
		})
	}
}

// One refusal says everything in the way, so a person fixes it once, whatever kind of
// thing is in the way: a scaled service, a name the project runs already, and what no
// spec holds all come together.
func TestARefusalSaysEverythingAtOnce(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	for _, n := range []string{"mix", "split", "shop", "shop2"} {
		e.AddNetwork(docker.Network{ID: n + "net", Name: n + "_default", Driver: "bridge"})
	}
	imports := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	take := func(req *dockwellv1.AdoptComposeRequest) error {
		_, err := imports.AdoptCompose(t.Context(), connect.NewRequest(req))
		return err
	}
	says := func(what string, err error, want ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s wasn't refused", what)
		}
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %v, want it to say %q", what, err, w)
			}
		}
	}
	readOnly := func(r *docker.CreateRequest) { r.HostConfig.ReadonlyRootfs = true }

	composeContainer(t, e, "mix", "web", "mix-web-1", nil)
	composeContainer(t, e, "mix", "web", "mix-web-2", nil)
	composeContainer(t, e, "mix", "db", "mix-db-1", readOnly)
	says("a scaled service beside what can't come", take(&dockwellv1.AdoptComposeRequest{Name: "mix"}),
		"mix-web-1 and mix-web-2 are the service web", "--scale web=1", "mix-db-1 runs with read_only", "It all stays as it is")

	// Two services that come to one name are no scaled service, so no --scale helps.
	composeContainer(t, e, "split", "api_v1", "split-api_v1-1", nil)
	composeContainer(t, e, "split", "api-v1", "split-api-v1-1", nil)
	err := take(&dockwellv1.AdoptComposeRequest{Name: "split"})
	says("two services of one name", err, "split-api-v1-1 and split-api_v1-1 would both be the service api-v1", "Rename one")
	if err != nil && strings.Contains(err.Error(), "--scale") {
		t.Errorf("two services read as a scaled one: %v", err)
	}

	composeContainer(t, e, "shop", "web", "shop-web-1", nil)
	planned, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	applyConfirmed(t, changeClient(b, b.origin), planned.Msg.GetChange())
	composeContainer(t, e, "shop2", "web", "shop2-web-1", readOnly)
	says("a name it runs beside what can't come", take(&dockwellv1.AdoptComposeRequest{Name: "shop2", Project: "shop"}),
		"the project shop runs a service web already", "shop2-web-1 runs with read_only")
}

// Compose gives a service its name on every network it joins, so two installs of one
// app both answer to it on 1panel-network, as they do today. Taking one on changes
// nothing about that, so it isn't refused, and the plan says what it means. A clash
// Dockwell would make itself still is.
func TestAContainerKeepsANameOthersHoldToo(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddNetwork(docker.Network{ID: "onepanel", Name: "1panel-network", Driver: "bridge"})
	c := changeClient(b, b.origin)
	cc := containerClient(b, b.origin)
	imports := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	onPanel := func(project, name string) func(*docker.CreateRequest) {
		return func(r *docker.CreateRequest) {
			r.Labels["createdBy"] = "Apps"
			r.Labels[docker.LabelWorkingDir] = "/opt/1panel/apps/redis/" + project
			r.HostConfig.NetworkMode = "1panel-network"
			r.NetworkingConfig.EndpointsConfig = map[string]*docker.EndpointSettings{"1panel-network": {Aliases: []string{"redis", name}}}
		}
	}
	composeContainer(t, e, "redis", "redis", "1Panel-redis-AbCd", onPanel("redis", "1Panel-redis-AbCd"))
	composeContainer(t, e, "redis2", "redis", "1Panel-redis2-EfGh", onPanel("redis2", "1Panel-redis2-EfGh"))

	res, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "redis"}))
	if err != nil {
		t.Fatalf("taking on one of two installs: %v", err)
	}
	planned := res.Msg.GetChange()
	var n *dockwellv1.PlanNote
	for _, note := range planned.GetPlan().GetNotes() {
		if note.GetReason() == "shares_a_name" {
			n = note
		}
	}
	if n == nil || !slices.Equal(n.GetNames(), []string{"redis", "1panel-network", "1Panel-redis2-EfGh"}) {
		t.Fatalf("the plan doesn't say who else answers to it: %v", planned.GetPlan().GetNotes())
	}
	for _, says := range []string{"More than one container answers to redis on the network 1panel-network", "can reach either"} {
		if !strings.Contains(n.GetMessage(), says) {
			t.Errorf("the note %q doesn't say %q", n.GetMessage(), says)
		}
	}
	applyConfirmed(t, c, planned)

	// A replace keeps what it held, so the first new image isn't refused for it either.
	rep, err := services.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "redis", Project: "redis", Image: "nginx:1.27"}))
	if err != nil {
		t.Fatalf("a new image of what holds a shared name: %v", err)
	}
	apply(t, c, rep.Msg.GetChange())

	// Alone, under a name of its own, the other comes the same way.
	if _, err := cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "1Panel-redis2-EfGh", Service: "redis-b", Project: "default"})); err != nil {
		t.Errorf("taking on the other under a name of its own: %v", err)
	}

	// A name it doesn't hold is one Dockwell would be taking, which it won't.
	composeContainer(t, e, "clash", "other", "clash-other-1", func(r *docker.CreateRequest) {
		r.HostConfig.NetworkMode = "1panel-network"
		r.NetworkingConfig.EndpointsConfig = map[string]*docker.EndpointSettings{"1panel-network": {Aliases: []string{"other"}}}
	})
	_, err = cc.AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "clash-other-1", Service: "redis", Project: "default"}))
	if err == nil || !strings.Contains(err.Error(), "would answer to redis on the network 1panel-network") {
		t.Errorf("a service named what others answer to, which it doesn't: %v", err)
	}
}

// A token may run a new image on what was taken on, as on any service: that's what a
// deploy from CI is. Not on what's as good as root, where a new image is any code
// with root's reach, so a person approves that, whichever way it got there.
func TestANewImageAsGoodAsRootNeedsAPerson(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)
	c := changeClient(b, b.origin)
	script := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)
	services := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	takeOn := func(name string, edit func(*docker.CreateRequest)) {
		t.Helper()
		runByHand(t, e, name, func(r *docker.CreateRequest) {
			r.HostConfig.PortBindings, r.HostConfig.Binds = nil, nil
			edit(r)
		})
		applyConfirmed(t, c, adopt(t, cc, name, name))
	}
	takeOn("plain", func(*docker.CreateRequest) {})
	takeOn("runs-docker", func(r *docker.CreateRequest) {
		r.HostConfig.Binds = []string{"/var/run/docker.sock:/var/run/docker.sock"}
	})
	takeOn("runs-privileged", func(r *docker.CreateRequest) { r.HostConfig.Privileged = true })

	deploy := func(name string) *dockwellv1.Change {
		t.Helper()
		res, err := services.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: name, Image: "nginx:1.27"}))
		if err != nil {
			t.Fatalf("deploying %s: %v", name, err)
		}
		return res.Msg.GetChange()
	}
	if p := deploy("plain"); p.GetPlan().GetHostAccess() {
		t.Errorf("a new image on an ordinary service needs a person: %v", p.GetPlan())
	} else {
		apply(t, script, p)
	}
	for _, name := range []string{"runs-docker", "runs-privileged"} {
		p := deploy(name)
		if !p.GetPlan().GetHostAccess() {
			t.Errorf("a new image on %s asks nothing of anyone: %v", name, p.GetPlan())
		}
		tokenRefused(t, script, p, name)
		if !slices.ContainsFunc(p.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool { return n.GetReason() == "new_code_as_root" }) {
			t.Errorf("the plan for %s doesn't say why it needs a person: %v", name, p.GetPlan().GetNotes())
		}
		if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: p.GetId()})); err != nil {
			t.Fatal(err)
		}
	}
}

// A new command is any code at all too, and a health check runs one, so what's as good
// as root needs a person for those as for an image: a token that holds the socket's
// command runs what it likes as root. That holds for what Dockwell made as for what it
// took on. The environment isn't that, and stays open.
func TestANewCommandAsGoodAsRootNeedsAPerson(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	cc := containerClient(b, b.origin)
	c := changeClient(b, b.origin)
	script := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)
	runByHand(t, e, "runs-docker", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings = nil
		r.HostConfig.Binds = []string{"/var/run/docker.sock:/var/run/docker.sock"}
	})
	applyConfirmed(t, c, adopt(t, cc, "runs-docker", "runs-docker"))
	env := listedEnv(t, b)
	current := func() *resourcev1.Service {
		t.Helper()
		var spec proto.Message
		err := f.store.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResource(t.Context(), "service", env, "runs-docker")
			if err != nil {
				return err
			}
			spec, err = f.keeper.Decode(t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return proto.Clone(spec).(*resourcev1.Service)
	}
	plan := func(what string, edit func(*resourcev1.Container)) *dockwellv1.Change {
		t.Helper()
		svc := current()
		edit(svc.GetApp())
		return propose(t, c, what, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: env, Name: "runs-docker"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}},
		})
	}
	for what, edit := range map[string]func(*resourcev1.Container){
		"a command":     func(a *resourcev1.Container) { a.Command = []string{"sh", "-c", "docker run --privileged alpine"} },
		"an entrypoint": func(a *resourcev1.Container) { a.Entrypoint = []string{"sh", "-c", "docker ps"} },
		"a health check": func(a *resourcev1.Container) {
			a.Healthcheck = &resourcev1.Healthcheck{Test: []string{"CMD-SHELL", "docker ps"}}
		},
	} {
		p := plan(what, edit)
		if !p.GetPlan().GetHostAccess() {
			t.Errorf("%s on what holds the socket asks nothing of anyone: %v", what, p.GetPlan())
		}
		tokenRefused(t, script, p, "runs-docker")
		if !slices.ContainsFunc(p.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool { return n.GetReason() == "new_code_as_root" }) {
			t.Errorf("the plan for %s doesn't say why it needs a person: %v", what, p.GetPlan().GetNotes())
		}
		if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: p.GetId()})); err != nil {
			t.Fatal(err)
		}
	}
	if p := plan("a variable", func(a *resourcev1.Container) { a.Env["LOG_LEVEL"] = "debug" }); p.GetPlan().GetHostAccess() {
		t.Errorf("a change of environment on what holds the socket needs a person: %v", p.GetPlan().GetResources())
	}
}

// A token can't take on a container, and can't plan to. A plan shows a stranger's
// command, labels, user and mounts, which the list of containers leaves out, and it
// stays on record for a read-only token. Applying is for a person too: a token could
// read the environment that comes in through a command of its own.
func TestATokenCantTakeOnAContainer(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	runByHand(t, e, "by-hand", nil)
	token := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}
	_, err := containerClient(token, b.origin).AdoptContainer(t.Context(), connect.NewRequest(&dockwellv1.AdoptContainerRequest{Id: "by-hand", Service: "api"}))
	if reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("a token planning a take-on = %v, want %s", err, authapi.ReasonNeedsSession)
	}
	planned := adopt(t, containerClient(b, b.origin), "by-hand", "api")
	tokenRefused(t, changeClient(token, b.origin), planned, "its environment, which comes in with it")

	// Nor a project whole.
	imports := dockwellv1connect.NewComposeServiceClient(token, b.origin, connect.WithProtoJSON())
	_, err = imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "shop"}))
	if reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("a token planning a take-on of a project = %v, want %s", err, authapi.ReasonNeedsSession)
	}
}

// composeContainer runs what docker compose makes of one service: its labels, and a
// place on the project's network under the service's name.
func composeContainer(t *testing.T, e *dockertest.Engine, project, service, name string, edit func(*docker.CreateRequest)) string {
	t.Helper()
	return runByHand(t, e, name, func(r *docker.CreateRequest) {
		r.Labels = map[string]string{docker.LabelProject: project, docker.LabelService: service, docker.LabelNumber: "1", docker.LabelOneoff: "False"}
		r.HostConfig.PortBindings, r.HostConfig.Binds, r.HostConfig.NetworkMode = nil, nil, project+"_default"
		r.NetworkingConfig.EndpointsConfig = map[string]*docker.EndpointSettings{project + "_default": {Aliases: []string{service, name}}}
		if edit != nil {
			edit(r)
		}
	})
}

// A Compose project made elsewhere comes in whole, in one change, with none of it
// touched: each of its containers a service, on the network and volumes it has, and
// each one's image can then change on its own.
func TestTakingOnACompose(t *testing.T) { storetest.Each(t, testTakingOnACompose) }

func testTakingOnACompose(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddNetwork(docker.Network{ID: "shopnet", Name: "shop_default", Driver: "bridge"})
	e.AddNetwork(docker.Network{ID: "othernet", Name: "other_default", Driver: "bridge"})
	c := changeClient(b, b.origin)
	cc := containerClient(b, b.origin)
	imports := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())

	web := composeContainer(t, e, "shop", "web", "shop-web-1", func(r *docker.CreateRequest) {
		r.HostConfig.PortBindings = map[string][]docker.PortBinding{"80/tcp": {{HostPort: "8089"}}}
	})
	// A one-off is no container of the service it ran as, so it's no scaled one.
	composeContainer(t, e, "shop", "web", "shop-web-run-1", func(r *docker.CreateRequest) { r.Labels[docker.LabelOneoff] = "True" })
	db := composeContainer(t, e, "shop", "db", "shop-db-1", func(r *docker.CreateRequest) {
		r.HostConfig.Binds = []string{"shop_db-data:/var/lib/data"}
	})
	composeContainer(t, e, "other", "web", "other-web-1", nil)

	seen := len(e.Seen())
	res, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "shop"}))
	if err != nil {
		t.Fatal(err)
	}
	planned := res.Msg.GetChange()
	for _, n := range planned.GetPlan().GetNotes() {
		if n.GetReason() == "takes_on" && !strings.HasSuffix(n.GetMessage(), ".") {
			t.Errorf("the note %q has no full stop", n.GetMessage())
		}
	}
	if !slices.ContainsFunc(planned.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool {
		return n.GetReason() == "managed_elsewhere" && strings.Contains(n.GetMessage(), "the Compose project shop")
	}) {
		t.Errorf("the plan doesn't say something may still run the project: %v", planned.GetPlan().GetNotes())
	}
	if s := planned.String(); strings.Contains(s, secretValue) {
		t.Fatalf("the plan shows a value of an environment: %s", s)
	}
	took := map[string]bool{}
	for _, step := range planned.GetPlan().GetServers()[0].GetSteps() {
		if step.GetAdopts() != "" {
			took[step.GetAdopts()] = true
		}
	}
	if !took[web] || !took[db] || len(took) != 2 {
		t.Fatalf("the plan takes on %v, want the two of shop", took)
	}
	applyConfirmed(t, c, planned)
	if got := mutations(e, seen); len(got) != 0 {
		t.Errorf("taking the project on asked Docker to %v", got)
	}
	list, err := cc.ListContainers(t.Context(), connect.NewRequest(&dockwellv1.ListContainersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(list.Msg.GetContainers()); !slices.Equal(got, []string{"other-web-1", "shop-web-run-1"}) {
		t.Errorf("the list has %v, want what's left", got)
	}
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Errorf("drift after taking it on = %v, %v", drift.Msg.GetDrift(), err)
	}

	// One service's image changes on its own, on the project's network, and the other
	// isn't touched.
	svc := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	deployed, err := svc.DeployService(t.Context(), connect.NewRequest(&dockwellv1.DeployServiceRequest{Id: "web", Project: "shop", Image: "nginx:1.27"}))
	if err != nil {
		t.Fatal(err)
	}
	done := apply(t, c, deployed.Msg.GetChange())
	if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the image change ended %v: %s", done.GetState(), done.GetError())
	}
	project := docker.Project(planned.GetPlan().GetResources()[len(planned.GetPlan().GetResources())-1].GetResource().GetScope())
	now := e.Newest(project, "web")
	if now == nil || now.Config.Image != "nginx:1.27" {
		t.Fatalf("Docker runs %+v", now)
	}
	if _, on := now.NetworkSettings.Networks["shop_default"]; !on {
		t.Errorf("web left the project's network: %v", now.NetworkSettings.Networks)
	}
	if e.Container("shop-db-1") == nil {
		t.Errorf("the other service's container went")
	}
}

// A project comes in whole or not at all, and a refusal says so for all of it at once.
func TestAComposeThatCantComeInSaysWhy(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddNetwork(docker.Network{ID: "shopnet", Name: "shop_default", Driver: "bridge"})
	e.AddNetwork(docker.Network{ID: "scalednet", Name: "scaled_default", Driver: "bridge"})
	imports := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())

	composeContainer(t, e, "shop", "web", "shop-web-1", func(r *docker.CreateRequest) { r.HostConfig.ReadonlyRootfs = true })
	composeContainer(t, e, "shop", "db", "shop-db-1", func(r *docker.CreateRequest) {
		r.Cmd = []string{"db", "--password=" + secretValue}
	})
	composeContainer(t, e, "scaled", "web", "scaled-web-1", nil)
	composeContainer(t, e, "scaled", "web", "scaled-web-2", nil)

	_, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "shop"}))
	if err == nil || !strings.Contains(err.Error(), "shop-web-1 runs with read_only") || !strings.Contains(err.Error(), "shop-db-1 holds the value of DB_PASSWORD") {
		t.Errorf("a project with what can't come: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), secretValue) {
		t.Errorf("the refusal shows a value: %v", err)
	}
	if _, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "scaled"})); err == nil || !strings.Contains(err.Error(), "--scale web=1") {
		t.Errorf("a scaled service: %v", err)
	}
	if _, err := imports.AdoptCompose(t.Context(), connect.NewRequest(&dockwellv1.AdoptComposeRequest{Name: "nothing"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a project that isn't there: %v", err)
	}
}
