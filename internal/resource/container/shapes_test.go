// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

const shapesEnv = "env_01kat4zr6e3v0y5xbkzb5d9q6m"

func shapesEngine(t *testing.T) (*dockertest.Engine, resource.Local) {
	t.Helper()
	e := dockertest.New(t)
	e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	e.Registry["nginx:1.28"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	e.AddNetwork(docker.Network{Name: "edge", Driver: "bridge"})
	e.AddNetwork(docker.Network{Name: docker.ScopedName(shapesEnv, "back"), Driver: "bridge"})
	rt := &container.Docker{Client: e.Client(), Settle: 1, Poll: 1, Soak: -1}
	d := &service.Driver{Containers: container.Driver{Runtime: rt}}
	return e, resource.Local{Drivers: resource.NewRegistry(d)}
}

func svc(c *resourcev1.Container) *resourcev1.Service {
	if c.Image == "" {
		c.Image = "nginx:1.27"
	}
	// As the engine does with a spec, so what it reads back compares with it.
	if err := container.Validate(c); err != nil {
		panic(err)
	}
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}
}

func webRef() resource.Ref { return resource.Ref{Kind: service.Kind, Scope: shapesEnv, Name: "web"} }

func planAndApply(t *testing.T, l resource.Local, ref resource.Ref, spec *resourcev1.Service) resource.Step {
	t.Helper()
	steps, err := l.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: spec}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := l.Apply(t.Context(), steps[0], spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return steps[0]
}

// What a spec asks for is what Docker gets, reads back as the same spec, and so
// shows as no drift.
func TestAServiceRunsAndReadsBackAsItsSpec(t *testing.T) {
	e, l := shapesEngine(t)
	spec := svc(&resourcev1.Container{
		Networks: []string{"back"}, ExternalNetworks: []string{"edge"}, Aliases: []string{"Db"},
		Resources: &resourcev1.Resources{Cpus: "1.5", Memory: "1G", MemoryReservation: "256M"},
		CapAdd:    []string{"NET_ADMIN"}, CapDrop: []string{"NET_RAW"}, GroupAdd: []string{"999"}, Privileged: true,
		Restart: resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE, RestartRetries: 5,
	})
	planAndApply(t, l, webRef(), spec)
	c := e.Container(service.ContainerName(webRef()))
	h := c.HostConfig
	if h.NanoCPUs != 1_500_000_000 || h.Memory != 1<<30 || h.MemoryReservation != 256<<20 {
		t.Errorf("limits on Docker: %d, %d, %d", h.NanoCPUs, h.Memory, h.MemoryReservation)
	}
	if !slices.Equal(h.CapAdd, []string{"NET_ADMIN"}) || !slices.Equal(h.CapDrop, []string{"NET_RAW"}) || !slices.Equal(h.GroupAdd, []string{"999"}) || !h.Privileged {
		t.Errorf("privileges on Docker: %+v", h)
	}
	if h.RestartPolicy.Name != "on-failure" || h.RestartPolicy.MaximumRetryCount != 5 {
		t.Errorf("restart on Docker: %+v", h.RestartPolicy)
	}
	for _, n := range []string{docker.ScopedName(shapesEnv, "back"), "edge"} {
		ep := c.NetworkSettings.Networks[n]
		if ep == nil || !slices.Equal(ep.Aliases, []string{"web", "Db"}) {
			t.Errorf("on %s it answers to %+v", n, ep)
		}
	}
	got, err := l.Observe(t.Context(), []resource.Ref{webRef()})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got[0], spec) {
		t.Errorf("reads back as a different spec, which would show as drift: %v", resource.Diff(spec, got[0]))
	}
}

