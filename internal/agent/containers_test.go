// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// What a container holds in its labels goes nowhere: the fake's list has them, and
// a value in one doesn't get past this. Its environment and command aren't in the
// fake's list at all, so scripts/docker-e2e.sh holds those on real Docker. A list is
// also all it takes: reading it changes nothing, and asks Docker for nothing else.
func TestAListHoldsNoneOfWhatAContainerKeepsToItself(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("acme/shop:1", dockertest.Image{})
	e.AddContainer(docker.Container{
		ID: "a1", Name: "/shop",
		Config: docker.Config{
			Image: "acme/shop:1", Env: []string{"DB_PASSWORD=hunter2"}, Cmd: []string{"shop", "--token=hunter2"},
			Labels: map[string]string{"backup.password": "hunter2", docker.LabelProject: "shopstack", docker.LabelService: "shop", docker.LabelOneoff: "True"},
		},
	})
	// What loads a database copy is Dockwell's own, for a moment, and nobody's to look at.
	e.AddContainer(docker.Container{
		ID: "c1", Name: "/" + docker.CopyPrefix + "blog-1",
		Config: docker.Config{Image: "acme/shop:1", Labels: map[string]string{docker.LabelOneoff: "True", docker.LabelCopy: "blog"}},
	})
	found, err := containers(t.Context(), e.Client(), &agentv1.ContainersCall{})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := proto.Marshal(found)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "hunter2") {
		t.Errorf("the list carries a value a plan wouldn't show: %v", found)
	}
	if len(found.GetContainers()) != 1 {
		t.Fatalf("listed %v", found.GetContainers())
	}
	c := found.GetContainers()[0]
	if c.GetName() != "shop" || c.GetImage() != "acme/shop:1" || c.GetComposeProject() != "shopstack" || c.GetComposeService() != "shop" || !c.GetOneOff() ||
		c.GetState() != dockwellv1.ContainerState_CONTAINER_STATE_RUNNING {
		t.Errorf("listed %v", c)
	}
	for _, r := range e.Seen() {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("a list asked Docker to %s", r)
		}
		if strings.Contains(r, "/json") && !strings.HasSuffix(strings.SplitN(r, "?", 2)[0], "/containers/json") {
			t.Errorf("a list read more than the list: %s", r)
		}
	}
}

