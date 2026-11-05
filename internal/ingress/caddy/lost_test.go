// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// With no last config at all, Caddy starts from its bootstrap, which serves
// nothing. Read as it is, that's a server with no routes, and the agent would
// stop it, so the copy goes back in instead.
func TestAMissingLastConfigComesBackOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()

	c.proc.stop(syscall.SIGKILL)
	if err := os.Remove(c.autosaveFile()); err != nil {
		t.Fatal(err)
	}
	if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	waitAdmin(t, c)
	if routes, err := c.Routes(t.Context()); err != nil || len(routes) != 0 {
		t.Fatalf("started from its bootstrap, Routes = %+v, %v, want none", routes, err)
	}
	recovered, err := c.Recover(t.Context())
	if err != nil || !recovered {
		t.Fatalf("Recover = %v, %v", recovered, err)
	}
	if got := c.whoAt(t, "app.example.net", "/who"); got["name"] != "web" {
		t.Errorf("after recovering, it serves %v", got)
	}
	if b := c.readGood(); b == nil || !strings.Contains(string(b), "app.example.net") {
		t.Errorf("the copy is %.60q, so the next look would lose the route", b)
	}
	if again, err := c.Recover(t.Context()); err != nil || again {
		t.Errorf("Recover again = %v, %v, want nothing to do", again, err)
	}
}

// A bootstrap is what a first route starts from, and a copy of nothing but that
// isn't something to load in its place.
func TestABootstrapWithNoRoutesToComeBackIsLeftAloneOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	if recovered, err := c.Recover(t.Context()); err != nil || recovered {
		t.Errorf("Recover on a Caddy started from its bootstrap, with nothing else to load, = %v, %v", recovered, err)
	}
}

// With Caddy running, a last config that has gone is put back, so that its next
// start, as Docker makes after it runs out of memory, isn't from the bootstrap.
func TestAMissingFileBehindARunningCaddyIsWrittenAgainOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()
	if err := os.Remove(c.autosaveFile()); err != nil {
		t.Fatal(err)
	}
	recovered, err := c.Recover(t.Context())
	if err != nil || !recovered {
		t.Fatalf("Recover = %v, %v", recovered, err)
	}
	b, missing, err := c.lastConfig()
	if err != nil || missing || !strings.Contains(string(b), "app.example.net") {
		t.Fatalf("the last config is %.60q, missing %v, %v", b, missing, err)
	}
	c.proc.stop(syscall.SIGKILL)
	if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	if got := c.whoAt(t, "app.example.net", "/who"); got["name"] != "web" {
		t.Errorf("after a restart, it serves %v", got)
	}
}
