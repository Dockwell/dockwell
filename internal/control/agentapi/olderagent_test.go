// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// naming stands in for the keeper: what it lists names values on the server.
type naming struct {
	named []resource.Ref
	asked int
}

func (n *naming) NamingValues(_ context.Context, _ string, refs []resource.Ref) ([]resource.Ref, error) {
	n.asked++
	var out []resource.Ref
	for _, r := range refs {
		for _, m := range n.named {
			if r == m {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// An agent that doesn't say it writes references in reads what names values as
// Docker has it. So every way the control plane reads it is refused for what names
// one there, whatever spec it is asked about: reading it back, planning and applying.
// What names nothing goes, and an agent that says it writes them in is never asked.
func TestAnOlderAgentIsRefusedWhatNamesAValueAtEveryEntry(t *testing.T) {
	redis := resource.Ref{Kind: service.Kind, Scope: "env_1", Name: "redis"}
	web := resource.Ref{Kind: service.Kind, Scope: "env_1", Name: "web"}
	plain := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "redis:8", Command: []string{"redis-server"}}}}
	errSent := errors.New("the test stops the call here")
	open := func(writes bool, n *naming) *Session {
		a := NewAgents(resource.NewRegistry(&service.Driver{}), slog.New(slog.DiscardHandler))
		a.CheckNaming(n)
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(service.Kind)}, WritesReferences: writes}, func(*agentv1.ConnectResponse) error { return errSent })
	}
	entries := map[string]func(*Session, resource.Ref) error{
		"reading back": func(s *Session, ref resource.Ref) error {
			_, err := s.Observe(t.Context(), []resource.Ref{web, ref})
			return err
		},
		"planning": func(s *Session, ref resource.Ref) error {
			_, err := s.Plan(t.Context(), []resource.Desired{{Ref: ref, Spec: plain}})
			return err
		},
		"applying": func(s *Session, ref resource.Ref) error { return s.Apply(t.Context(), resource.Step{Ref: ref}, plain) },
	}
	for name, entry := range entries {
		t.Run(name, func(t *testing.T) {
			n := &naming{named: []resource.Ref{redis}}
			err := entry(open(false, n), redis)
			if !errors.Is(err, resource.ErrAgentTooOld) || !strings.Contains(err.Error(), "until you update the agent there") {
				t.Errorf("an older agent, with what names a value = %v", err)
			}
			if err := entry(open(false, n), web); errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an older agent, with what names nothing = %v", err)
			}
			current := &naming{named: []resource.Ref{redis}}
			if err := entry(open(true, current), redis); errors.Is(err, resource.ErrAgentTooOld) || current.asked != 0 {
				t.Errorf("an agent that writes references in = %v, asked %d times", err, current.asked)
			}
		})
	}
}

// With nothing wired to say what names values, an older agent is refused all of it,
// what names nothing included, rather than given it: a control plane that forgot
// CheckNaming fails closed. An agent that writes references in is never asked.
func TestAnOlderAgentIsRefusedEverythingWhenNothingCanSay(t *testing.T) {
	web := resource.Ref{Kind: service.Kind, Scope: "env_1", Name: "web"}
	plain := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "redis:8", Command: []string{"redis-server"}}}}
	errSent := errors.New("the test stops the call here")
	open := func(writes bool) *Session {
		a := NewAgents(resource.NewRegistry(&service.Driver{}), slog.New(slog.DiscardHandler))
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(service.Kind)}, WritesReferences: writes}, func(*agentv1.ConnectResponse) error { return errSent })
	}
	for name, entry := range map[string]func(*Session) error{
		"reading back": func(s *Session) error {
			_, err := s.Observe(t.Context(), []resource.Ref{web})
			return err
		},
		"planning": func(s *Session) error {
			_, err := s.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: plain}})
			return err
		},
		"applying": func(s *Session) error { return s.Apply(t.Context(), resource.Step{Ref: web}, plain) },
	} {
		if err := entry(open(false)); !errors.Is(err, resource.ErrAgentTooOld) || !strings.Contains(err.Error(), "Update the agent on hz-1") {
			t.Errorf("%s, an older agent with nothing to say what names values = %v", name, err)
		}
		// What goes out fails as the stream does, which a refusal comes before.
		if err := entry(open(true)); !errors.Is(err, resource.ErrNotConnected) {
			t.Errorf("%s, an agent that writes references in, with nothing to say what names values = %v", name, err)
		}
	}
}

