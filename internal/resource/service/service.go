// SPDX-License-Identifier: AGPL-3.0-only

// Package service is the "service" kind: anything that runs in an
// environment, placed on servers. Each revision is deployed on the
// servers it is placed on, and the agent on each runs it.
//
// For now a service is an app from an image, which the driver runs as one
// container through the container driver's Runtime, named and labelled as
// Docker Compose names and labels a service's first container. Its
// environment is its Compose project. A Compose file becomes services,
// with volumes and networks of their own kinds.
//
// A service uses its environment's variables and secrets by reference
// (resource.Binder): the revision store pins each to its revision, and the
// driver builds the container's environment from the values the agent
// stream sent for the step, in memory (package binding).
package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// Kind is the service kind.
const Kind resource.Kind = "service"

// Driver manages services. On the control plane, where it only reads and
// checks specs, its Containers needs no Runtime.
type Driver struct {
	Containers container.Driver
	// Passwords, where set, keeps a database's first user's password the one its
	// container starts with, as a replace gives it a new one (database.FirstPasswords).
	Passwords Passwords
}

// Passwords changes a database's first user's password in what runs of a service, to
// the one app starts with, before app replaces it, and says how to put it back should
// what runs afterwards be the container from before. A nil undo means it changed
// nothing.
type Passwords interface {
	Change(ctx context.Context, project, service string, app *resourcev1.Container) (undo func(context.Context) error, err error)
}

// passwordsBack is how long putting a password back may take, after a rollout failed.
const passwordsBack = 30 * time.Second

var (
	_ resource.Driver   = (*Driver)(nil)
	_ resource.Binder   = (*Driver)(nil)
	_ resource.Rebinder = (*Driver)(nil)
	_ resource.Adopter  = (*Driver)(nil)
)

// Kind returns "service".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the environment.
func (*Driver) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "svc".
func (*Driver) IDPrefix() string { return id.Service }

// New returns an empty Service.
func (*Driver) New() proto.Message { return &resourcev1.Service{} }

// CheckName requires a DNS label, such as web or api-v2, since a service's
// name goes in its URL and in the name of its container.
func (*Driver) CheckName(name string) error { return resource.CheckLabel("service", name, "api-v2") }

// Validate requires something to run, and checks it as a container. It
// checks what the service uses, writes an empty name as the name the
// service sees, and drops a revision for a name it doesn't use, so specs
// that mean the same compare equal.
func (d *Driver) Validate(spec proto.Message) error {
	svc := spec.(*resourcev1.Service)
	app := svc.GetApp()
	if app == nil {
		return errors.New("a service needs something to run: an app from an image, such as nginx:1.27")
	}
	if _, ok := app.GetLabels()[binding.LabelKey]; ok {
		return fmt.Errorf("the label %s is Dockwell's own, which records what a service uses. Use another", binding.LabelKey)
	}
	for _, m := range []struct {
		what  string
		names map[string]string
	}{{"variable", svc.GetVariables()}, {"secret", svc.GetSecrets()}} {
		for as, name := range m.names {
			if err := variable.CheckName("environment variable", as); err != nil {
				return err
			}
			if name == "" {
				m.names[as] = as
			} else if err := variable.CheckName(m.what, name); err != nil {
				return err
			}
			if _, ok := app.GetEnv()[as]; ok {
				return fmt.Errorf("%s is set in the app's environment and from a %s too. Keep one", as, m.what)
			}
		}
	}
	for as := range svc.GetSecrets() {
		if _, ok := svc.GetVariables()[as]; ok {
			return fmt.Errorf("%s comes from a variable and a secret. Keep one", as)
		}
	}
	maps.DeleteFunc(svc.Revisions, func(as string, rev int64) bool {
		_, v := svc.GetVariables()[as]
		_, s := svc.GetSecrets()[as]
		return !v && !s || rev <= 0
	})
	if len(svc.Revisions) == 0 {
		svc.Revisions = nil
	}
	if err := container.Validate(app); err != nil {
		return err
	}
	return binding.CheckWords(app, func(name string) bool {
		_, inEnv := app.GetEnv()[name]
		_, v := svc.GetVariables()[name]
		_, s := svc.GetSecrets()[name]
		return inEnv || v || s
	})
}

