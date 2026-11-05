// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/health"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

const refValue = "hunter2hunter2"

// naming is a service whose command and health check name PW, which its own environment
// holds, as a Compose file with a value in a command makes.
func naming(image, value string) *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: image, Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
		Command:     []string{"redis-server", "--requirepass", "${PW}", "--note", "$${HOME}"},
		Env:         map[string]string{"PW": value},
		Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "redis-cli", "-a", "${PW}", "ping"}},
		References:  []string{"command", "healthcheck.test"},
	}}}
}

// inPlainSight lists what of a container holds value, as anything Dockwell reads back of it
// and hands on could: every label, and what its fields read as.
func inPlainSight(c *docker.Container, value string) []string {
	var out []string
	for k, v := range c.Config.Labels {
		if strings.Contains(v, value) || strings.Contains(k, value) {
			out = append(out, "label "+k)
		}
	}
	return out
}

// The value is in what Docker runs, and nowhere Dockwell goes by: it reads back as the
// name, so a plan sees no change, and no label, the config hash included, can say it.
func TestAContainerRunsTheValueAndReadsBackTheName(t *testing.T) {
	s := newStack(t)
	spec := naming("nginx:1.27", refValue)
	if step := s.deploy(t, webRef, proto.Clone(spec).(*resourcev1.Service)); step.Action != resource.ActionCreate {
		t.Fatalf("the first step is %s", step.Action)
	}
	c := s.engine.Newest(project, "web")
	if want := []string{"redis-server", "--requirepass", refValue, "--note", "${HOME}"}; !slices.Equal(c.Config.Cmd, want) {
		t.Errorf("Docker runs %q, want %q", c.Config.Cmd, want)
	}
	if want := []string{"CMD", "redis-cli", "-a", refValue, "ping"}; !slices.Equal(c.Config.Healthcheck.Test, want) {
		t.Errorf("Docker checks %q, want %q", c.Config.Healthcheck.Test, want)
	}
	if !slices.Contains(c.Config.Env, "PW="+refValue) {
		t.Errorf("its environment is %v", c.Config.Env)
	}
	if got := inPlainSight(c, refValue); len(got) > 0 {
		t.Errorf("these hold the value: %v", got)
	}
	got, err := s.local.Observe(t.Context(), []resource.Ref{webRef})
	if err != nil {
		t.Fatal(err)
	}
	read := got[0].(*resourcev1.Service)
	if want := []string{"redis-server", "--requirepass", "${PW}", "--note", "$${HOME}"}; !slices.Equal(read.GetApp().GetCommand(), want) {
		t.Errorf("it reads back as %q, want %q", read.GetApp().GetCommand(), want)
	}
	if want := []string{"CMD", "redis-cli", "-a", "${PW}", "ping"}; !slices.Equal(read.GetApp().GetHealthcheck().GetTest(), want) {
		t.Errorf("its check reads back as %q, want %q", read.GetApp().GetHealthcheck().GetTest(), want)
	}
	if !slices.Equal(read.GetApp().GetReferences(), []string{"command", "healthcheck.test"}) {
		t.Errorf("its references read back as %q", read.GetApp().GetReferences())
	}
	if step := s.plan(t, webRef, proto.Clone(spec).(*resourcev1.Service), ""); step.Action != resource.ActionNone || len(step.Changes) != 0 {
		t.Errorf("planning what runs takes %s, changing %+v", step.Action, step.Changes)
	}
}

// The config hash is a function of the names. Were it of the text, a label anyone with a
// container could read would check a guess at a short password offline.
func TestTheConfigHashDoesntDependOnTheValue(t *testing.T) {
	render := func(value string) string {
		spec := naming("nginx:1.27", value).GetApp()
		return container.Labels(container.Target{Name: "web", Project: "p", Service: "web", Scoped: true}, spec)[docker.LabelConfigHash]
	}
	if render("one-value") != render("another-value") {
		t.Error("the config hash changes with the value")
	}
}