// An agent that doesn't say it makes a mount's missing folder as its owner would
// make it root's, so a plan or a step that asks for one is refused there, with the
// fix; one that asks for nothing such goes, and an agent that says so takes it.
func TestAnOlderAgentIsRefusedAFolderToMake(t *testing.T) {
	web := resource.Ref{Kind: service.Kind, Scope: "env_1", Name: "web"}
	owned := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "node:24", Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/shop"}, Target: "/app", MakeOwner: "1000:1000"},
	}}}}
	plain := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "redis:8"}}}
	errSent := errors.New("the test stops the call here")
	open := func(makes bool) *Session {
		a := NewAgents(resource.NewRegistry(&service.Driver{}), slog.New(slog.DiscardHandler))
		a.CheckNaming(&naming{})
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(service.Kind)}, WritesReferences: true, MakesFolders: makes}, func(*agentv1.ConnectResponse) error { return errSent })
	}
	for name, entry := range map[string]func(*Session, *resourcev1.Service) error{
		"planning": func(s *Session, spec *resourcev1.Service) error {
			_, err := s.Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}})
			return err
		},
		"applying": func(s *Session, spec *resourcev1.Service) error {
			return s.Apply(t.Context(), resource.Step{Ref: web}, spec)
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := entry(open(false), owned)
			if !errors.Is(err, resource.ErrAgentTooOld) || !strings.Contains(err.Error(), "from before Dockwell made a missing folder as its owner, which web asks for, so it would be root's. Update the agent on hz-1") {
				t.Errorf("an older agent, with a folder to make = %v", err)
			}
			if err := entry(open(false), plain); errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an older agent, with no folder to make = %v", err)
			}
			if err := entry(open(true), owned); errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an agent that makes folders = %v", err)
			}
		})
	}
}

// An agent that doesn't say its proxy keeps strangers out would serve a private route
// to anyone, so a plan, a step or a desired state with one is refused there, with the
// fix; a public route goes, and an agent that says so takes it (ADR 0087).
func TestAnOlderAgentIsRefusedAPrivateRoute(t *testing.T) {
	www := resource.Ref{Kind: route.Kind, Scope: "env_1", Name: "www"}
	private := &resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 80, Access: &resourcev1.RouteAccess{Allow: []string{"203.0.113.0/24"}}}
	public := &resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 80}
	errSent := errors.New("the test stops the call here")
	open := func(keeps bool) *Session {
		a := NewAgents(resource.NewRegistry(&route.Driver{}), slog.New(slog.DiscardHandler))
		a.CheckNaming(&naming{})
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(route.Kind)}, WritesReferences: true, KeepsRoutesPrivate: keeps}, func(*agentv1.ConnectResponse) error { return errSent })
	}
	for name, entry := range map[string]func(*Session, *resourcev1.Route) error{
		"planning": func(s *Session, spec *resourcev1.Route) error {
			_, err := s.Plan(t.Context(), []resource.Desired{{Ref: www, Spec: spec}})
			return err
		},
		"applying": func(s *Session, spec *resourcev1.Route) error {
			return s.Apply(t.Context(), resource.Step{Ref: www}, spec)
		},
		"remembering": func(s *Session, spec *resourcev1.Route) error {
			_, err := s.Remember(t.Context(), www, spec, 1, "chg_1", "")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := entry(open(false), private)
			if !errors.Is(err, resource.ErrAgentTooOld) || !strings.Contains(err.Error(), "from before Dockwell kept strangers out of a route, which www asks for, so it would let anyone in. Update the agent on hz-1") {
				t.Errorf("an older agent, with a private route = %v", err)
			}
			if err := entry(open(false), public); errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an older agent, with a public route = %v", err)
			}
			if err := entry(open(true), private); errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an agent that keeps routes private = %v", err)
			}
		})
	}
}

