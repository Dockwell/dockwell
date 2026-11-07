// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"errors"
	"os"
	"path/filepath"
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

// aServiceOnAFolder is a stack that follows server paths, and a service that mounts
// a folder, with what turns the folder into a link to /etc.
func aServiceOnAFolder(t *testing.T, order resourcev1.RolloutOrder) (s *stack, spec *resourcev1.Service, conf string, toEtc func()) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf = filepath.Join(dir, "conf")
	if err := os.Mkdir(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	s = newStack(t)
	s.rt.Paths = &container.Paths{Client: s.engine.Client(), DataDir: filepath.Join(dir, "dockwell")}
	spec = app("nginx:1.27")
	spec.GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: conf}, Target: "/conf"}}
	if order != resourcev1.RolloutOrder_ROLLOUT_ORDER_UNSPECIFIED {
		spec.GetApp().Rollout = &resourcev1.Rollout{Order: order}
	}
	return s, spec, conf, func() {
		t.Helper()
		if err := os.Remove(conf); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc", conf); err != nil {
			t.Fatal(err)
		}
	}
}

// A plan to start what's stopped was approved for where its paths led then. A link
// made since isn't what anyone approved, so it stays stopped, whichever way the
// rollout goes, and what was running goes on. Planned again, it says so, and what a
// person approves then does start.
func TestAStartChecksWhereItsPathsLead(t *testing.T) {
	for name, order := range map[string]resourcev1.RolloutOrder{
		"the new one first": resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST,
		"the old one first": resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST,
	} {
		t.Run(name, func(t *testing.T) {
			s, spec, conf, toEtc := aServiceOnAFolder(t, order)
			s.deploy(t, webRef, proto.Clone(spec).(*resourcev1.Service))
			c := s.engine.Newest(project, "web")
			container := strings.TrimPrefix(c.Name, "/")
			s.engine.Stop(container)
			plan := func() resource.Step {
				t.Helper()
				steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: proto.Clone(spec), Run: true}})
				if err != nil {
					t.Fatal(err)
				}
				return steps[0]
			}
			step := plan()
			if len(step.LeadsMoved) != 0 {
				t.Fatalf("nothing has moved, and it says %v", step.LeadsMoved)
			}
			toEtc()
			started := s.engine.Started(container)
			err := s.local.Apply(t.Context(), step, spec)
			if err == nil || !strings.Contains(err.Error(), conf+" leads to /etc now") || !strings.Contains(err.Error(), "it wasn't started") {
				t.Fatalf("a link made since its plan = %v", err)
			}
			if got := s.engine.Started(container); got != started {
				t.Errorf("it started %d times, want %d", got, started)
			}

			again := plan()
			if !slices.Equal(again.LeadsMoved, []string{conf}) {
				t.Fatalf("planned again it says %v moved", again.LeadsMoved)
			}
			if err := s.local.Apply(t.Context(), again, spec); err != nil {
				t.Fatalf("what a person approved: %v", err)
			}
			if got := s.engine.Newest(project, "web"); got == nil || !got.State.Running {
				t.Errorf("it is %+v, want it running", got)
			}
		})
	}
}

// The way back for what a rollout began to stop has no plan of its own for it, so it
// goes by the container's record, and a link made since keeps it down. Starting it
// would hand it /etc, which is worse than a service that's down and says why.
func TestAWayBackChecksWhereItsPathsLead(t *testing.T) {
	s, spec, conf, toEtc := aServiceOnAFolder(t, resourcev1.RolloutOrder_ROLLOUT_ORDER_UNSPECIFIED)
	s.deploy(t, webRef, proto.Clone(spec).(*resourcev1.Service))
	c := s.engine.Newest(project, "web")
	marked := "stopping_" + c.ID[:12] + "_" + strings.TrimPrefix(c.Name, "/")
	if err := s.engine.Client().RenameContainer(t.Context(), c.ID, marked); err != nil {
		t.Fatal(err)
	}
	s.engine.Stop(marked)
	steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: proto.Clone(spec), Run: true}})
	if err != nil {
		t.Fatal(err)
	}
	toEtc()
	started := s.engine.Started(marked)
	err = s.local.Apply(t.Context(), steps[0], spec)
	var moved *container.PathMovedError
	if !errors.As(err, &moved) || moved.Path != conf || moved.To != "/etc" {
		t.Fatalf("a way back over a link made since = %v", err)
	}
	if got := s.engine.Started(marked); got != started {
		t.Errorf("it started %d times, want %d", got, started)
	}
}

// A plan to take a container on says where its paths lead, as any plan does: a link to
// /etc reads as what it is, and what a person approves is what's kept.
func TestAPlanToTakeOnSaysWhereItsPathsLead(t *testing.T) {
	s, _, conf, toEtc := aServiceOnAFolder(t, resourcev1.RolloutOrder_ROLLOUT_ORDER_UNSPECIFIED)
	toEtc()
	s.engine.AddLocalImage("nginx:1.26", dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}})
	c := s.rt.Client
	c.Adopted = &docker.Adopted{}
	id, err := c.CreateContainer(t.Context(), "legacy_api", &docker.CreateRequest{
		Config: docker.Config{Image: "nginx:1.26"}, HostConfig: docker.HostConfig{Binds: []string{conf + ":/conf"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	d := &service.Driver{Containers: container.Driver{Runtime: s.rt}}
	read, err := d.ReadAdoption(t.Context(), webRef, id)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: read.Spec, Adopt: id}})
	if err != nil {
		t.Fatal(err)
	}
	if got := steps[0].Leads[conf]; got != "/etc" {
		t.Errorf("the plan says %s leads to %q, want /etc", conf, got)
	}
}
