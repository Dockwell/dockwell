// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"errors"
	"net/http"
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
// made since isn't what anyone approved, so nothing starts on it: made since the plan,
// the container isn't the one to start, and what would be made in its place is refused;
// made right before the start, the start is refused, and the container won't start by
// itself either. Planned again, the plan says so, and what a person approves makes it
// anew, with a record of where the path leads now, so the next plan doesn't ask again.
func TestAStartChecksWhereItsPathsLead(t *testing.T) {
	for name, tt := range map[string]struct {
		order resourcev1.RolloutOrder
		// point is where a link is made right before the start.
		point string
	}{
		"the new one first": {order: resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST, point: container.PointStarting},
		"the old one first": {order: resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST, point: container.PointSwapNamed},
	} {
		t.Run(name, func(t *testing.T) {
			s, spec, conf, toEtc := aServiceOnAFolder(t, tt.order)
			s.deploy(t, webRef, proto.Clone(spec).(*resourcev1.Service))
			c := s.engine.Newest(project, "web")
			named := strings.TrimPrefix(c.Name, "/")
			s.engine.Stop(named)
			plan := func() resource.Step {
				t.Helper()
				steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: proto.Clone(spec), Run: true}})
				if err != nil {
					t.Fatal(err)
				}
				return steps[0]
			}
			started := func() int { return s.engine.Started(named) }

			// Right before the start, after everything else was checked.
			step, was := plan(), started()
			s.rt.Points = func(at string) {
				if at == tt.point {
					toEtc()
				}
			}
			err := s.local.Apply(t.Context(), step, spec)
			s.rt.Points = nil
			if err == nil || !strings.Contains(err.Error(), conf+" leads to /etc now") || !strings.Contains(err.Error(), "won't start by itself") {
				t.Fatalf("a link made right before the start = %v", err)
			}
			if got := started(); got != was {
				t.Errorf("it started %d times, want %d", got, was)
			}
			if got := s.engine.Container(named); got != nil && got.HostConfig.RestartPolicy.Name != "no" {
				t.Errorf("it can start by itself: %q", got.HostConfig.RestartPolicy.Name)
			}

			// Planned again, it says so, and a person's approval makes it anew.
			again := plan()
			if !slices.Equal(again.LeadsMoved, []string{conf}) {
				t.Fatalf("planned again it says %v moved", again.LeadsMoved)
			}
			if err := s.local.Apply(t.Context(), again, spec); err != nil {
				t.Fatalf("what a person approved: %v", err)
			}
			now := s.engine.Newest(project, "web")
			if now == nil || !now.State.Running || now.ID == c.ID {
				t.Fatalf("it is %+v, want a new one running", now)
			}
			if !strings.Contains(now.Config.Labels[container.LabelLeads], `"/etc"`) {
				t.Errorf("its record is %q", now.Config.Labels[container.LabelLeads])
			}
			if moved := plan().LeadsMoved; len(moved) != 0 {
				t.Errorf("the plan after it says %v moved", moved)
			}
		})
	}
}

// A container's record can't change, so a start a person approved over a path that
// moved makes it anew, with a record of where the path leads now. Started as it was,
// it would keep its old record, and every plan after would ask again.
func TestAnApprovedStartOverAMovedPathMakesItAnew(t *testing.T) {
	s, spec, conf, toEtc := aServiceOnAFolder(t, resourcev1.RolloutOrder_ROLLOUT_ORDER_UNSPECIFIED)
	s.deploy(t, webRef, proto.Clone(spec).(*resourcev1.Service))
	c := s.engine.Newest(project, "web")
	s.engine.Stop(strings.TrimPrefix(c.Name, "/"))
	toEtc()
	plan := func() resource.Step {
		t.Helper()
		steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: proto.Clone(spec), Run: true}})
		if err != nil {
			t.Fatal(err)
		}
		return steps[0]
	}
	approved := plan()
	if !slices.Equal(approved.LeadsMoved, []string{conf}) {
		t.Fatalf("the plan says %v moved", approved.LeadsMoved)
	}
	if err := s.local.Apply(t.Context(), approved, spec); err != nil {
		t.Fatal(err)
	}
	now := s.engine.Newest(project, "web")
	if now == nil || now.ID == c.ID || !now.State.Running {
		t.Fatalf("it is %+v, want a new one running", now)
	}
	if moved := plan().LeadsMoved; len(moved) != 0 {
		t.Errorf("the plan after it says %v moved", moved)
	}
}