// A plan compares names, never what a value comes to, so a literal guess at what a
// container runs with reads as a change whether it is right or not, and says the same.
func TestAGuessAtTheCommandReadsTheSameWhetherOrNotItIsRight(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, naming("nginx:1.27", refValue))
	guess := func(value string) resource.Step {
		spec := naming("nginx:1.27", refValue)
		spec.GetApp().Command = []string{"redis-server", "--requirepass", value, "--note", "${HOME}"}
		spec.GetApp().Healthcheck.Test = []string{"CMD", "redis-cli", "-a", value, "ping"}
		spec.GetApp().References = nil
		return s.plan(t, webRef, spec, "")
	}
	right, wrong := guess(refValue), guess("not-the-password")
	if right.Action != resource.ActionReplace || wrong.Action != right.Action {
		t.Errorf("the right guess takes %s, and a wrong one %s", right.Action, wrong.Action)
	}
	shape := func(step resource.Step) []string {
		var out []string
		for _, c := range step.Changes {
			out = append(out, c.Path)
			if strings.Contains(c.From, refValue) {
				t.Errorf("%s reads back from %q", c.Path, c.From)
			}
		}
		return out
	}
	if !slices.Equal(shape(right), shape(wrong)) {
		t.Errorf("the right guess changes %v, and a wrong one %v", shape(right), shape(wrong))
	}
}

// A new value is a change a plan says without saying it, and replacing the container
// writes the new one, as a rolling deploy starts its new container with it. The next
// plan sees nothing.
func TestANewValueReplacesTheContainer(t *testing.T) {
	s := newStack(t)
	s.deploy(t, webRef, naming("nginx:1.27", refValue))
	step := s.deploy(t, webRef, naming("nginx:1.27", "a-new-value-7"))
	if step.Action != resource.ActionReplace {
		t.Errorf("the step is %s", step.Action)
	}
	if len(step.Changes) != 1 || step.Changes[0].Path != "app.env[PW]" || !step.Changes[0].Sensitive {
		t.Errorf("it changes %+v", step.Changes)
	}
	c := s.engine.Newest(project, "web")
	if !slices.Contains(c.Config.Cmd, "a-new-value-7") || !slices.Contains(c.Config.Healthcheck.Test, "a-new-value-7") {
		t.Errorf("Docker runs %q, checking %q", c.Config.Cmd, c.Config.Healthcheck.Test)
	}
	if again := s.plan(t, webRef, naming("nginx:1.27", "a-new-value-7"), ""); again.Action != resource.ActionNone {
		t.Errorf("after it, a plan takes %s: %+v", again.Action, again.Changes)
	}
	if !slices.Equal(s.instances(), names(2)) {
		t.Errorf("Docker has %v, want %v", s.instances(), names(2))
	}
}

// A value that is a secret comes with the step's call, and goes no further: a plan, which
// never has one, still sees nothing to do, and a new image rolls with it.
func TestASecretInACommandComesWithTheCall(t *testing.T) {
	s := newStack(t)
	spec := func(image string) *resourcev1.Service {
		out := naming(image, "")
		out.GetApp().Env = nil
		out.Secrets = map[string]string{"PW": "DB_PASSWORD"}
		out.Revisions = map[string]int64{"PW": 3}
		return out
	}
	ctx := resource.WithValues(t.Context(), map[string]string{"PW": refValue})
	apply := func(image string) {
		t.Helper()
		step := s.plan(t, webRef, spec(image), "")
		if err := s.local.Apply(ctx, step, spec(image)); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	apply("nginx:1.27")
	c := s.engine.Newest(project, "web")
	if !slices.Contains(c.Config.Cmd, refValue) || !slices.Contains(c.Config.Healthcheck.Test, refValue) {
		t.Fatalf("Docker runs %q, checking %q", c.Config.Cmd, c.Config.Healthcheck.Test)
	}
	if got := inPlainSight(c, refValue); len(got) > 0 {
		t.Errorf("these hold the value: %v", got)
	}
	if got := c.Config.Labels[binding.LabelKey]; got != "PW=secret:DB_PASSWORD@3" {
		t.Errorf("its label says %q", got)
	}
	if step := s.plan(t, webRef, spec("nginx:1.27"), ""); step.Action != resource.ActionNone {
		t.Errorf("a plan with no value takes %s: %+v", step.Action, step.Changes)
	}
	got, err := s.local.Observe(t.Context(), []resource.Ref{webRef})
	if err != nil {
		t.Fatal(err)
	}
	if !resource.Equal(got[0], spec("nginx:1.27")) {
		t.Errorf("it reads back as %v", got[0])
	}
	// A rolling deploy starts its new container with the value.
	apply("nginx:1.28")
	c = s.engine.Newest(project, "web")
	if c.Config.Image != "nginx:1.28" || !slices.Contains(c.Config.Cmd, refValue) {
		t.Errorf("after a deploy Docker runs %s %q", c.Config.Image, c.Config.Cmd)
	}
	// With no value there is nothing to write, and it says which, not what.
	step := s.plan(t, webRef, spec("nginx:1.27"), "")
	err = s.local.Apply(t.Context(), step, spec("nginx:1.27"))
	if err == nil || !strings.Contains(err.Error(), "the control plane sent no value for PW") {
		t.Errorf("with no value: %v", err)
	}
}

// Docker quotes what it couldn't run, so a value in a command's first word is in its
// answer. That goes where anyone with the read scope reads, so it never says it, in the
// plain words or in the chain behind them, as Docker words it when it makes one and when
// it starts one.
func TestWhatDockerSaysOfACommandDoesntGiveTheValueAway(t *testing.T) {
	for _, c := range []struct {
		name, on, says, plain string
		reason                string
	}{
		{
			name: "as it starts", on: "/start",
			says:   `failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: error during container init: exec: "` + refValue + `": executable file not found in $PATH: unknown`,
			plain:  "The command ••• isn't in the image for web",
			reason: resource.ReasonCommand,
		},
		{
			name: "not executable", on: "/start",
			says:   `OCI runtime create failed: exec: "` + refValue + `": permission denied: unknown`,
			plain:  "The command ••• can't run in the image for web",
			reason: resource.ReasonCommand,
		},
		{
			name: "something else", on: "/start",
			says:  `something it hadn't said before about ` + refValue,
			plain: "web didn't start, and Dockwell doesn't recognize why. Docker says: something it hadn't said before about •••",
		},
		{
			name: "as it is made", on: "/containers/create",
			says:  `no such thing as ` + refValue,
			plain: "web didn't start, and Dockwell doesn't recognize why. Docker says: no such thing as •••",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStack(t)
			spec := naming("nginx:1.27", refValue)
			step := s.plan(t, webRef, spec, "")
			s.engine.Intercept(func(r *http.Request) error {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, c.on) {
					return errors.New(c.says)
				}
				return nil
			})
			err := s.local.Apply(t.Context(), step, spec)
			if err == nil {
				t.Fatal("it applied")
			}
			plain, detail := resource.Explain(err)
			for what, text := range map[string]string{"the plain words": plain, "the chain": detail, "the error": err.Error()} {
				if strings.Contains(text, refValue) {
					t.Errorf("%s hold the value: %s", what, text)
				}
			}
			if !strings.Contains(plain, c.plain) {
				t.Errorf("it says %q, want %q", plain, c.plain)
			}
			if got := resource.ReasonOf(err); got != c.reason {
				t.Errorf("the reason is %q, want %q", got, c.reason)
			}
		})
	}
}