// An agent that doesn't say it signs people in would leave a route that does with no
// one to answer its proxy, so a plan, a step or a desired state with one is refused
// there, with the fix, though the agent keeps routes private; an agent that says so
// takes it (ADR 0087).
func TestAnOlderAgentIsRefusedARouteThatSignsIn(t *testing.T) {
	www := resource.Ref{Kind: route.Kind, Scope: "env_1", Name: "www"}
	signsIn := &resourcev1.Route{Domain: "app.example.com", Service: "web", Port: 80, Access: &resourcev1.RouteAccess{SignIn: &resourcev1.RouteSignIn{
		Provider: "ours", Emails: []string{"mira@example.com"},
		Filled: &resourcev1.RouteSignInProvider{Issuer: "https://auth.example.com/", ClientId: "dockwell"},
	}}}
	errSent := errors.New("the test stops the call here")
	open := func(signs bool) *Session {
		a := NewAgents(resource.NewRegistry(&route.Driver{}), slog.New(slog.DiscardHandler))
		a.CheckNaming(&naming{})
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(route.Kind)}, WritesReferences: true, KeepsRoutesPrivate: true, SignsIn: signs}, func(*agentv1.ConnectResponse) error { return errSent })
	}
	for name, entry := range map[string]func(*Session) error{
		"planning": func(s *Session) error {
			_, err := s.Plan(t.Context(), []resource.Desired{{Ref: www, Spec: signsIn}})
			return err
		},
		"applying":    func(s *Session) error { return s.Apply(t.Context(), resource.Step{Ref: www}, signsIn) },
		"remembering": func(s *Session) error { _, err := s.Remember(t.Context(), www, signsIn, 1, "chg_1", ""); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := entry(open(false))
			if !errors.Is(err, resource.ErrAgentTooOld) || !strings.Contains(err.Error(), "from before Dockwell signed people in to a route with a provider, which www asks for. Update the agent on hz-1") {
				t.Errorf("an older agent, with a route that signs in = %v", err)
			}
			if err := entry(open(true)); errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an agent that signs people in = %v", err)
			}
		})
	}
}

// An agent that doesn't say it sets what a container asks of itself would leave a
// hostname, limits and the like out, so a plan or a step that asks for one is refused
// there, with the fix; one that asks for nothing such goes, and an agent that says so
// takes it (ADR 0085).
func TestAnOlderAgentIsRefusedWhatAContainerAsksOfItself(t *testing.T) {
	web := resource.Ref{Kind: service.Kind, Scope: "env_1", Name: "web"}
	plain := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "redis:8"}}}
	errSent := errors.New("the test stops the call here")
	open := func(sets bool) *Session {
		a := NewAgents(resource.NewRegistry(&service.Driver{}), slog.New(slog.DiscardHandler))
		a.CheckNaming(&naming{})
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(service.Kind)}, WritesReferences: true, OwnSettings: sets}, func(*agentv1.ConnectResponse) error { return errSent })
	}
	for field, app := range map[string]*resourcev1.Container{
		"hostname":          {Image: "redis:8", Hostname: "cache"},
		"init":              {Image: "redis:8", Init: true},
		"ulimits":           {Image: "redis:8", Ulimits: []*resourcev1.Ulimit{{Name: "nofile", Soft: 1024, Hard: 1024}}},
		"no_new_privileges": {Image: "redis:8", NoNewPrivileges: true},
		"dns":               {Image: "redis:8", Dns: []string{"10.0.0.53"}},
	} {
		asks := &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}
		t.Run(field, func(t *testing.T) {
			_, err := open(false).Plan(t.Context(), []resource.Desired{{Ref: web, Spec: asks}})
			if !errors.Is(err, resource.ErrAgentTooOld) || !strings.Contains(err.Error(), "from before Dockwell set a container's hostname, limits and the like, which web asks for") {
				t.Errorf("an older agent, planning = %v", err)
			}
			if err := open(false).Apply(t.Context(), resource.Step{Ref: web}, asks); !errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an older agent, applying = %v", err)
			}
			if _, err := open(true).Plan(t.Context(), []resource.Desired{{Ref: web, Spec: asks}}); errors.Is(err, resource.ErrAgentTooOld) {
				t.Errorf("an agent that sets them = %v", err)
			}
		})
	}
	if _, err := open(false).Plan(t.Context(), []resource.Desired{{Ref: web, Spec: plain}}); errors.Is(err, resource.ErrAgentTooOld) {
		t.Errorf("an older agent, with nothing asked = %v", err)
	}
}