// References are the volumes the service mounts and the networks it
// joins, in its environment.
func (*Driver) References(spec proto.Message) []resource.Reference {
	app := spec.(*resourcev1.Service).GetApp()
	var out []resource.Reference
	for _, m := range app.GetMounts() {
		if v := m.GetVolume(); v != "" && !m.GetExternal() {
			out = append(out, resource.Reference{Kind: volume.Kind, Name: v, Verb: "mount", ReadOnly: m.GetReadOnly()})
		}
	}
	for _, n := range app.GetNetworks() {
		out = append(out, resource.Reference{Kind: network.Kind, Name: n, Verb: "join"})
	}
	return out
}

// ReferenceKinds are volumes and networks.
func (*Driver) ReferenceKinds() []resource.Kind { return []resource.Kind{volume.Kind, network.Kind} }

// Answers are the names the service answers to on each of its networks: its own
// and any others its app takes.
func (*Driver) Answers(ref resource.Ref, spec proto.Message) []resource.Answer {
	return container.Answers(ref.Name, spec.(*resourcev1.Service).GetApp())
}

// KeptData names the volumes and server paths a service writes to.
func (*Driver) KeptData(spec proto.Message) []string {
	var out []string
	for _, m := range spec.(*resourcev1.Service).GetApp().GetMounts() {
		if !m.GetReadOnly() {
			out = append(out, cmp.Or(m.GetVolume(), m.GetHostPath()))
		}
	}
	return out
}

// KeepEnv puts the values of the app's own environment variables named into
// desired, from current, for an edit by a client that can't read them back, as
// no response carries them (resource.EnvKeeper).
func (*Driver) KeepEnv(current, desired proto.Message, names []string) (resource.Kept, error) {
	if len(names) == 0 {
		return resource.Kept{}, nil
	}
	cur, _ := current.(*resourcev1.Service)
	want, _ := desired.(*resourcev1.Service)
	app := want.GetApp()
	if cur == nil || app == nil {
		return resource.Kept{}, errors.New("it keeps environment variables, but runs no image to give them to. Leave keep_env out")
	}
	have := cur.GetApp().GetEnv()
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		v, ok := have[name]
		switch {
		case seen[name]:
			return resource.Kept{}, fmt.Errorf("keep_env names %s twice. Name it once", name)
		case !ok:
			return resource.Kept{}, fmt.Errorf("it has no environment variable %s of its own to keep. Read it again, and set %s or leave it out", name, name)
		}
		if _, set := app.GetEnv()[name]; set {
			return resource.Kept{}, fmt.Errorf("the edit both sets and keeps %s. Set it, or keep it", name)
		}
		seen[name] = true
		if app.Env == nil {
			app.Env = make(map[string]string, len(names))
		}
		app.Env[name] = v
	}
	words := "keeps the value of " + names[0] + " as it is, as nobody read it back"
	if len(names) > 1 {
		words = "keeps the values of " + human.List(names) + " as they are, as nobody read them back"
	}
	return resource.Kept{
		Reason: "keeps_env",
		Words:  words,
		Stays:  "the values it keeps go only to the servers that have them. Set them again in the edit to run it there",
	}, nil
}

// Target is where a service runs on a server: in its environment's
// Compose project, as that project's service of the same name, in a
// container named as Compose names a service's first, such as
// env_01kat…-web-1. Containers run by hand can't take names of that form
// (container.Driver.CheckName), so neither manages the other's.
func Target(ref resource.Ref) container.Target {
	p := docker.Project(ref.Scope)
	return container.Target{Name: docker.ContainerName(p, ref.Name), Project: p, Service: ref.Name, Scoped: true}
}

