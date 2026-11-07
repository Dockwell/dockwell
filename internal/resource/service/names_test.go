// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// named is a call that says what people call the environment, as the control
// plane's do.
func named(t *testing.T) context.Context {
	return resource.WithScopeNames(t.Context(), map[string]string{env: "default.production"})
}

// deployIn is deploy with ctx.
func (s *stack) deployIn(ctx context.Context, t *testing.T, spec *resourcev1.Service) resource.Step {
	t.Helper()
	steps, err := s.local.Plan(ctx, []resource.Desired{{Ref: webRef, Spec: spec}})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if steps[0].Action != resource.ActionNone {
		if err := s.local.Apply(ctx, steps[0], spec); err != nil {
			t.Fatalf("applying %s: %v", steps[0].Action, err)
		}
	}
	return steps[0]
}

func readable(numbers ...int) []string {
	out := make([]string, len(numbers))
	for i, n := range numbers {
		out[i] = docker.ReadableName("default.production", "web", n)
	}
	return out
}

// docker ps says what a service's container is: its project, environment and
// service, rather than the environment's ID. Its Compose project is still that
// ID, so docker compose -p finds it, and so do rollouts (#112).
func TestAServiceIsNamedAfterItsEnvironment(t *testing.T) {
	s := newStack(t)
	ctx := named(t)
	s.deployIn(ctx, t, app("nginx:1.27"))
	if got := s.instances(); !slices.Equal(got, []string{"default.production.web-1"}) {
		t.Fatalf("Docker has %v, want default.production.web-1", got)
	}
	c := s.engine.Container("default.production.web-1")
	if l := c.Config.Labels; l[docker.LabelProject] != project || l[docker.LabelService] != "web" || l[docker.LabelNumber] != "1" {
		t.Errorf("its labels are %v, want Compose's project %s", l, project)
	}
	list, err := s.engine.Client().ListContainers(t.Context(), dockertest.ComposeFilters(project, "web"))
	if err != nil || len(list) != 1 || list[0].ID != c.ID {
		t.Errorf("docker compose ps finds %+v, %v", list, err)
	}
	if step := s.deployIn(ctx, t, app("nginx:1.27")); step.Action != resource.ActionNone {
		t.Errorf("again, the plan is %s", step.Action)
	}
	s.deployIn(ctx, t, app("nginx:1.28"))
	if got := s.instances(); !slices.Equal(got, readable(2)) {
		t.Errorf("after a rolling deploy, Docker has %v, want %v", got, readable(2))
	}
	// A call that doesn't say finds it all the same, by its labels.
	if step := s.plan(t, webRef, app("nginx:1.28"), ""); step.Action != resource.ActionNone {
		t.Errorf("planned with no name for the environment, it's %s", step.Action)
	}
}

// A service running from before containers took their environment's name
// keeps running, and nothing replaces it for that: the next deploy that makes
// a container names it the new way (ADR 0035).
func TestAServiceNamedTheOldWayGoesOnUntilItsNextDeploy(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, app("nginx:1.27"))
	old := s.engine.Container(names(1)[0])
	if old == nil {
		t.Fatalf("Docker has %v", s.instances())
	}
	ctx := named(t)
	if step := s.deployIn(ctx, t, app("nginx:1.27")); step.Action != resource.ActionNone {
		t.Fatalf("its name alone makes the plan %s", step.Action)
	}
	if c := s.engine.Container(names(1)[0]); c == nil || c.ID != old.ID || !c.State.Running {
		t.Fatalf("what ran is %+v", c)
	}
	s.deployIn(ctx, t, app("nginx:1.28"))
	if got := s.instances(); !slices.Equal(got, readable(2)) {
		t.Errorf("after a rolling deploy, Docker has %v, want %v", got, readable(2))
	}

	// One that stops first takes the name of its number, the new way.
	s = newStack(t)
	stopFirst := func(image string) *resourcev1.Service {
		spec := app(image)
		spec.GetApp().Rollout = &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST}
		return spec
	}
	s.deploy(t, webRef, stopFirst("nginx:1.27"))
	s.deployIn(ctx, t, stopFirst("nginx:1.28"))
	if got := s.instances(); !slices.Equal(got, readable(1)) {
		t.Errorf("after a stop-first deploy, Docker has %v, want %v", got, readable(1))
	}
}