// An agent that doesn't say it writes the files a container mounts is refused a spec
// that mounts one, with the fix, and an agent that says so takes it (ADR 0085).
func TestAnOlderAgentIsRefusedAFileToWrite(t *testing.T) {
	web := resource.Ref{Kind: service.Kind, Scope: "env_1", Name: "web"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{
		{Target: "/etc/nginx/nginx.conf", ReadOnly: true, Source: &resourcev1.Mount_File{File: &resourcev1.MountFile{Content: "events {}\n"}}},
	}}}}
	errSent := errors.New("the test stops the call here")
	open := func(writes bool) *Session {
		a := NewAgents(resource.NewRegistry(&service.Driver{}), slog.New(slog.DiscardHandler))
		a.CheckNaming(&naming{})
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(service.Kind)}, WritesReferences: true, WritesFiles: writes}, func(*agentv1.ConnectResponse) error { return errSent })
	}
	_, err := open(false).Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}})
	if !errors.Is(err, resource.ErrAgentTooOld) || !strings.Contains(err.Error(), "from before Dockwell wrote the files a container mounts, which web does") {
		t.Errorf("an older agent, planning = %v", err)
	}
	if err := open(false).Apply(t.Context(), resource.Step{Ref: web}, spec); !errors.Is(err, resource.ErrAgentTooOld) {
		t.Errorf("an older agent, applying = %v", err)
	}
	if _, err := open(true).Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}}); errors.Is(err, resource.ErrAgentTooOld) {
		t.Errorf("an agent that writes files = %v", err)
	}
}

// An agent that doesn't say it gives a volume its owner is refused a spec that
// names one, with the fix, and an agent that says so takes it (ADR 0085).
func TestAnOlderAgentIsRefusedAVolumesOwner(t *testing.T) {
	web := resource.Ref{Kind: service.Kind, Scope: "env_1", Name: "web"}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27", Mounts: []*resourcev1.Mount{
		{Target: "/data", Source: &resourcev1.Mount_Volume{Volume: "data"}, MakeOwner: "1000:1000"},
	}}}}
	errSent := errors.New("the test stops the call here")
	open := func(owns bool) *Session {
		a := NewAgents(resource.NewRegistry(&service.Driver{}), slog.New(slog.DiscardHandler))
		a.CheckNaming(&naming{})
		return a.open("srv_1", "hz-1", &agentv1.Hello{Kinds: []string{string(service.Kind)}, WritesReferences: true, MakesFolders: true, OwnsVolumes: owns}, func(*agentv1.ConnectResponse) error { return errSent })
	}
	_, err := open(false).Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}})
	if !errors.Is(err, resource.ErrAgentTooOld) || !strings.Contains(err.Error(), "from before Dockwell gave a new volume the owner its mount names, which web asks for") {
		t.Errorf("an older agent, planning = %v", err)
	}
	if err := open(false).Apply(t.Context(), resource.Step{Ref: web}, spec); !errors.Is(err, resource.ErrAgentTooOld) {
		t.Errorf("an older agent, applying = %v", err)
	}
	if _, err := open(true).Plan(t.Context(), []resource.Desired{{Ref: web, Spec: spec}}); errors.Is(err, resource.ErrAgentTooOld) {
		t.Errorf("an agent that gives volumes owners = %v", err)
	}
}