// TargetIn is Target for a call that says what people call the service's
// environment (resource.WithScopeNames): its containers are named after that,
// such as default.production.web-1, so docker ps says what each is, though
// its Compose project, and so its volumes and networks, stay the environment's
// ID. Dockwell finds a service's containers by their labels, so one named the
// old way goes on being its own until a deploy replaces it (ADR 0035).
func TargetIn(ctx context.Context, ref resource.Ref) container.Target {
	t := Target(ref)
	if name := resource.ScopeName(ctx, ref.Scope); container.IsScopeName(name) {
		t.Prefix = name
		t.Name = docker.ReadableName(name, ref.Name, 1)
	}
	return t
}

// ContainerName is the name of a service's container on a server.
func ContainerName(ref resource.Ref) string { return Target(ref).Name }

// Uses lists the variables and secrets the service uses.
func (*Driver) Uses(spec proto.Message) []resource.Use {
	return binding.Uses(spec.(*resourcev1.Service))
}

// Pin sets the revision of each variable and secret the service uses.
func (*Driver) Pin(spec proto.Message, revisions map[string]int) {
	svc := spec.(*resourcev1.Service)
	svc.Revisions = nil
	for _, u := range binding.Uses(svc) {
		if rev := revisions[u.As]; rev > 0 {
			if svc.Revisions == nil {
				svc.Revisions = map[string]int64{}
			}
			svc.Revisions[u.As] = int64(rev)
		}
	}
}

// Rebind moves each use of the variable or secret name, of kind from, to
// the one of kind to, under the same name the service sees it as.
func (*Driver) Rebind(spec proto.Message, name string, from, to resource.Kind) {
	svc := spec.(*resourcev1.Service)
	src, dst := usesOfKind(svc, from), usesOfKind(svc, to)
	if from == to || !known(from) || !known(to) {
		return
	}
	for as, n := range src {
		if n == name || n == "" && as == name {
			delete(src, as)
			if dst == nil {
				dst = map[string]string{}
			}
			dst[as] = name
		}
	}
	if len(src) == 0 {
		src = nil
	}
	setUses(svc, from, src)
	setUses(svc, to, dst)
}

func known(kind resource.Kind) bool {
	return kind == variable.VariableKind || kind == variable.SecretKind
}

// usesOfKind is the map of a service's uses of kind.
func usesOfKind(svc *resourcev1.Service, kind resource.Kind) map[string]string {
	if kind == variable.SecretKind {
		return svc.GetSecrets()
	}
	return svc.GetVariables()
}

// setUses sets the map of a service's uses of kind.
func setUses(svc *resourcev1.Service, kind resource.Kind, uses map[string]string) {
	if kind == variable.SecretKind {
		svc.Secrets = uses
	} else {
		svc.Variables = uses
	}
}

// Observe reads the service's container, and what it uses from the
// container's label. The values themselves never leave the driver.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	c, err := d.Containers.ObserveAt(ctx, TargetIn(ctx, ref))
	if err != nil || c == nil {
		return nil, err
	}
	return binding.Restore(c.(*resourcev1.Container))
}

// NotRunning says how the service's container isn't running, in a word.
func (d *Driver) NotRunning(ctx context.Context, ref resource.Ref) (string, error) {
	return d.Containers.NotRunningAt(ctx, TargetIn(ctx, ref))
}

// Plan judges the step as the container driver does for the app.
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	return d.Containers.Plan(app(actual), app(desired), inner(changes))
}

// Judge judges the step as the container driver does for the app, with
// what its server has.
func (d *Driver) Judge(ctx context.Context, ref resource.Ref, actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	return d.Containers.JudgeAt(ctx, TargetIn(ctx, ref), app(actual), planned(desired), inner(changes))
}