// A link made after the plan, before anything happens: the container isn't the one
// to start, and what would be made in its place is refused, so nothing starts.
func TestAPathMovedSinceThePlanStartsNothing(t *testing.T) {
	s, spec, conf, toEtc := aServiceOnAFolder(t, resourcev1.RolloutOrder_ROLLOUT_ORDER_UNSPECIFIED)
	s.deploy(t, webRef, proto.Clone(spec).(*resourcev1.Service))
	named := strings.TrimPrefix(s.engine.Newest(project, "web").Name, "/")
	s.engine.Stop(named)
	steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: proto.Clone(spec), Run: true}})
	if err != nil {
		t.Fatal(err)
	}
	toEtc()
	was := s.engine.Started(named)
	if err := s.local.Apply(t.Context(), steps[0], spec); err == nil || !strings.Contains(err.Error(), conf+" leads to /etc now") {
		t.Fatalf("a link made since its plan = %v", err)
	}
	if got := s.engine.Started(named); got != was {
		t.Errorf("it started %d times, want %d", got, was)
	}
	for _, name := range s.instances() {
		if c := s.engine.Container(name); c != nil && c.State.Running {
			t.Errorf("%s runs", name)
		}
	}
}

// The way back for what a rollout began to stop has no plan of its own for it, so it
// goes by the container's record, and by where the step at hand was approved for. A
// link made while it stops keeps it down, and from starting by itself; one the step
// approved doesn't.
func TestAWayBackChecksWhereItsPathsLead(t *testing.T) {
	for name, approved := range map[string]bool{"a link made while it stops": false, "a link the step approved": true} {
		t.Run(name, func(t *testing.T) {
			s, spec, conf, toEtc := aServiceOnAFolder(t, resourcev1.RolloutOrder_ROLLOUT_ORDER_UNSPECIFIED)
			s.deploy(t, webRef, proto.Clone(spec).(*resourcev1.Service))
			c := s.engine.Newest(project, "web")
			marked := "stopping_" + c.ID[:12] + "_" + strings.TrimPrefix(c.Name, "/")
			if err := s.engine.Client().RenameContainer(t.Context(), c.ID, marked); err != nil {
				t.Fatal(err)
			}
			if approved {
				toEtc()
			}
			steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: proto.Clone(spec), Run: true}})
			if err != nil {
				t.Fatal(err)
			}
			if !approved {
				s.engine.Intercept(func(r *http.Request) error {
					if strings.HasSuffix(r.URL.Path, "/stop") {
						toEtc()
					}
					return nil
				})
			}
			was := s.engine.Started(marked)
			err = s.local.Apply(t.Context(), steps[0], spec)
			s.engine.Intercept(nil)
			if approved {
				if err != nil {
					t.Fatalf("a way back over what the step approved: %v", err)
				}
				return
			}
			var moved *container.PathMovedError
			if !errors.As(err, &moved) || moved.Path != conf || moved.To != "/etc" {
				t.Fatalf("a way back over a link made since = %v", err)
			}
			if got := s.engine.Started(marked); got != was {
				t.Errorf("it started %d times, want %d", got, was)
			}
			if got := s.engine.Container(marked); got == nil || got.HostConfig.RestartPolicy.Name != "no" {
				t.Errorf("it can start by itself: %+v", got)
			}
		})
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

// When a deploy can't stop what ran, it puts that back, and the way back checks where
// its paths lead as any start does. What it says then is what to do about the path:
// docker start would follow it.
func TestPuttingBackWhatRanSaysWhatToDoAboutAPath(t *testing.T) {
	s, spec, conf, toEtc := aServiceOnAFolder(t, resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST)
	s.deploy(t, webRef, proto.Clone(spec).(*resourcev1.Service))
	old := s.engine.Newest(project, "web")
	next := proto.Clone(spec).(*resourcev1.Service)
	next.GetApp().Image = "nginx:1.28"
	step := s.plan(t, webRef, next, "")
	stops := 0
	s.engine.Intercept(func(r *http.Request) error {
		if !strings.HasSuffix(r.URL.Path, "/stop") {
			return nil
		}
		stops++
		if stops == 1 {
			toEtc()
			return errors.New("the engine is busy")
		}
		return nil
	})
	err := s.local.Apply(t.Context(), step, next)
	s.engine.Intercept(nil)
	var moved *container.PathMovedError
	if !errors.As(err, &moved) || moved.Path != conf {
		t.Fatalf("putting back what ran over a link made meanwhile = %v", err)
	}
	if strings.Contains(err.Error(), "docker start") {
		t.Errorf("it says to start it by hand: %v", err)
	}
	if got := s.engine.Container(strings.TrimPrefix(old.Name, "/")); got != nil && got.State.Running {
		t.Error("what ran was started over the link")
	}
}
