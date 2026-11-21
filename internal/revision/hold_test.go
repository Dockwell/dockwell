// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"strings"
	"testing"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// An install that restarts Docker on a server and another change there don't run
// beside each other, either way round; a rollback isn't refused, another server
// isn't held, and the hold goes with the change (ADR 0064, Holds).
func TestAnInstallRestartingDockerHoldsItsServer(t *testing.T) {
	e := &Engine{}
	on := func(servers ...string) *Plan {
		p := &Plan{}
		for _, s := range servers {
			p.Servers = append(p.Servers, ServerPlan{Server: s, Name: strings.Replace(s, "srv_", "web-", 1)})
		}
		return p
	}
	install := func(server string, docker bool) *Plan {
		p := on(server)
		p.Resources = []ResourcePlan{{Ref: resource.Ref{Kind: "server.host", Scope: server, Name: "host"}, Install: &resource.Install{DockerRestarts: docker}}}
		return p
	}
	if err := e.holdServers("chg_install", install("srv_1", true), false, false); err != nil {
		t.Fatal(err)
	}
	err := e.holdServers("chg_deploy", on("srv_1"), false, false)
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "chg_install installs updates on web-1 now and restarts Docker there") {
		t.Errorf("a deploy while Docker restarts there: %v", err)
	}
	if err := e.holdServers("chg_elsewhere", on("srv_2"), false, false); err != nil {
		t.Errorf("a change on another server: %v", err)
	}
	if err := e.holdServers("chg_rollback", on("srv_1"), true, false); err != nil {
		t.Errorf("a rollback while Docker restarts: %v", err)
	}
	e.letGo(t.Context(), "chg_install")
	e.letGo(t.Context(), "chg_rollback")
	if err := e.holdServers("chg_deploy", on("srv_1"), false, false); err != nil {
		t.Errorf("a deploy once the install ended: %v", err)
	}

	// The other way round: an install that restarts Docker waits for what applies there.
	err = e.holdServers("chg_install2", install("srv_1", true), false, false)
	if !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "chg_deploy is applying on web-1 now, and this install restarts Docker there") {
		t.Errorf("an install restarting Docker beside a deploy: %v", err)
	}
	// One that leaves Docker alone doesn't.
	if err := e.holdServers("chg_install3", install("srv_1", false), false, false); err != nil {
		t.Errorf("an install that doesn't restart Docker beside a deploy: %v", err)
	}
}

func TestDockerCleanupHoldsDesiredStateOnItsServer(t *testing.T) {
	e := &Engine{waiters: map[string]bool{}}
	other := &Plan{Servers: []ServerPlan{{Server: "srv_cleanup"}}}
	cleanup := &Plan{
		Servers: other.Servers,
		Resources: []ResourcePlan{{
			Ref:               resource.Ref{Kind: "server.host", Scope: "srv_cleanup", Name: "host"},
			DockerMaintenance: &v1.DockerMaintenance{},
		}},
	}
	if err := e.holdServers("chg_deploy", other, false, true); err != nil {
		t.Fatal(err)
	}
	if err := e.holdServers("chg_cleanup", cleanup, false, true); !errors.Is(err, ErrBusy) || !e.waiters["chg_deploy"] {
		t.Fatalf("cleanup beside a resumed deploy: %v", err)
	}
	delete(e.waiters, "chg_deploy")
	e.letGo(t.Context(), "chg_deploy")
	if err := e.holdServers("chg_cleanup", cleanup, false, true); err != nil {
		t.Fatal(err)
	}
	if err := e.holdServers("chg_deploy", other, false, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("deploy beside resumed cleanup: %v", err)
	}
	e.letGo(t.Context(), "chg_cleanup")
	if err := e.holdServers("chg_deploy", other, false, false); err != nil {
		t.Fatalf("deploy after cleanup: %v", err)
	}
}