// CheckPlan refuses a port something else on the server holds, or another service
// in the plan wants, for a service about to start.
func (d *Driver) CheckPlan(ctx context.Context, want resource.Desired, all []resource.Desired) error {
	var planned []container.Planned
	for _, o := range all {
		switch o.Ref.Kind {
		case Kind:
			var c *resourcev1.Container
			if s, ok := o.Spec.(*resourcev1.Service); ok {
				c = s.GetApp()
			}
			t := TargetIn(ctx, o.Ref)
			planned = append(planned, container.Planned{Name: t.Name, Project: t.Project, Service: t.Service, Words: o.Ref.Name, Spec: c})
		case container.Kind:
			c, _ := o.Spec.(*resourcev1.Container)
			h := container.HostTarget(o.Ref)
			planned = append(planned, container.Planned{Name: h.Name, Project: h.Project, Service: h.Service, Words: o.Ref.Name, Spec: c})
		}
	}
	return d.Containers.CheckPortsAt(ctx, TargetIn(ctx, want.Ref), want.Spec.(*resourcev1.Service).GetApp(), planned)
}

// ImageOf is the image a service's app runs, as it is written.
func (*Driver) ImageOf(spec proto.Message) string {
	return spec.(*resourcev1.Service).GetApp().GetImage()
}

// UserOf is the user a service's app runs as, as it is written.
func (*Driver) UserOf(spec proto.Message) string {
	return spec.(*resourcev1.Service).GetApp().GetUser()
}

// WhatRuns is a copy of a service's spec without its app's limits.
func (*Driver) WhatRuns(spec proto.Message) proto.Message {
	s := proto.Clone(spec).(*resourcev1.Service)
	if app := s.GetApp(); app != nil {
		app.Resources = nil
	}
	return s
}

// SetImage changes the image a service's app runs.
func (*Driver) SetImage(spec proto.Message, image string) {
	if app := spec.(*resourcev1.Service).GetApp(); app != nil {
		app.Image = image
	}
}

// Leads says where the server paths the app mounts lead on its server.
func (d *Driver) Leads(ctx context.Context, ref resource.Ref, desired proto.Message) (map[string]string, error) {
	return d.Containers.LeadsAt(ctx, TargetIn(ctx, ref), desired.(*resourcev1.Service).GetApp())
}

// Survey says where every server path on the server leads (container.Docker.Survey).
func (d *Driver) Survey(ctx context.Context, paths []string) (resource.Surveyed, error) {
	return d.Containers.Survey(ctx, paths)
}

// LeadsMoved says which of the server paths a service's app mounts don't lead where
// they did when what runs now was made.
func (d *Driver) LeadsMoved(ctx context.Context, ref resource.Ref, desired proto.Message) ([]string, error) {
	return d.Containers.LeadsMovedAt(ctx, TargetIn(ctx, ref), desired.(*resourcev1.Service).GetApp())
}

// Check checks the app on its server, as the container driver does.
func (d *Driver) Check(ctx context.Context, ref resource.Ref, desired proto.Message) error {
	return d.Containers.CheckAt(ctx, TargetIn(ctx, ref), desired.(*resourcev1.Service).GetApp())
}

// HostAccess is what the app itself reaches on its server: the server paths it
// mounts, the networks it joins that aren't its environment's, the server's own
// network, and what it runs with beyond what Docker gives a container. The
// engine adds what its volumes and networks reach (References).
func (*Driver) HostAccess(spec proto.Message) []resource.Access {
	return container.ServiceAccess(spec.(*resourcev1.Service).GetApp())
}

// NamedAsText says what the app writes as ${NAME} as text, for what its environment
// or the service's uses has.
func (*Driver) NamedAsText(spec proto.Message) map[string][]string {
	svc := spec.(*resourcev1.Service)
	return binding.NamedAsText(svc.GetApp(), func(name string) bool {
		_, inEnv := svc.GetApp().GetEnv()[name]
		_, v := svc.GetVariables()[name]
		_, s := svc.GetSecrets()[name]
		return inEnv || v || s
	})
}