// Docker's own additions to what it reports never read as drift.
func TestDockersOwnNamesAreNotDrift(t *testing.T) {
	e, l := shapesEngine(t)
	spec := svc(&resourcev1.Container{ExternalNetworks: []string{"edge"}, Aliases: []string{"db"}})
	planAndApply(t, l, webRef(), spec)
	name := service.ContainerName(webRef())
	e.Edit(name, func(c *docker.Container) {
		c.NetworkSettings.Networks["edge"].Aliases = []string{c.ID[:12], name, "web", "db"}
	})
	got, err := l.Observe(t.Context(), []resource.Ref{webRef()})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got[0], spec) {
		t.Errorf("Docker's additions read as drift: %v", resource.Diff(spec, got[0]))
	}
	// Compose reads a number of CPUs through a float of 32 bits, so what it asks
	// Docker for can be a little off: to the nearest thousandth, it isn't drift.
	limited := svc(&resourcev1.Container{ExternalNetworks: []string{"edge"}, Aliases: []string{"db"}, Resources: &resourcev1.Resources{Cpus: "0.3"}})
	planAndApply(t, l, webRef(), limited)
	e.Edit(name, func(c *docker.Container) { c.HostConfig.NanoCPUs = 300_000_012 })
	if got, err := l.Observe(t.Context(), []resource.Ref{webRef()}); err != nil || !proto.Equal(got[0], limited) {
		t.Errorf("Compose's rounding reads as drift: %v, %v", resource.Diff(limited, got[0]), err)
	}
	e.Edit(name, func(c *docker.Container) { c.HostConfig.NanoCPUs = 299_999_990 })
	if got, err := l.Observe(t.Context(), []resource.Ref{webRef()}); err != nil || !proto.Equal(got[0], limited) {
		t.Errorf("Compose's rounding down reads as drift: %v, %v", resource.Diff(limited, got[0]), err)
	}
	planAndApply(t, l, webRef(), spec)
	// Lifting a limit makes a new container, under the next number.
	name = e.Newest(docker.Project(webRef().Scope), webRef().Name).Name
	name = strings.TrimPrefix(name, "/")
	// Being on the ingress network is the ingress's doing, and no spec's.
	e.Edit(name, func(c *docker.Container) {
		c.NetworkSettings.Networks[docker.IngressNetwork] = &docker.EndpointSettings{Aliases: []string{"web"}}
	})
	if got, err := l.Observe(t.Context(), []resource.Ref{webRef()}); err != nil || !proto.Equal(got[0], spec) {
		t.Errorf("the ingress network reads as drift: %v, %v", resource.Diff(spec, got[0]), err)
	}
	e.Edit(name, func(c *docker.Container) { delete(c.NetworkSettings.Networks, docker.IngressNetwork) })
	// A name added by hand is drift.
	e.Edit(name, func(c *docker.Container) {
		c.NetworkSettings.Networks["edge"].Aliases = []string{"web", "db", "shadow"}
	})
	if got, _ := l.Observe(t.Context(), []resource.Ref{webRef()}); proto.Equal(got[0], spec) {
		t.Error("a name added by hand doesn't read as drift")
	}
}

// An engine before 1.44 takes one network at create, and the rest join after,
// with the same names on each.
func TestOlderEnginesGiveTheSameNames(t *testing.T) {
	e, l := shapesEngine(t)
	e.APIVersion = "1.43"
	planAndApply(t, l, webRef(), svc(&resourcev1.Container{Networks: []string{"back"}, ExternalNetworks: []string{"edge"}, Aliases: []string{"db"}}))
	c := e.Container(service.ContainerName(webRef()))
	for _, n := range []string{docker.ScopedName(shapesEnv, "back"), "edge"} {
		if ep := c.NetworkSettings.Networks[n]; ep == nil || !slices.Equal(ep.Aliases, []string{"web", "db"}) {
			t.Errorf("on %s it answers to %+v", n, ep)
		}
	}
}

func TestAServiceOnTheServersNetwork(t *testing.T) {
	e, l := shapesEngine(t)
	spec := svc(&resourcev1.Container{HostNetwork: true})
	planAndApply(t, l, webRef(), spec)
	if got := e.Container(service.ContainerName(webRef())).HostConfig.NetworkMode; got != "host" {
		t.Errorf("network mode %q", got)
	}
	got, err := l.Observe(t.Context(), []resource.Ref{webRef()})
	if err != nil || !proto.Equal(got[0], spec) {
		t.Errorf("reads back as %v, %v", got, err)
	}
}

