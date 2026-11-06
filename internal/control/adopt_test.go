// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
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

	for name, says := range map[string]string{
		"in-word":    "the value of DB_PASSWORD in its command",
		"own-rootfs": "read_only",
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
		"with-tmpfs":    {"HostConfig", map[string]any{"Mounts": []any{map[string]any{"Type": "tmpfs", "Target": "/cache"}}}, "a tmpfs mount at /cache"},
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

// A value of the environment is sealed, and a spec's other fields aren't, so a value
// anywhere else in one would show in the plan, and reach a read-only token and an
// export. Wherever it is, it's refused, and the refusal doesn't say it. A path isn't a
// secret, and TZ=Asia/Shanghai beside a mount of its zoneinfo is ordinary.
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

	// Its whole spec holds no value, on a container that passes.
	runByHand(t, e, "with-zone", plainly(func(r *docker.CreateRequest) {
		r.Env = append(r.Env, "TZ=Asia/Shanghai", "DATA_DIR=/data/app")
		r.WorkingDir = "/data/app"
		r.HostConfig.Binds = []string{"/usr/share/zoneinfo/Asia/Shanghai:/etc/localtime:ro"}
	}))
	change := adopt(t, cc, "with-zone", "zone")
	if text := protojson.Format(change); strings.Contains(text, secretValue) {
		t.Errorf("the plan shows a value: %s", text)
	}
}

// A token that can change things can't bring in a stranger's environment: it could
// read it through a command of its own, and a stranger's was never in its reach.
func TestATokenCantTakeOnAContainer(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	runByHand(t, e, "by-hand", nil)
	cc := containerClient(b, b.origin)
	planned := adopt(t, cc, "by-hand", "api")
	script := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)
	tokenRefused(t, script, planned, "its environment, which comes in with it")
}