// Normalize orders the app, as Validate would.
func (*Driver) Normalize(spec proto.Message) {
	container.Normalize(spec.(*resourcev1.Service).GetApp())
}

// app is a service's app, or nil.
func app(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	return m.(*resourcev1.Service).GetApp()
}

// planned is the container a service runs as, as a plan knows it: with the label
// that says what it uses, and none of their values, as only a step's call brings
// them.
func planned(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	svc := m.(*resourcev1.Service)
	if svc.GetApp() == nil {
		return svc.GetApp()
	}
	return binding.App(svc, svc.GetApp().GetEnv())
}

// inner are changes to a service as changes to its app.
func inner(changes []resource.Change) []resource.Change {
	out := make([]resource.Change, len(changes))
	for i, c := range changes {
		c.Path = strings.TrimPrefix(c.Path, "app.")
		out[i] = c
	}
	return out
}

// Apply runs, updates or removes the service's container. Its
// environment holds the values of what the service uses, which the call
// brought (resource.WithValues), and a label records what they came from.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	step.Changes = inner(step.Changes)
	if desired == nil {
		return d.Containers.ApplyAt(ctx, TargetIn(ctx, step.Ref), step, nil)
	}
	svc := desired.(*resourcev1.Service)
	env, err := binding.Environment(ctx, svc)
	if err != nil {
		return err
	}
	app := binding.App(svc, env)
	t := TargetIn(ctx, step.Ref)
	var undo func(context.Context) error
	if d.Passwords != nil && step.Action == resource.ActionReplace {
		if undo, err = d.Passwords.Change(ctx, t.Project, t.Service, app); err != nil {
			return err
		}
	}
	err = d.Containers.ApplyAt(ctx, t, step, app)
	if err != nil && undo != nil {
		// What failed may have put the old container back, which starts with the old one.
		back, cancel := resource.Finishing(ctx, passwordsBack)
		defer cancel()
		if undoErr := undo(back); undoErr != nil {
			return fmt.Errorf("%w. %w", err, undoErr)
		}
	}
	return err
}

// Verify checks the service's container is healthy.
func (d *Driver) Verify(ctx context.Context, ref resource.Ref, _ proto.Message) error {
	return d.Containers.VerifyAt(ctx, TargetIn(ctx, ref))
}

// Settle says what is left of a service that reads back as its spec: what a
// rollout that stopped partway left, or a restart it hasn't had.
func (d *Driver) Settle(ctx context.Context, ref resource.Ref, desired proto.Message, restart string) (resource.Decision, bool, error) {
	return d.Containers.SettleAt(ctx, TargetIn(ctx, ref), planned(desired).(*resourcev1.Container), restart)
}

// ReadAdoption reads a container as the app of the service ref would be.
func (d *Driver) ReadAdoption(ctx context.Context, ref resource.Ref, containerID string) (resource.Adoption, error) {
	read, err := d.Containers.ReadAdoptionAt(ctx, TargetIn(ctx, ref), containerID)
	if err != nil {
		return resource.Adoption{}, err
	}
	read.Spec = &resourcev1.Service{Runs: &resourcev1.Service_App{App: read.Spec.(*resourcev1.Container)}}
	return read, nil
}

// Adopts says what taking the container on as the service takes.
func (d *Driver) Adopts(ctx context.Context, ref resource.Ref, containerID string, desired proto.Message) (resource.Decision, error) {
	return d.Containers.AdoptsAt(ctx, TargetIn(ctx, ref), containerID, desired.(*resourcev1.Service).GetApp())
}

// Ready says why services can't run now, as their containers can't.
func (d *Driver) Ready(ctx context.Context) error { return d.Containers.Ready(ctx) }

// ReadyKey is the containers' runtime's.
func (d *Driver) ReadyKey() any { return d.Containers.ReadyKey() }