func TestAPortOnEveryAddressIsOnePort(t *testing.T) {
	got := publishedPorts([]docker.SummaryPort{
		{IP: "::", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
		{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
		{IP: "127.0.0.1", PrivatePort: 5432, PublicPort: 5432, Type: "tcp"},
		{PrivatePort: 9000, Type: "tcp"}, // exposed, and published nowhere
		{IP: "0.0.0.0", PrivatePort: 53, PublicPort: 53, Type: "udp"},
	})
	said := make([]string, len(got))
	for i, p := range got {
		said[i] = fmt.Sprintf("%s:%d->%d/%s", p.GetAddress(), p.GetServerPort(), p.GetContainerPort(), p.GetProtocol())
	}
	want := []string{":53->53/udp", "127.0.0.1:5432->5432/tcp", ":8080->80/tcp"}
	if !slices.Equal(said, want) {
		t.Errorf("ports = %v, want %v", said, want)
	}
}

func TestMountsSayWhatTheyAre(t *testing.T) {
	anon := strings.Repeat("ab", 32)
	got := mountsOf([]docker.MountPoint{
		{Type: "tmpfs", Destination: "/run"},
		{Type: "bind", Source: "/srv/app", Destination: "/app", RW: true},
		{Type: "volume", Name: "pgdata", Source: "/var/lib/docker/volumes/pgdata/_data", Destination: "/data", RW: false},
		{Type: "volume", Name: anon, Source: "/var/lib/docker/volumes/x/_data", Destination: "/cache", RW: true},
	})
	if len(got) != 3 {
		t.Fatalf("mounts = %v, want three, without the tmpfs", got)
	}
	if m := got[0]; m.GetTarget() != "/app" || m.GetHostPath() != "/srv/app" || m.GetVolume() != "" || m.GetReadOnly() {
		t.Errorf("a server path reads %v", m)
	}
	if m := got[1]; m.GetTarget() != "/cache" || m.GetVolume() != anon || !m.GetAnonymous() || m.GetHostPath() != "" {
		t.Errorf("an image's volume reads %v", m)
	}
	if m := got[2]; m.GetVolume() != "pgdata" || m.GetAnonymous() || !m.GetReadOnly() {
		t.Errorf("a volume reads %v", m)
	}
}

// A port range publishes a port for each number, and thousands of containers fill
// any message: the agent's stream carries stats too, and one message over its limit
// would end it. So a list stops short, and says by how much.
func TestAListStaysWithinWhatTheStreamCarries(t *testing.T) {
	e := dockertest.New(t)
	bindings := map[string][]docker.PortBinding{}
	for port := range 1000 {
		bindings[fmt.Sprintf("%d/tcp", 20000+port)] = []docker.PortBinding{{HostPort: strconv.Itoa(20000 + port)}}
	}
	e.AddContainer(docker.Container{ID: "b1", Name: "/ranges", HostConfig: docker.HostConfig{PortBindings: bindings}})
	long := strings.Repeat("x", 8192)
	const many = 200
	for i := range many {
		e.AddContainer(docker.Container{ID: fmt.Sprintf("c%04x", i), Name: fmt.Sprintf("/big-%d", i), Config: docker.Config{Image: long}})
	}
	found, err := containers(t.Context(), e.Client(), &agentv1.ContainersCall{})
	if err != nil {
		t.Fatal(err)
	}
	if size := proto.Size(found); size > listBytes+4096 {
		t.Errorf("the list came to %d bytes", size)
	}
	if listed, left := len(found.GetContainers()), int(found.GetUnlisted()); listed+left != many+1 || left == 0 {
		t.Errorf("listed %d and left out %d of %d", listed, left, many+1)
	}
	found, err = containers(t.Context(), e.Client(), &agentv1.ContainersCall{Container: "ranges"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.GetContainers()) != 1 {
		t.Fatalf("by name it listed %v", found.GetContainers())
	}
	c := found.GetContainers()[0]
	if len(c.GetPorts()) != listEach || c.GetMorePorts() != 1000-listEach {
		t.Errorf("a range of 1000 ports listed %d, and %d more", len(c.GetPorts()), c.GetMorePorts())
	}
}

// What runs a server's ingress is Dockwell's own, though no project says so, and a
// name is found exactly, as ADR 0021 asks: a name of hex digits isn't a prefix of
// an ID.
func TestAContainerIsFoundExactly(t *testing.T) {
	e := dockertest.New(t)
	e.AddContainer(docker.Container{ID: "cafe01", Name: "/web"})
	proxy := e.AddContainer(docker.Container{ID: "d0", Name: "/dockwell-ingress", Config: docker.Config{Labels: map[string]string{docker.LabelIngress: "caddy"}}})
	all, err := containers(t.Context(), e.Client(), &agentv1.ContainersCall{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.GetContainers()) != 1 || all.GetContainers()[0].GetName() != "web" || all.GetIngress() != 1 {
		t.Errorf("all of them: %v", all)
	}
	named, err := containers(t.Context(), e.Client(), &agentv1.ContainersCall{Container: "cafe"})
	if err != nil {
		t.Fatal(err)
	}
	if len(named.GetContainers()) != 0 {
		t.Errorf("a name that is a prefix of an ID found %v", named.GetContainers())
	}
	byID, err := containers(t.Context(), e.Client(), &agentv1.ContainersCall{Container: proxy.ID})
	if err != nil {
		t.Fatal(err)
	}
	if byID.GetIngress() != 1 {
		t.Errorf("by its ID, the proxy read %v", byID)
	}
}

func TestAnAgentWithoutDockerSaysSo(t *testing.T) {
	if _, err := containers(t.Context(), nil, &agentv1.ContainersCall{}); err == nil || !strings.Contains(err.Error(), "no Docker driver") {
		t.Errorf("err = %v", err)
	}
}
