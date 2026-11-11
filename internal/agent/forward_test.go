// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"crypto/x509"
	"errors"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// A forward dials where Docker publishes the asked port of a service's running
// container: over TCP, on the address it's published on, loopback for one
// published on every address. A stopped container, a one-off run, a port in UDP
// and a port it doesn't publish give nothing to dial.
func TestAForwardDialsWhereDockerPublishesThePort(t *testing.T) {
	e := dockertest.New(t)
	add := func(name, service string, oneoff bool, running bool, bindings map[string][]docker.PortBinding) {
		c := docker.Container{Name: "/" + name, HostConfig: docker.HostConfig{PortBindings: bindings}}
		c.Config.Labels = map[string]string{
			docker.LabelProject: "default-production", docker.LabelService: service,
			docker.LabelOneoff: map[bool]string{true: "True", false: "False"}[oneoff],
		}
		if !running {
			c.State = docker.State{Status: "exited"}
		}
		e.AddContainer(c)
	}
	// A one-off run of web comes first, and publishes 80 elsewhere.
	add("web-run", "web", true, true, map[string][]docker.PortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "9001"}}})
	add("web", "web", false, true, map[string][]docker.PortBinding{
		"80/tcp": {{HostIP: "127.0.0.1", HostPort: "8083"}},
		"53/udp": {{HostIP: "127.0.0.1", HostPort: "5353"}},
	})
	add("every", "every", false, true, map[string][]docker.PortBinding{"80/tcp": {{HostPort: "8084"}}})
	add("six", "six", false, true, map[string][]docker.PortBinding{"80/tcp": {{HostIP: "::", HostPort: "8085"}}})
	add("old", "old", false, false, map[string][]docker.PortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "8086"}}})
	add("jobs", "jobs", true, true, map[string][]docker.PortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "8087"}}})

	client := e.Client()
	for _, tt := range []struct {
		service string
		port    uint32
		want    string
	}{
		{"web", 80, "127.0.0.1:8083"},
		{"every", 80, "127.0.0.1:8084"},
		{"six", 80, "[::1]:8085"},
		{"web", 53, ""},
		{"web", 443, ""},
		{"old", 80, ""},
		{"jobs", 80, ""},
		{"nobody", 80, ""},
	} {
		got, err := published(t.Context(), client, "default-production", tt.service, tt.port)
		switch {
		case tt.want != "" && (err != nil || got != tt.want):
			t.Errorf("%s's %d dials %q, %v, want %s", tt.service, tt.port, got, err, tt.want)
		case tt.want == "" && (err == nil || !strings.Contains(err.Error(), "publishes no port")):
			t.Errorf("%s's %d dials %q, %v, want nothing", tt.service, tt.port, got, err)
		}
	}
	if _, err := published(t.Context(), client, "another-project", "web", 80); err == nil {
		t.Error("web's port was found in a project it isn't in")
	}
}

// Forwards share a connection four at a time, so none waits on another's window,
// and an agent carries maxForwards at most, refusing the next with why. A
// connection goes once it carries none.
func TestForwardsShareConnectionsUpToTheirMost(t *testing.T) {
	id := &enroll.Identity{ControlURL: "https://control.example.com:7601", CA: &x509.Certificate{}}
	p := &forwardPool{}
	var gives []func()
	for range maxForwards {
		_, give, err := p.take(id)
		if err != nil {
			t.Fatal(err)
		}
		gives = append(gives, give)
	}
	if len(p.conns) != maxForwards/forwardsPerConn {
		t.Errorf("%d forwards went over %d connections, want %d", maxForwards, len(p.conns), maxForwards/forwardsPerConn)
	}
	if _, _, err := p.take(id); !errors.Is(err, errTooManyForwards) {
		t.Errorf("a forward past the most: %v", err)
	}
	gives[0]()
	gives[0]() // giving one back twice gives back one
	if _, give, err := p.take(id); err != nil {
		t.Errorf("a forward once one had ended: %v", err)
	} else {
		give()
	}
	for _, give := range gives {
		give()
	}
	if p.open != 0 || len(p.conns) != 0 {
		t.Errorf("with every forward ended, %d are open over %d connections", p.open, len(p.conns))
	}
}
