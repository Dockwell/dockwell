// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
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

// With the last config gone and Caddy refusing the copy, the agent has nothing but
// that copy, and Caddy reads as a server with no routes. Stopping it would delete the
// copy, leaving nothing to try again with.
func TestAFailedRecoverLeavesCaddyAndItsCopyOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", backend(t, "web")))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()
	if _, err := c.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	good := c.readGood()
	if good == nil {
		t.Fatal("the agent has no copy, so this proves nothing")
	}
	c.proc.stop(syscall.SIGKILL)
	if err := os.Remove(c.autosaveFile()); err != nil {
		t.Fatal(err)
	}
	if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	waitAdmin(t, c)
	// A copy Caddy won't load, as a change of version could make one.
	refused := bytes.ReplaceAll(good, []byte(`"reverse_proxy"`), []byte(`"no_such_handler"`))
	if err := os.WriteFile(filepath.Join(c.Dir, goodFile), refused, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "active"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	m := &ingress.Manager{Client: dockertest.New(t).Client(), Proxy: c.Caddy, Dir: c.Dir, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "can't load Caddy's last good config") {
		t.Errorf("the log doesn't say what failed: %s", logs.String())
	}
	if st, err := c.Status(t.Context()); err != nil || !st.Running {
		t.Fatalf("after a failed Recover, Status = %+v, %v, want it running", st, err)
	}
	if !bytes.Equal(c.readGood(), refused) {
		t.Fatal("the agent's copy went")
	}
	// What was wrong is put right, and the next look brings the route back.
	if err := os.WriteFile(filepath.Join(c.Dir, goodFile), good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := c.whoAt(t, "app.example.net", "/who"); got["name"] != "web" {
		t.Errorf("after trying again, it serves %v", got)
	}
}

// While Caddy won't load the copy, a route's step has the same way to lose it. Taking
// away one route of two reads as the last going, which stops Caddy, and a new one
// writes over the copy. So neither happens, the step says why, and it works once the
// copy can be loaded, with the routes in the copy kept.
func TestARouteStepLeavesACopyCaddyWontLoadOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	web := backend(t, "web")
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))
	put(t, c, aRoute("dockwell.env.two", "two.example.net", "", web))
	_ = c.waitServing(t, "app.example.net", "/who").Body.Close()
	if _, err := c.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	good := c.readGood()
	if good == nil {
		t.Fatal("the agent has no copy, so this proves nothing")
	}
	c.proc.stop(syscall.SIGKILL)
	if err := os.Remove(c.autosaveFile()); err != nil {
		t.Fatal(err)
	}
	if err := c.proc.Run(t.Context(), filepath.Join(c.proc.Paths().Boot, bootstrapName)); err != nil {
		t.Fatal(err)
	}
	waitAdmin(t, c)
	refused := bytes.ReplaceAll(good, []byte(`"reverse_proxy"`), []byte(`"no_such_handler"`))
	if err := os.WriteFile(filepath.Join(c.Dir, goodFile), refused, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, "active"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := &ingress.Manager{Client: dockertest.New(t).Client(), Proxy: c.Caddy, Dir: c.Dir, Logger: slog.New(slog.DiscardHandler)}
	leftAlone := func(step string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "wasn't made") || !strings.Contains(err.Error(), "docker logs") {
			t.Errorf("%s = %v, want it to say it didn't, why, and what to do", step, err)
		}
		if st, err := c.Status(t.Context()); err != nil || !st.Running {
			t.Fatalf("after %s, Status = %+v, %v, want Caddy left running", step, st, err)
		}
		if !bytes.Equal(c.readGood(), refused) {
			t.Fatalf("%s changed the agent's copy", step)
		}
	}
	www := resource.Ref{Kind: "route", Scope: "env", Name: "www"}
	if id := ingress.RouteID(www); id != "dockwell.env.www" {
		t.Fatalf("that ref's route is %s, so this proves nothing", id)
	}
	leftAlone("removing one route of two", m.Apply(t.Context(), www, nil, nil))
	leftAlone("adding a route", c.PutRoute(t.Context(), aRoute("dockwell.env.new", "new.example.net", "", web)))

	if err := os.WriteFile(filepath.Join(c.Dir, goodFile), good, 0o600); err != nil {
		t.Fatal(err)
	}
	put(t, c, aRoute("dockwell.env.new", "new.example.net", "", web))
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Errorf("once the copy loads, the routes are %+v, want its two and the new one", routes)
	}
}

// A person replacing the list by hand takes the generation route with it, which
// doesn't make this a Caddy that lost everything: loading the copy would undo the edit,
// which is for a person to revert as drift.
func TestAListReplacedByHandIsntTakenForABareCaddyOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	web := backend(t, "web")
	put(t, c, aRoute("dockwell.env.www", "app.example.net", "", web))
	if _, err := c.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	edit := `[{"@id":"dockwell.env.www","match":[{"host":["app.example.net"]}],"handle":[{"handler":"static_response","body":"mine"}],"terminal":true}]`
	if _, _, err := c.call(t.Context(), http.MethodPatch, routesPath, []byte(edit), nil); err != nil {
		t.Fatalf("editing the list by hand: %v", err)
	}
	if recovered, err := c.Recover(t.Context()); err != nil || recovered {
		t.Errorf("Recover = %v, %v, want it left alone", recovered, err)
	}
	routes, err := c.Routes(t.Context())
	if err != nil || len(routes) != 1 || !routes[0].Edited {
		t.Errorf("Routes = %+v, %v, want the hand edit, reading as one", routes, err)
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