// What a check prints goes to a change's error and the timeline, and a value now in a
// command is in the environment Clean masks by, whichever way a check is read.
func TestWhatACheckPrintsDoesntGiveTheValueAway(t *testing.T) {
	s := newStack(t)
	spec := naming("sick:1", refValue)
	step := s.plan(t, webRef, spec, "")
	err := s.local.Apply(t.Context(), step, spec)
	if err == nil {
		t.Fatal("a failing check applied")
	}
	plain, detail := resource.Explain(err)
	if !strings.Contains(plain, "fails its health check") || !strings.Contains(plain, "•••") {
		t.Errorf("it says %q", plain)
	}
	if strings.Contains(plain, refValue) || strings.Contains(detail, refValue) {
		t.Errorf("it holds the value: %q, %q", plain, detail)
	}

	// Once it runs, a monitor reads Docker's check the same way.
	fresh := newStack(t)
	fresh.deploy(t, webRef, naming("nginx:1.27", refValue))
	name := fresh.engine.Newest(project, "web").Name
	fresh.engine.SetHealth(strings.TrimPrefix(name, "/"), "unhealthy", "redis-cli -a "+refValue+" said no")
	m := &health.Monitor{Client: fresh.engine.Client(), Scan: 5 * time.Millisecond, Refresh: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	var got *dockwellv1.InstanceHealth
	for time.Now().Before(deadline) {
		for _, h := range m.Report() {
			if h.GetService() == "web" && h.GetLast() != nil {
				got = h
			}
		}
		if got != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("the monitor said nothing of it")
	}
	if out := got.GetLast().GetOutput(); strings.Contains(out, refValue) || !strings.Contains(out, "•••") {
		t.Errorf("the monitor says %q", out)
	}
}

// What can't be vouched for isn't passed on. A field that doesn't come to what its label
// says may hold anything, a value among it, as when docker compose ran an export with
// no value in its shell, or someone made the container by hand and kept the labels. So
// it reads as nothing a person can take for a command, shows as drift, and can't be kept.
func TestWhatDoesntComeToItsLabelIsntPassedOn(t *testing.T) {
	for name, c := range map[string]struct {
		edit       func(*docker.Config)
		command    []string
		test       []string
		references []string
	}{
		"another value in the command": {
			edit: func(cfg *docker.Config) {
				cfg.Cmd = []string{"redis-server", "--requirepass", "typed-by-hand-77", "--note", "${HOME}"}
			},
			command:    []string{binding.Unread},
			test:       []string{"CMD", "redis-cli", "-a", "${PW}", "ping"},
			references: []string{"command", "healthcheck.test"},
		},
		"another test": {
			edit: func(cfg *docker.Config) {
				cfg.Healthcheck = &docker.Healthcheck{Test: []string{"CMD", "typed-by-hand-77"}}
			},
			command:    []string{"redis-server", "--requirepass", "${PW}", "--note", "$${HOME}"},
			test:       []string{binding.Unread},
			references: []string{"command", "healthcheck.test"},
		},
		"no test at all": {
			edit:       func(cfg *docker.Config) { cfg.Healthcheck = nil },
			command:    []string{"redis-server", "--requirepass", "${PW}", "--note", "$${HOME}"},
			test:       []string{binding.Unread},
			references: []string{"command", "healthcheck.test"},
		},
		"its environment doesn't have the name": {
			edit: func(cfg *docker.Config) {
				cfg.Env = slices.DeleteFunc(slices.Clone(cfg.Env), func(kv string) bool { return strings.HasPrefix(kv, "PW=") })
			},
			command:    []string{binding.Unread},
			test:       []string{binding.Unread},
			references: []string{"command", "healthcheck.test"},
		},
		"a label Dockwell can't read": {
			edit: func(cfg *docker.Config) {
				cfg.Labels = maps.Clone(cfg.Labels)
				cfg.Labels[container.LabelReferences] = "{not json"
			},
			command:    []string{binding.Unread},
			test:       []string{binding.Unread},
			references: []string{"command", "entrypoint", "healthcheck.test"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStack(t)
			spec := naming("nginx:1.27", refValue)
			s.deploy(t, webRef, spec)
			before := s.engine.Newest(project, "web")
			s.engine.Edit(strings.TrimPrefix(before.Name, "/"), func(on *docker.Container) { c.edit(&on.Config) })
			got, err := s.local.Observe(t.Context(), []resource.Ref{webRef})
			if err != nil {
				t.Fatal(err)
			}
			app := got[0].(*resourcev1.Service).GetApp()
			if !slices.Equal(app.GetCommand(), c.command) {
				t.Errorf("command reads %q, want %q", app.GetCommand(), c.command)
			}
			if !slices.Equal(app.GetHealthcheck().GetTest(), c.test) {
				t.Errorf("test reads %q, want %q", app.GetHealthcheck().GetTest(), c.test)
			}
			if !slices.Equal(app.GetReferences(), c.references) {
				t.Errorf("references read %q, want %q", app.GetReferences(), c.references)
			}
			text := fmt.Sprint(app.GetCommand(), app.GetEntrypoint(), app.GetHealthcheck().GetTest())
			if strings.Contains(text, "typed-by-hand-77") || strings.Contains(text, refValue) {
				t.Errorf("it passes on %s", text)
			}
			if resource.Equal(got[0], spec) {
				t.Error("it reads as its spec, so it isn't drift")
			}
			if err := (&service.Driver{}).Validate(proto.Clone(got[0])); err == nil {
				t.Error("what it reads as could be kept as a spec")
			}
			step := s.plan(t, webRef, spec, "")
			if step.Action != resource.ActionReplace {
				t.Errorf("a plan takes %s", step.Action)
			}
			for _, ch := range step.Changes {
				if strings.Contains(ch.From+ch.To, "typed-by-hand-77") || strings.Contains(ch.From+ch.To, refValue) {
					t.Errorf("the plan shows %+v", ch)
				}
			}
		})
	}
}

// A service can name only what it has a value for, and its own spec is where that's
// checked, whether from a file or an edit. A secret's value can't be a name's way in.
func TestAServiceNamesOnlyWhatItHasAValueFor(t *testing.T) {
	d := &service.Driver{}
	ok := naming("nginx:1.27", refValue)
	if err := d.Validate(ok); err != nil {
		t.Fatal(err)
	}
	unknown := naming("nginx:1.27", refValue)
	unknown.GetApp().Env = nil
	if err := d.Validate(unknown); err == nil || !strings.Contains(err.Error(), "names PW, and the container has no value for it") {
		t.Errorf("a name with no value: %v", err)
	}
	used := naming("nginx:1.27", refValue)
	used.GetApp().Env = nil
	used.Secrets = map[string]string{"PW": "DB_PASSWORD"}
	if err := d.Validate(used); err != nil {
		t.Errorf("a name it uses: %v", err)
	}
	asked := naming("nginx:1.27", refValue)
	asked.GetApp().Labels = map[string]string{container.LabelReferences: "{}"}
	if err := d.Validate(asked); err == nil || !strings.Contains(err.Error(), "is Dockwell's own") {
		t.Errorf("the label itself: %v", err)
	}
	if err := (&container.Driver{}).Validate(&resourcev1.Container{Image: "nginx:1.27", Command: []string{"${PW}"}, References: []string{"command"}}); err == nil || !strings.Contains(err.Error(), "names PW") {
		t.Errorf("a container on its own, with no value: %v", err)
	}
	if err := (&container.Driver{}).Validate(&resourcev1.Container{Image: "nginx:1.27", Command: []string{"${PW}"}, Env: map[string]string{"PW": "x"}, References: []string{"command"}}); err != nil {
		t.Errorf("a container on its own, with one: %v", err)
	}
}

// An entrypoint names values as a command does, and reads back the same.
func TestAnEntrypointRunsTheValueToo(t *testing.T) {
	s := newStack(t)
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "nginx:1.27", Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
		Entrypoint: []string{"run", "--key=${PW}"},
		Env:        map[string]string{"PW": refValue},
		References: []string{"entrypoint"},
	}}}
	s.deploy(t, webRef, proto.Clone(spec).(*resourcev1.Service))
	c := s.engine.Newest(project, "web")
	if want := []string{"run", "--key=" + refValue}; !slices.Equal(c.Config.Entrypoint, want) {
		t.Errorf("Docker runs %q, want %q", c.Config.Entrypoint, want)
	}
	if step := s.plan(t, webRef, proto.Clone(spec).(*resourcev1.Service), ""); step.Action != resource.ActionNone {
		t.Errorf("planning what runs takes %s, changing %+v", step.Action, step.Changes)
	}
}