// A network the server hasn't got is refused as it plans, by name. So is one that
// went after the change was planned, as it applies.
func TestAMissingNetworkIsRefusedByName(t *testing.T) {
	e, l := shapesEngine(t)
	spec := svc(&resourcev1.Container{ExternalNetworks: []string{"nowhere"}})
	_, err := l.Plan(t.Context(), []resource.Desired{{Ref: webRef(), Spec: spec}})
	if err == nil || !strings.Contains(err.Error(), "the network nowhere isn't on this server") || !strings.Contains(err.Error(), "docker network create nowhere") {
		t.Errorf("Plan = %v", err)
	}
	created := e.Creates
	step := resource.Step{Ref: webRef(), Decision: resource.Decision{Action: resource.ActionCreate}}
	if err := l.Apply(t.Context(), step, spec); err == nil || !strings.Contains(err.Error(), "the network nowhere isn't on this server") {
		t.Errorf("Apply = %v", err)
	}
	if e.Creates != created || e.Network("nowhere") != nil {
		t.Error("it went on to make something for a network that isn't there")
	}
}

// Whatever else is on a shared network, in another project or none, can't answer
// to the same name, as each would get the other's traffic.
func TestNamesOnASharedNetwork(t *testing.T) {
	spec := func() *resourcev1.Service {
		return svc(&resourcev1.Container{ExternalNetworks: []string{"edge"}, Aliases: []string{"db"}})
	}
	other := func(name string, labels map[string]string, aliases ...string) docker.Container {
		c := docker.Container{Name: "/" + name, Config: docker.Config{Labels: labels}}
		c.NetworkSettings.Networks = map[string]*docker.EndpointSettings{"edge": {Aliases: aliases}}
		return c
	}
	for name, c := range map[string]struct {
		there docker.Container
		stop  bool
		want  string
	}{
		"another project's alias":     {there: other("env_x-api-1", map[string]string{docker.LabelProject: "env_x", docker.LabelService: "api"}, "api", "db"), want: "answer to db"},
		"another project's service":   {there: other("env_x-db-1", map[string]string{docker.LabelProject: "env_x", docker.LabelService: "db"}, "db"), want: "answer to db"},
		"this service's name":         {there: other("env_x-web-1", map[string]string{docker.LabelProject: "env_x", docker.LabelService: "web"}, "web"), want: "answer to web"},
		"a container started by hand": {there: other("db", nil), want: "answer to db"},
		"a name in another case":      {there: other("1Panel-DB", nil, "DB"), want: "answer to DB"},
		"one that isn't running":      {there: other("db", nil), stop: true, want: "answer to db"},
		"another name is no one's":    {there: other("cache", nil, "redis")},
	} {
		t.Run(name, func(t *testing.T) {
			e, l := shapesEngine(t)
			e.AddContainer(c.there)
			if c.stop {
				e.Stop(strings.TrimPrefix(c.there.Name, "/"))
			}
			_, err := l.Plan(t.Context(), []resource.Desired{{Ref: webRef(), Spec: spec()}})
			if c.want == "" {
				if err != nil {
					t.Errorf("Plan = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "the network edge") {
				t.Errorf("Plan = %v, want it refused, saying %s", err, c.want)
			}
		})
	}
	// Itself doesn't count, or it could never be replaced.
	_, l := shapesEngine(t)
	planAndApply(t, l, webRef(), spec())
	again := spec()
	again.GetApp().Image = "nginx:1.28"
	planAndApply(t, l, webRef(), again)
}

// Docker makes no room for what it can't change on a running container, and takes
// a limit of zero as leaving it as it is.
func TestWhatChangesInPlace(t *testing.T) {
	base := func(r *resourcev1.Resources) *resourcev1.Service {
		return svc(&resourcev1.Container{Resources: r, Restart: resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE})
	}
	limits := func(cpus, memory, reserve string) *resourcev1.Resources {
		return &resourcev1.Resources{Cpus: cpus, Memory: memory, MemoryReservation: reserve}
	}
	for name, c := range map[string]struct {
		from, to *resourcev1.Service
		action   resource.Action
	}{
		"from no limits":           {base(nil), base(limits("1", "1G", "")), resource.ActionUpdate},
		"a higher memory":          {base(limits("", "1G", "")), base(limits("", "4G", "")), resource.ActionUpdate},
		"a lower memory":           {base(limits("", "1G", "")), base(limits("", "512M", "")), resource.ActionUpdate},
		"more CPUs":                {base(limits("1", "", "")), base(limits("2", "", "")), resource.ActionUpdate},
		"another reservation":      {base(limits("", "1G", "128M")), base(limits("", "1G", "256M")), resource.ActionUpdate},
		"lifting the memory":       {base(limits("", "1G", "")), base(limits("", "", "")), resource.ActionReplace},
		"lifting the CPUs":         {base(limits("2", "", "")), base(limits("", "", "")), resource.ActionReplace},
		"lifting a reservation":    {base(limits("", "1G", "128M")), base(limits("", "1G", "")), resource.ActionReplace},
		"a limit, and a new image": {base(limits("", "1G", "")), svc(&resourcev1.Container{Image: "nginx:1.28", Resources: limits("", "2G", ""), Restart: resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE}), resource.ActionReplace},
	} {
		t.Run(name, func(t *testing.T) {
			e, l := shapesEngine(t)
			planAndApply(t, l, webRef(), c.from)
			id := e.Container(service.ContainerName(webRef())).ID
			step := planAndApply(t, l, webRef(), c.to)
			if step.Action != c.action {
				t.Fatalf("planned %v, want %v", step.Action, c.action)
			}
			now := e.Newest(docker.Project(webRef().Scope), webRef().Name)
			if (now.ID == id) != (c.action == resource.ActionUpdate) {
				t.Errorf("%v, and the container's ID went from %s to %s", c.action, id, now.ID)
			}
			if c.action == resource.ActionUpdate && step.Downtime != 0 {
				t.Errorf("changing it in place plans %v of downtime", step.Downtime)
			}
			got, err := l.Observe(t.Context(), []resource.Ref{webRef()})
			if err != nil || !proto.Equal(got[0], c.to) {
				t.Errorf("reads back as a different spec: %v", resource.Diff(c.to, got[0]))
			}
		})
	}
	// The count on-failure takes changes in place too.
	e, l := shapesEngine(t)
	planAndApply(t, l, webRef(), base(nil))
	id := e.Container(service.ContainerName(webRef())).ID
	counted := base(nil)
	counted.GetApp().RestartRetries = 3
	if step := planAndApply(t, l, webRef(), counted); step.Action != resource.ActionUpdate || e.Container(service.ContainerName(webRef())).ID != id {
		t.Errorf("a count of restarts planned %v", step.Action)
	}
}

// More CPUs than a server has is refused where that can be known, on the server.
func TestMoreCPUsThanTheServerHas(t *testing.T) {
	e, l := shapesEngine(t)
	e.NCPU = 2
	_, err := l.Plan(t.Context(), []resource.Desired{{Ref: webRef(), Spec: svc(&resourcev1.Container{Resources: &resourcev1.Resources{Cpus: "4"}})}})
	if err == nil || !strings.Contains(err.Error(), "limited to 4 CPUs, and this server has 2") {
		t.Errorf("Plan = %v", err)
	}
	planAndApply(t, l, webRef(), svc(&resourcev1.Container{Resources: &resourcev1.Resources{Cpus: "2"}}))
}

// An image the server has runs, wherever it came from. One it hasn't, that a pull
// can't get, is refused as it plans, and one it can't tell about isn't.
func TestAnImageAPullCantGet(t *testing.T) {
	spec := func(image string) *resourcev1.Service { return svc(&resourcev1.Container{Image: image}) }
	e, l := shapesEngine(t)
	e.AddLocalImage("acme/built-here:1", dockertest.Image{Config: docker.Config{Cmd: []string{"app"}}})
	planAndApply(t, l, webRef(), spec("acme/built-here:1"))
	if e.Pulls != 0 {
		t.Errorf("an image the server has was pulled, %d times", e.Pulls)
	}
	_, err := l.Plan(t.Context(), []resource.Desired{{Ref: resource.Ref{Kind: service.Kind, Scope: shapesEnv, Name: "api"}, Spec: spec("acme/no-such:1")}})
	if err == nil || !strings.Contains(err.Error(), "the image acme/no-such:1 isn't on this server, and its registry won't give it to a pull") {
		t.Errorf("an image no one has = %v", err)
	}
	// A registry that doesn't answer says nothing about the image.
	e.RegistryDown = true
	if _, err := l.Plan(t.Context(), []resource.Desired{{Ref: resource.Ref{Kind: service.Kind, Scope: shapesEnv, Name: "api"}, Spec: spec("acme/no-such:1")}}); err != nil {
		t.Errorf("planning with the registry down = %v", err)
	}
}

func TestLimitsTakeOneForm(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "0": "", "0.0": "", "0.000": "", "1": "1", "1.0": "1", "1.50": "1.5", "0.5": "0.5", ".5": "0.5", "2.125": "2.125", "0.01": "0.01",
	} {
		if got, err := container.CanonicalCPUs(in); err != nil || got != want {
			t.Errorf("CPUs %q = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"lots", "1.", ".", "-1", "1e3", "0.001", "0.009", "1.2345", "1,5", " 1"} {
		if got, err := container.CanonicalCPUs(in); err == nil {
			t.Errorf("CPUs %q = %q, want it refused", in, got)
		}
	}
	for in, want := range map[string]string{
		"": "", "0": "", "1024m": "1G", "1G": "1G", "1g": "1G", "1gb": "1G", "1GiB": "1G", "512M": "512M", "1536m": "1536M", "1.5g": "1536M",
		"6291456": "6M", "1048576": "1M", "2t": "2T", "1234": "1234", "1k": "1K",
	} {
		if got, err := container.CanonicalMemory(in); err != nil || got != want {
			t.Errorf("memory %q = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"lots", "1x", "g", "-1", "1.g", "1 2", "99999999999999999999", "9999999999t"} {
		if got, err := container.CanonicalMemory(in); err == nil {
			t.Errorf("memory %q = %q, want it refused", in, got)
		}
	}
}

// A letter past ASCII can be longer in lowercase, as Ⱥ is, or shorter, as the
// Kelvin sign is, so reading the unit from the lowercase cut the amount in the
// wrong place: Ⱥ panicked, and a write token got an internal error where it
// should get a refusal. The last check of #92 found it.
func TestAMemoryLimitPastASCIIIsRefused(t *testing.T) {
	for _, in := range []string{"Ⱥ", "1Ⱥ", "1\u023a", "İ", "1İB", "1\u212a", "1\u212aB", "５12M", "512\u00a0M"} {
		got, err := container.CanonicalMemory(in)
		if err == nil || !strings.Contains(err.Error(), "isn't an amount of memory") {
			t.Errorf("memory %q = %q, %v, want it refused", in, got, err)
		}
		spec := &resourcev1.Container{Image: "nginx:1.27", Resources: &resourcev1.Resources{Memory: in}}
		if err := container.Validate(spec); err == nil {
			t.Errorf("Validate of a limit of %q = nil, want it refused", in)
		}
		spec = &resourcev1.Container{Image: "nginx:1.27", Resources: &resourcev1.Resources{MemoryReservation: in}}
		if err := container.Validate(spec); err == nil {
			t.Errorf("Validate of a reservation of %q = nil, want it refused", in)
		}
	}
}

func TestWhatASpecTakes(t *testing.T) {
	for name, c := range map[string]struct {
		spec *resourcev1.Container
		want string
	}{
		"too little memory":            {&resourcev1.Container{Resources: &resourcev1.Resources{Memory: "1M"}}, "Give at least 6M"},
		"too little reserved":          {&resourcev1.Container{Resources: &resourcev1.Resources{MemoryReservation: "1M"}}, "Give at least 6M"},
		"more reserved than the limit": {&resourcev1.Container{Resources: &resourcev1.Resources{Memory: "64M", MemoryReservation: "128M"}}, "is more than the limit"},
		"a capability that isn't":      {&resourcev1.Container{CapAdd: []string{"MAGIC"}}, "isn't a Linux capability"},
		"dropping one that isn't":      {&resourcev1.Container{CapDrop: []string{"MAGIC"}}, "isn't a Linux capability"},
		"a group with a space":         {&resourcev1.Container{GroupAdd: []string{"a b"}}, "isn't a group"},
		"a count on another policy":    {&resourcev1.Container{Restart: resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS, RestartRetries: 2}, "only on-failure restarts a set number of times"},
		"host beside a network":        {&resourcev1.Container{HostNetwork: true, Networks: []string{"a"}}, "joins no others"},
		"host beside the server's":     {&resourcev1.Container{HostNetwork: true, ExternalNetworks: []string{"a"}}, "joins no others"},
		"host with ports":              {&resourcev1.Container{HostNetwork: true, Ports: []*resourcev1.PortMapping{{HostPort: 80, ContainerPort: 80}}}, "listens on the server's ports itself"},
		"names with no network":        {&resourcev1.Container{Aliases: []string{"a"}}, "nothing could reach it by them"},
		"one network twice":            {&resourcev1.Container{ExternalNetworks: []string{"a", "a"}}, "listed twice"},
		"both kinds of one name":       {&resourcev1.Container{Networks: []string{"a"}, ExternalNetworks: []string{"a"}}, "both as one of the environment's and as one the server has"},
		"one name twice":               {&resourcev1.Container{Networks: []string{"a"}, Aliases: []string{"x", "X"}}, "listed twice"},
		"an environment's network":     {&resourcev1.Container{ExternalNetworks: []string{"ENV_01ABC_back"}}, "has the form of an environment's"},
		"another form of a container":  {&resourcev1.Container{Networks: []string{"a"}, Aliases: []string{"env_01abc-web-1"}}, "has the form of a service's container"},
	} {
		t.Run(name, func(t *testing.T) {
			c.spec.Image = "nginx:1.27"
			if err := container.Validate(c.spec); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate = %v, want %q", err, c.want)
			}
		})
	}
	// None of it may come in numbers that cost a plan more than it should.
	many := make([]string, 65)
	for i := range many {
		many[i] = "n" + strings.Repeat("x", i)
	}
	for name, spec := range map[string]*resourcev1.Container{
		"networks": {Networks: many}, "the server's": {ExternalNetworks: many}, "names": {Networks: []string{"a"}, Aliases: many}, "groups": {GroupAdd: many},
	} {
		spec.Image = "nginx:1.27"
		if err := container.Validate(spec); err == nil || !strings.Contains(err.Error(), "Dockwell takes up to") {
			t.Errorf("65 %s = %v", name, err)
		}
	}
	// What means the same compares the same.
	a := &resourcev1.Container{Image: "nginx:1.27", CapAdd: []string{"cap_net_admin", "NET_ADMIN", "SYS_TIME"}, GroupAdd: []string{"2", "1", "1"}, Resources: &resourcev1.Resources{}}
	if err := container.Validate(a); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.GetCapAdd(), []string{"NET_ADMIN", "SYS_TIME"}) || !slices.Equal(a.GetGroupAdd(), []string{"1", "2"}) || a.GetResources() != nil {
		t.Errorf("in one form: %v", a)
	}
	// A limit an API call spells its own way keeps the form Docker's reads back as,
	// or it would read as drift for as long as it ran.
	spelled := &resourcev1.Container{Image: "nginx:1.27", Resources: &resourcev1.Resources{Cpus: "1.50", Memory: "1024m", MemoryReservation: "512mb"}}
	if err := container.Validate(spelled); err != nil {
		t.Fatal(err)
	}
	if r := spelled.GetResources(); r.GetCpus() != "1.5" || r.GetMemory() != "1G" || r.GetMemoryReservation() != "512M" {
		t.Errorf("limits as spelled = %v", r)
	}
	once := proto.Clone(a)
	if err := container.Validate(a); err != nil || !proto.Equal(a, once) {
		t.Errorf("Validate isn't idempotent: %v", resource.Diff(once, a))
	}
}

// What a person approves names what it adds, and drops nothing but what only takes
// away. A container on its own, whose networks are all Docker's, reaches the same.
func TestWhatIsHostAccess(t *testing.T) {
	d := &service.Driver{}
	access := func(c *resourcev1.Container) []resource.Access { return d.HostAccess(svc(c)) }
	if got := access(&resourcev1.Container{Networks: []string{"back"}, CapDrop: []string{"ALL"}, Aliases: []string{"db"}, Resources: &resourcev1.Resources{Memory: "1G"}, Restart: resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE, RestartRetries: 3}); len(got) != 0 {
		t.Errorf("what reaches nothing past Docker = %+v", got)
	}
	for name, c := range map[string]struct {
		spec *resourcev1.Container
		want resource.Access
	}{
		"a network the server has": {&resourcev1.Container{ExternalNetworks: []string{"edge"}}, resource.Access{Kind: resource.AccessExternalNetwork, Path: "edge"}},
		"names there":              {&resourcev1.Container{ExternalNetworks: []string{"edge"}, Aliases: []string{"a", "b"}}, resource.Access{Kind: resource.AccessExternalNetwork, Path: "edge", Detail: "a,b"}},
		"the server's network":     {&resourcev1.Container{HostNetwork: true}, resource.Access{Kind: resource.AccessNetwork}},
		"a capability":             {&resourcev1.Container{CapAdd: []string{"NET_ADMIN"}}, resource.Access{Kind: resource.AccessCapability, Path: "NET_ADMIN"}},
		"a capability like root":   {&resourcev1.Container{CapAdd: []string{"SYS_ADMIN"}}, resource.Access{Kind: resource.AccessCapability, Path: "SYS_ADMIN", SameAsRoot: true}},
		"every capability":         {&resourcev1.Container{CapAdd: []string{"ALL"}}, resource.Access{Kind: resource.AccessCapability, Path: "ALL", SameAsRoot: true}},
		"a group":                  {&resourcev1.Container{GroupAdd: []string{"999"}}, resource.Access{Kind: resource.AccessGroup, Path: "999"}},
		"docker's group":           {&resourcev1.Container{GroupAdd: []string{"docker"}}, resource.Access{Kind: resource.AccessGroup, Path: "docker", SameAsRoot: true}},
		"root's group":             {&resourcev1.Container{GroupAdd: []string{"0"}}, resource.Access{Kind: resource.AccessGroup, Path: "0", SameAsRoot: true}},
		"privileged":               {&resourcev1.Container{Privileged: true}, resource.Access{Kind: resource.AccessPrivileged, SameAsRoot: true}},
	} {
		if got := access(c.spec); !slices.Equal(got, []resource.Access{c.want}) {
			t.Errorf("%s = %+v, want %+v", name, got, c.want)
		}
	}
	// A swap keeps what was approved; another of anything is new.
	had := access(&resourcev1.Container{Image: "a:1", ExternalNetworks: []string{"edge"}, CapAdd: []string{"NET_ADMIN"}, GroupAdd: []string{"999"}})
	if got := resource.NewAccess(had, access(&resourcev1.Container{Image: "a:2", ExternalNetworks: []string{"edge"}, CapAdd: []string{"NET_ADMIN"}, GroupAdd: []string{"999"}})); len(got) != 0 {
		t.Errorf("a new image counts as new access: %+v", got)
	}
	for name, more := range map[string]*resourcev1.Container{
		"another network":    {ExternalNetworks: []string{"lan"}},
		"another name there": {ExternalNetworks: []string{"edge"}, Aliases: []string{"db"}},
		"another capability": {CapAdd: []string{"SYS_ADMIN"}},
		"another group":      {GroupAdd: []string{"988"}},
		"privileged":         {Privileged: true},
		"the server's":       {HostNetwork: true},
	} {
		if got := resource.NewAccess(had, access(more)); len(got) == 0 {
			t.Errorf("%s counts as nothing new, so a token applies it", name)
		}
	}
	// On its own, a network it names is one it didn't make.
	own := &container.Driver{}
	got := own.HostAccess(&resourcev1.Container{Image: "a:1", Networks: []string{"edge", "bridge"}, CapAdd: []string{"NET_ADMIN"}})
	if !slices.Contains(got, resource.Access{Kind: resource.AccessExternalNetwork, Path: "edge"}) || !slices.Contains(got, resource.Access{Kind: resource.AccessCapability, Path: "NET_ADMIN"}) || len(got) != 2 {
		t.Errorf("a container on its own = %+v", got)
	}
	for name, spec := range map[string]*resourcev1.Container{
		"external_networks": {ExternalNetworks: []string{"a"}}, "host_network": {HostNetwork: true}, "aliases": {Networks: []string{"a"}, Aliases: []string{"b"}},
	} {
		spec.Image = "nginx:1.27"
		if err := own.Validate(spec); err == nil {
			t.Errorf("a container on its own with %s passes", name)
		}
	}
}

// What the new fields hold is in a spec that is sealed at rest, with its
// environment, and none of it is a secret, so plans show it and agents' reports of
// it pass, as an image does. The environment beside them stays masked.
func TestTheNewFieldsAreSealedAndShown(t *testing.T) {
	if !resource.Sealed(&service.Driver{}) || !resource.Sealed(&container.Driver{}) {
		t.Fatal("a spec with a sensitive field isn't sealed whole")
	}
	md := (&resourcev1.Service{}).ProtoReflect().Descriptor()
	for _, path := range []string{
		"app.external_networks", "app.external_networks[0]", "app.host_network", "app.aliases", "app.aliases[0]",
		"app.resources", "app.resources.cpus", "app.resources.memory", "app.resources.memory_reservation",
		"app.cap_add", "app.cap_add[0]", "app.cap_drop", "app.cap_drop[0]", "app.group_add", "app.group_add[0]", "app.privileged", "app.restart_retries",
	} {
		if resource.SensitivePath(md, path) {
			t.Errorf("%s counts as sensitive, so a plan masks what a person approves", path)
		}
	}
	if !resource.SensitivePath(md, "app.env[KEY]") {
		t.Error("app.env counts as plain")
	}
	from := svc(&resourcev1.Container{Env: map[string]string{"KEY": "old-value"}})
	to := svc(&resourcev1.Container{Env: map[string]string{"KEY": "new-value"}, CapAdd: []string{"NET_ADMIN"}, Resources: &resourcev1.Resources{Memory: "1G"}})
	for _, c := range resource.Diff(from, to) {
		switch {
		case strings.HasPrefix(c.Path, "app.env"):
			if !c.Sensitive || c.From != "" || c.To != "" {
				t.Errorf("the environment isn't masked: %+v", c)
			}
		case c.Sensitive || c.To == "":
			t.Errorf("a new field is masked: %+v", c)
		}
	}
	// What a config hash takes in leaves the environment's values out as before.
	a, b := proto.CloneOf(from.GetApp()), proto.CloneOf(from.GetApp())
	b.Env["KEY"] = "another-value"
	if container.ConfigHash(a) != container.ConfigHash(b) {
		t.Error("the config hash depends on a value")
	}
	b.Aliases = []string{"db"}
	if container.ConfigHash(a) == container.ConfigHash(b) {
		t.Error("the config hash doesn't change with the spec")
	}
}

// Importing what was exported gives the same spec, as it's read from Docker too.
func TestRestoringKeepsTheNewFields(t *testing.T) {
	e, l := shapesEngine(t)
	spec := svc(&resourcev1.Container{ExternalNetworks: []string{"edge"}, Aliases: []string{"db"}, CapAdd: []string{"NET_ADMIN"}, Resources: &resourcev1.Resources{Memory: "64M"}})
	spec.Variables = map[string]string{"LOG_LEVEL": "LOG_LEVEL"}
	spec.Revisions = map[string]int64{"LOG_LEVEL": 3}
	ctx := resource.WithValues(t.Context(), map[string]string{"LOG_LEVEL": "debug"})
	steps, err := l.Plan(ctx, []resource.Desired{{Ref: webRef(), Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Apply(ctx, steps[0], spec); err != nil {
		t.Fatal(err)
	}
	c := e.Container(service.ContainerName(webRef()))
	if !slices.Contains(c.Config.Env, "LOG_LEVEL=debug") || c.HostConfig.Memory != 64<<20 || !slices.Equal(c.HostConfig.CapAdd, []string{"NET_ADMIN"}) {
		t.Errorf("a service with a variable lost what it runs with: %+v, %+v", c.Config.Env, c.HostConfig)
	}
	got, err := l.Observe(t.Context(), []resource.Ref{webRef()})
	if err != nil || !proto.Equal(got[0], spec) {
		t.Errorf("reads back as a different spec: %v, %v", resource.Diff(spec, got[0]), err)
	}
}