// A container run by hand can't take the form of a service's: the old one never,
// and the new one, <project>.<environment>.<service>-<n>, only when it's made
// anew, as checkName runs on every edit. Other names with dots are anyone's
// (ADR 0035).
func TestAContainerByHandCantTakeAServicesName(t *testing.T) {
	d := &container.Driver{}
	for name, ok := range map[string]bool{
		"env_01kat-web-1":          false,
		"default.production.web-1": true,
		"blog.example.com":         true,
		"my.app.v2":                true,
		"web-1":                    true,
	} {
		if err := d.CheckName(name); (err == nil) != ok {
			t.Errorf("CheckName(%q) = %v", name, err)
		}
	}
	for name, ok := range map[string]bool{
		"default.production.web-1":       false,
		"shop.staging.my-api-12":         false,
		"blog.example.com":               true,
		"my.app.v2":                      true,
		"com.example.web":                true,
		"nginx.1.27":                     true,
		"my.app":                         true,
		"Default.Production.web-1":       true,
		"default.production.web":         true,
		"default.production.web-1.local": true,
	} {
		if err := d.CheckNewName(name); (err == nil) != ok {
			t.Errorf("CheckNewName(%q) = %v", name, err)
		}
	}
	// Nor can another name of a service's, on a network others share, as Docker's
	// DNS would answer it with both.
	for name, ok := range map[string]bool{"default.production.web-1": false, "env_01kat-web-1": false, "db": true, "blog.example.com": true} {
		if err := container.CheckAlias(name); (err == nil) != ok {
			t.Errorf("CheckAlias(%q) = %v", name, err)
		}
	}
	if got := service.TargetIn(resource.WithScopeNames(t.Context(), map[string]string{env: "not a name"}), webRef).Name; got != names(1)[0] {
		t.Errorf("with a name that can't be a container's, the target is %s", got)
	}
}

// One change moves web off a port and gives it to api: what the plan takes off
// the port doesn't count (ADR 0026), whatever web's container is named, the way
// Compose names one, the readable way, or under a later number (#180's review).
func TestAPortTheSamePlanFreesIsFreeWhateverItsHolderIsNamed(t *testing.T) {
	on := func(image string, port uint32) *resourcev1.Service {
		spec := app(image)
		spec.GetApp().Ports = []*resourcev1.PortMapping{{HostPort: port, ContainerPort: 80}}
		return spec
	}
	api := resource.Ref{Kind: service.Kind, Scope: env, Name: "api"}
	for _, tt := range []struct {
		name    string
		deploys int
		named   bool
	}{
		{"named as Compose would, planned with no names", 1, false},
		{"named as Compose would, planned with names", 1, true},
		{"rolled to its second, planned with no names", 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newStack(t)
			s.deploy(t, webRef, on("nginx:1.27", 8080))
			if tt.deploys > 1 {
				s.deploy(t, webRef, on("nginx:1.28", 8080))
			}
			ctx := t.Context()
			if tt.named {
				ctx = named(t)
			}
			if _, err := s.local.Plan(ctx, []resource.Desired{{Ref: webRef, Spec: on("nginx:1.28", 8081)}, {Ref: api, Spec: on("nginx:1.27", 8080)}}); err != nil {
				t.Errorf("moving web off 8080 and api onto it, in one plan: %v", err)
			}
		})
	}
}

// A stop-first replace makes the new container under the name of its number,
// after the one it replaces, the readable way, whatever that one is named. A try
// that failed before leaves one there, which may be someone's, and the plan says
// so, rather than the step (#180's review).
func TestAStopFirstPlanFindsWhatAnEarlierTryLeft(t *testing.T) {
	s := newStack(t)
	stopFirst := func(image string) *resourcev1.Service {
		spec := app(image)
		spec.GetApp().Rollout = &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST}
		return spec
	}
	s.deploy(t, webRef, stopFirst("nginx:1.27")) // named as Compose would
	old := s.engine.Container(names(1)[0])
	left := old.ID[:12] + "_" + readable(1)[0]
	s.engine.AddContainer(docker.Container{Name: "/" + left, Config: docker.Config{Image: "by-hand"}})
	_, err := s.local.Plan(named(t), []resource.Desired{{Ref: webRef, Spec: stopFirst("nginx:1.28")}})
	if err == nil || !strings.Contains(err.Error(), "a container named "+left+" is on this server, but Dockwell didn't make it") {
		t.Errorf("planning a stop-first replace beside someone's %s = %v", left, err)
	}
}

// docker compose up makes a service's container under the name Compose gives
// one, env_…-web-1, as a rename does here. It's the service's all the same, by
// its labels: nothing plans a name back, and the next deploy names the next one
// the readable way (ADR 0035).
func TestAContainerComposeNamedStaysTheServices(t *testing.T) {
	for _, order := range []resourcev1.RolloutOrder{resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST, resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST} {
		t.Run(order.String(), func(t *testing.T) {
			s := newStack(t)
			ctx := named(t)
			spec := func(image string) *resourcev1.Service {
				sv := app(image)
				sv.GetApp().Rollout = &resourcev1.Rollout{Order: order}
				return sv
			}
			s.deployIn(ctx, t, spec("nginx:1.27"))
			made := s.engine.Container(readable(1)[0])
			s.engine.Edit(readable(1)[0], func(c *docker.Container) { c.Name = "/" + names(1)[0] })
			if step := s.deployIn(ctx, t, spec("nginx:1.27")); step.Action != resource.ActionNone {
				t.Fatalf("under Compose's name, the plan is %s", step.Action)
			}
			if got := s.instances(); !slices.Equal(got, names(1)) || s.engine.Container(names(1)[0]).ID != made.ID {
				t.Fatalf("Docker has %v, want %v, the container Compose named", got, names(1))
			}
			s.deployIn(ctx, t, spec("nginx:1.28"))
			want := readable(2)
			if order == resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST {
				want = readable(1)
			}
			if got := s.instances(); !slices.Equal(got, want) {
				t.Errorf("after the next deploy, Docker has %v, want %v", got, want)
			}
		})
	}
}