// A container that is there and stopped starts again as it was, so what Docker says of it
// comes from what it holds, as this step needn't have made it.
func TestStartingAStoppedContainerDoesntGiveTheValueAway(t *testing.T) {
	s := newStack(t)
	spec := naming("nginx:1.27", refValue)
	s.deploy(t, webRef, proto.Clone(spec).(*resourcev1.Service))
	name := strings.TrimPrefix(s.engine.Newest(project, "web").Name, "/")
	s.engine.Stop(name)
	steps, err := s.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: proto.Clone(spec), Run: true}})
	if err != nil {
		t.Fatal(err)
	}
	s.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start") {
			return errors.New(`OCI runtime create failed: exec: "` + refValue + `": executable file not found in $PATH: unknown`)
		}
		return nil
	})
	err = s.local.Apply(t.Context(), steps[0], spec)
	if err == nil {
		t.Fatalf("it started, as %+v", steps[0])
	}
	plain, detail := resource.Explain(err)
	if strings.Contains(plain, refValue) || strings.Contains(detail, refValue) || strings.Contains(err.Error(), refValue) {
		t.Errorf("it holds the value: %q, %q", plain, detail)
	}
	if !strings.Contains(plain, "The command ••• isn't in the image for web") {
		t.Errorf("it says %q", plain)
	}
}

