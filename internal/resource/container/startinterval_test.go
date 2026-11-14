// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// A health check's start_interval is a field of a spec: a container taken on with
// one keeps it, it's what Docker is asked for, and only a test takes one. Docker
// before 25 keeps none, so a plan for one on it says so, rather than drifting for
// good.
func TestAStartIntervalComesAlong(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("redis:8", dockertest.Image{Config: docker.Config{Cmd: []string{"redis-server"}}})
	c := e.Client()
	c.Adopted = &docker.Adopted{}
	d := &Docker{Client: c, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	id, err := c.CreateContainer(t.Context(), "legacy_cache", &docker.CreateRequest{Config: docker.Config{
		Image:       "redis:8",
		Healthcheck: &docker.Healthcheck{Test: []string{"CMD", "redis-cli", "ping"}, Interval: 30 * time.Second, StartPeriod: time.Minute, StartInterval: 2 * time.Second},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	read, err := d.ReadAdoption(t.Context(), taken, id)
	if err != nil {
		t.Fatal(err)
	}
	spec := read.Spec.(*resourcev1.Container)
	if len(read.Refused) != 0 || spec.GetHealthcheck().GetStartInterval() != "2s" || read.Invalid != "" {
		t.Errorf("read refuses %v, invalid %q, as %v", read.Refused, read.Invalid, spec.GetHealthcheck())
	}
	if got := healthcheck(spec.GetHealthcheck()); got.StartInterval != 2*time.Second {
		t.Errorf("Docker is asked for %+v", got)
	}

	for name, hc := range map[string]*resourcev1.Healthcheck{
		"an agent's check": {Tcp: &resourcev1.TcpCheck{Port: 6379}, StartInterval: "2s"},
		"none":             {Test: []string{"NONE"}, StartInterval: "2s"},
		"no duration":      {Test: []string{"CMD", "true"}, StartInterval: "soon"},
	} {
		if err := Validate(&resourcev1.Container{Image: "redis:8", Healthcheck: hc}); err == nil {
			t.Errorf("%s with a start_interval is valid", name)
		}
	}

	old := dockertest.New(t)
	old.APIVersion = "1.43"
	od := &Docker{Client: old.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	dec := &resource.Decision{Action: resource.ActionCreate}
	if err := od.Judge(t.Context(), taken, spec, dec); err == nil || !strings.Contains(err.Error(), "older than 25") {
		t.Errorf("planning a start_interval on Docker 24 = %v", err)
	}
}
