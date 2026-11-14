// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// askingProxy is the fake proxy, able to ask an agent who signed in.
type askingProxy struct{ *fakeProxy }

const askAt = "unix//run/dockwell/signin.sock"

func (askingProxy) SignInDial() string { return askAt }

// keptSignIns is what an agent keeps of the routes that sign people in.
type keptSignIns struct {
	mu   sync.Mutex
	kept map[string]*resourcev1.RouteSignIn
	at   map[string]string
}

func (k *keptSignIns) Put(id, domain, path string, s *resourcev1.RouteSignIn) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.kept == nil {
		k.kept, k.at = map[string]*resourcev1.RouteSignIn{}, map[string]string{}
	}
	k.kept[id], k.at[id] = proto.CloneOf(s), domain+path
	return nil
}

func (k *keptSignIns) Drop(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.kept, id)
}

func (k *keptSignIns) Get(id string) *resourcev1.RouteSignIn {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s, ok := k.kept[id]; ok {
		return proto.CloneOf(s)
	}
	return nil
}

func signingIn() *resourcev1.Route {
	r := spec("app.example.net", "/admin", "web", 80)
	r.Access = &resourcev1.RouteAccess{SignIn: &resourcev1.RouteSignIn{
		Provider: "ours", Emails: []string{"mira@example.com"},
		Filled: &resourcev1.RouteSignInProvider{Issuer: "https://auth.example.com/", ClientId: "dockwell", ClientSecret: "s3cret", Revision: 3},
	}}
	return r
}

// A route that signs people in has the proxy ask the agent, and the agent keeps who
// may sign in before the proxy asks; it reads back as the agent keeps it, so a
// route the agent has forgotten, as after it started again, reads as a difference.
// Taking sign-in off, or the route, forgets it (ADR 0087).
func TestARouteThatSignsInIsTheAgentsToAnswer(t *testing.T) {
	s := newSetup(t)
	keep := &keptSignIns{}
	s.Proxy, s.SignIns = askingProxy{s.proxy}, keep
	want := signingIn()
	s.apply(t, "admin", want)
	id := RouteID(routeRef("admin"))
	routes, _ := s.proxy.Routes(t.Context())
	if len(routes) != 1 || routes[0].SignIn != askAt {
		t.Fatalf("the proxy has %+v, want it to ask at %s", routes, askAt)
	}
	if got := keep.Get(id); !proto.Equal(got, want.GetAccess().GetSignIn()) || keep.at[id] != "app.example.net/admin" {
		t.Errorf("the agent keeps %v at %s", got, keep.at[id])
	}
	observed, err := s.Observe(t.Context(), routeRef("admin"))
	if err != nil || !proto.Equal(observed.GetAccess().GetSignIn(), want.GetAccess().GetSignIn()) {
		t.Errorf("it reads back as %v, %v", observed.GetAccess(), err)
	}

	s.SignIns = &keptSignIns{}
	observed, err = s.Observe(t.Context(), routeRef("admin"))
	if err != nil || observed.GetAccess().GetSignIn() == nil || proto.Equal(observed.GetAccess().GetSignIn(), want.GetAccess().GetSignIn()) {
		t.Errorf("forgotten by the agent, it reads back as %v, %v, want a difference", observed.GetAccess(), err)
	}
	s.SignIns = keep

	s.apply(t, "admin", spec("app.example.net", "/admin", "web", 80))
	if keep.Get(id) != nil {
		t.Error("taking sign-in off leaves it with the agent")
	}
	if routes, _ := s.proxy.Routes(t.Context()); routes[0].SignIn != "" {
		t.Error("taking sign-in off leaves the proxy asking")
	}
	s.apply(t, "admin", want)
	s.apply(t, "admin", nil)
	if keep.Get(id) != nil {
		t.Error("removing the route leaves its sign-in with the agent")
	}
}

// A server that can't ask its agent, and a route with nothing of its provider, are
// refused, as either would leave no one able to sign in.
func TestARouteThatCantSignAnyoneInIsRefused(t *testing.T) {
	s := newSetup(t)
	ref := routeRef("admin")
	effect, _, err := s.Judge(t.Context(), ref, signingIn(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(t.Context(), ref, signingIn(), effect); err == nil || !strings.Contains(err.Error(), "Update the agent") {
		t.Errorf("a server that can't ask its agent says %v", err)
	}
	s.Proxy, s.SignIns = askingProxy{s.proxy}, &keptSignIns{}
	bare := signingIn()
	bare.Access.SignIn.Filled = nil
	if err := s.Apply(t.Context(), ref, bare, effect); err == nil || !strings.Contains(err.Error(), "nothing of that provider") {
		t.Errorf("a route with nothing of its provider says %v", err)
	}
}

// Applying a route that signs people in writes the other routes of its host again,
// so each takes Dockwell's cookies out, as one written before every route did may
// not; routes of another host stay as they are (#598's review, M4).
func TestARouteThatSignsInRewritesItsHostsOtherRoutes(t *testing.T) {
	s := newSetup(t)
	s.Proxy, s.SignIns = askingProxy{s.proxy}, &keptSignIns{}
	s.apply(t, "home", spec("app.example.net", "", "web", 80))
	s.apply(t, "other", spec("other.example.net", "", "web", 80))
	s.proxy.log = nil
	s.apply(t, "admin", signingIn())
	if !slices.Contains(s.proxy.log, "put "+RouteID(routeRef("home"))) {
		t.Errorf("signing in on app.example.net did %v, want its public route written again", s.proxy.log)
	}
	if slices.Contains(s.proxy.log, "put "+RouteID(routeRef("other"))) {
		t.Errorf("signing in on app.example.net wrote another host's route: %v", s.proxy.log)
	}
}

// A route on the host edited by hand isn't written again, and a plan to sign in on
// that host names it, as it may pass the cookies on (#598's review, New 4).
func TestARouteEditedByHandBesideSignInIsNamed(t *testing.T) {
	s := newSetup(t)
	s.Proxy, s.SignIns = askingProxy{s.proxy}, &keptSignIns{}
	s.apply(t, "legacy", spec("app.example.net", "/legacy", "web", 80))
	s.apply(t, "home", spec("app.example.net", "", "web", 80))
	s.proxy.mu.Lock()
	for i := range s.proxy.routes {
		if s.proxy.routes[i].ID == RouteID(routeRef("legacy")) {
			s.proxy.routes[i].Edited = true
		}
	}
	s.proxy.mu.Unlock()
	if got := s.HandEditedBeside(t.Context(), routeRef("admin"), signingIn()); !slices.Equal(got, []string{"app.example.net/legacy"}) {
		t.Errorf("a plan to sign in names %v, want the route edited by hand", got)
	}
	if got := s.HandEditedBeside(t.Context(), routeRef("admin"), spec("app.example.net", "/admin", "web", 80)); got != nil {
		t.Errorf("a route that doesn't sign in names %v", got)
	}
	s.proxy.log = nil
	s.apply(t, "admin", signingIn())
	if slices.Contains(s.proxy.log, "put "+RouteID(routeRef("legacy"))) || !slices.Contains(s.proxy.log, "put "+RouteID(routeRef("home"))) {
		t.Errorf("signing in wrote %v, want the public route again and not the one edited by hand", s.proxy.log)
	}
}