// A stop-first deploy that can't put back what ran says what Docker said of that, which
// holds the value the old container was made with, and not the one this step brought.
func TestPuttingBackWhatRanDoesntGiveTheValueAway(t *testing.T) {
	spec := func(value string) *resourcev1.Service {
		out := naming("postgres:17", value)
		out.GetApp().Healthcheck = nil
		out.GetApp().References = []string{"command"}
		out.GetApp().Rollout = &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_STOP_FIRST}
		return out
	}
	const old = "the-old-value-in-the-container"
	s := newScene(t, spec(old))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cut := stopCutShort(s, cancel)
	s.engine.Intercept(func(r *http.Request) error {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start") {
			return errors.New(`exec: "` + old + `": executable file not found in $PATH`)
		}
		return cut(r)
	})
	next := spec(refValue)
	steps, err := s.first.local.Plan(t.Context(), []resource.Desired{{Ref: webRef, Spec: next}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.first.local.Apply(ctx, steps[0], next)
	if err == nil || !strings.Contains(err.Error(), "nothing of it may be running") {
		t.Fatalf("the step said %v, so this tests nothing", err)
	}
	plain, detail := resource.Explain(err)
	for what, text := range map[string]string{"the plain words": plain, "the chain": detail, "the error": err.Error()} {
		if strings.Contains(text, old) || strings.Contains(text, refValue) {
			t.Errorf("%s hold a value: %s", what, text)
		}
	}
}
