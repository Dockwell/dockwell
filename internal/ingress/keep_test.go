// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// A network of the name that Dockwell didn't make takes someone making it by
// hand, and it can be on any subnet, the one the routes dial too. That is as bad
// as another network having that subnet, so the routes close the same way, and
// open again once it goes.
func TestANetworkOfTheNameThatIsntOursClosesTheRoutes(t *testing.T) {
	s := newSetup(t)
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	subnet := s.table.Subnet
	s.stopAndPrune(t, "web")
	s.engine.AddNetwork(docker.Network{Name: docker.IngressNetwork, Driver: "bridge", IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: subnet}}}})

	err := s.Reconcile(t.Context())
	if err == nil || !strings.Contains(err.Error(), "didn't make it") || !strings.Contains(err.Error(), "503") ||
		!strings.Contains(err.Error(), "docker network rm "+docker.IngressNetwork) {
		t.Fatalf("Reconcile = %v, want it to say whose the network isn't, what that costs, and the fix", err)
	}
	if !s.proxy.closed {
		t.Error("the routes still dial addresses on a network someone else made")
	}
	s.publishIngress(t.Context())
	if st := s.Ingress(); st.GetProblem() == dockwellv1.IngressProblem_INGRESS_PROBLEM_UNSPECIFIED || !strings.Contains(st.GetMessage(), "didn't make it") {
		t.Errorf("the API says %+v", st)
	}

	if err := s.Client.RemoveNetwork(t.Context(), docker.IngressNetwork); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.proxy.closed {
		t.Error("the routes stayed closed once the network went")
	}
	if n := s.engine.Network(docker.IngressNetwork); n == nil || n.Labels[docker.OwnPrefix+"ingress"] == "" {
		t.Errorf("the network is %+v, want Dockwell's own again", n)
	}
}

// Connections, not routes, take the proxy past its memory limit, and Docker
// starts it again, which drops every site for a moment. Nothing else says why, so
// the agent does, once for each time.
func TestARestartForMemoryIsLoggedOnceEachTime(t *testing.T) {
	s := newSetup(t)
	var logs bytes.Buffer
	s.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	s.apply(t, "a", spec("a.example.com", "", "web", 80))
	said := func() int { return strings.Count(logs.String(), "went past its memory limit") }

	s.proxy.status = Status{Exists: true, Running: true, Restarts: 1, OutOfMemory: true}
	for range 3 {
		if err := s.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := said(); n != 1 {
		t.Fatalf("one restart for memory was said %d times:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "Connections take it there") || !strings.Contains(logs.String(), "limit connections per client") {
		t.Errorf("the log doesn't say what takes it there, or what to do:\n%s", logs.String())
	}
	s.proxy.status.Restarts = 2
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := said(); n != 2 {
		t.Errorf("a second restart for memory makes %d in all, want 2", n)
	}
	// A restart for anything else isn't put down to memory.
	s.proxy.status = Status{Exists: true, Running: true, Restarts: 3}
	if err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := said(); n != 2 {
		t.Errorf("a restart that wasn't for memory was said as one: %d", n)
	}
}
