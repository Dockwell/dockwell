// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/ingress"
)

// A link planted in Caddy's directories doesn't get the agent writing where Caddy
// can't: nothing there is to be trusted.
func TestAGenerationFileFollowsNoLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "precious")
	if err := os.WriteFile(target, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, generationPrefix+"planted")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := touchNew(link); err == nil {
		t.Error("it wrote through a link")
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "mine" {
		t.Errorf("what the link names holds %q, %v", b, err)
	}
	made := filepath.Join(dir, generationPrefix+"new")
	if err := touchNew(made); err != nil {
		t.Errorf("a new name = %v", err)
	}
	if err := touchNew(made); err == nil {
		t.Error("it took a file that was already there")
	}
}

// What is in flight to an upstream is what the kernel says is open to it, and only
// that: the address and port it dials, in the state that means open.
func TestInFlightCountsWhatIsOpenToAnUpstream(t *testing.T) {
	table := filepath.Join(t.TempDir(), "tcp")
	if err := os.WriteFile(table, []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:8AE0 02001EAC:1F90 01 00000000:00000000 00:00000000 00000000     0        0 1 1 0 20 0 0 0 10 -1
   1: 0100007F:8AE1 02001EAC:1F90 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0 20 0 0 0 10 -1
   2: 0100007F:8AE2 02001EAC:1F90 06 00000000:00000000 00:00000000 00000000     0        0 3 1 0 20 0 0 0 10 -1
   3: 0100007F:8AE3 03001EAC:1F90 01 00000000:00000000 00:00000000 00000000     0        0 4 1 0 20 0 0 0 10 -1
   4: 02001EAC:1F90 0100007F:8AE4 01 00000000:00000000 00:00000000 00000000     0        0 5 1 0 20 0 0 0 10 -1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Caddy{Dir: t.TempDir(), Runtime: &Container{}, procNetTCP: table}
	if n, err := c.InFlight(t.Context(), "172.30.0.2:8080"); n != 2 || err != nil {
		t.Errorf("InFlight = %d, %v, want the two open to it", n, err)
	}
	if n, err := c.InFlight(t.Context(), "172.30.0.9:8080"); n != 0 || err != nil {
		t.Errorf("InFlight of what nothing is open to = %d, %v", n, err)
	}
	if n, err := c.InFlight(t.Context(), "172.30.0.2:9090"); n != 0 || err != nil {
		t.Errorf("InFlight of another port of an address = %d, %v", n, err)
	}
	// An address alone is what is open to it on any port, as a route can have moved.
	if n, err := c.InFlight(t.Context(), "172.30.0.2"); n != 2 || err != nil {
		t.Errorf("InFlight of an address = %d, %v, want the two open to it", n, err)
	}
	c.procNetTCP = filepath.Join(t.TempDir(), "gone")
	if _, err := c.InFlight(t.Context(), "172.30.0.2:8080"); err == nil {
		t.Error("it said how many, with nothing to read")
	}
}

// A route's file is in Caddy's directory, so what it says is read as what it is: an
// address and a port, or a hand edit, which reads as drift and is put right by a
// revert.
func TestWhatARouteDialsIsReadWithSuspicion(t *testing.T) {
	dir := shortDir(t)
	if err := EnsureDirs(dir); err != nil {
		t.Fatal(err)
	}
	c := &Caddy{Dir: dir, Runtime: &Container{}}
	const id = "dockwell.env_x.www"
	route := func(dial string) ingress.Route {
		return c.dialed(ingress.Route{ID: id, Upstreams: []string{dial}})
	}
	through, err := c.throughFile(ingress.Route{ID: id, Upstreams: []string{"172.30.0.2:8080"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.dialed(ingress.Route{ID: id, Upstreams: slices.Clone(through.Upstreams)}); got.Edited || !slices.Equal(got.Upstreams, []string{"172.30.0.2:8080"}) {
		t.Errorf("reading it back gives %+v", got)
	}
	if got := route("172.30.0.3:8080"); got.Edited || got.Upstreams[0] != "172.30.0.3:8080" {
		t.Errorf("a route that dials an address as it is reads %+v", got)
	}
	if got := route("{file./somewhere/else}"); !got.Edited {
		t.Errorf("a route that dials another file reads %+v", got)
	}
	file := filepath.Join(c.runDir(), upstreamsDir, id)
	if err := os.WriteFile(file, []byte("not an address"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := c.dialed(ingress.Route{ID: id, Upstreams: slices.Clone(through.Upstreams)}); !got.Edited || len(got.Upstreams) != 0 {
		t.Errorf("a file that says nothing it should reads %+v", got)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", file); err != nil {
		t.Fatal(err)
	}
	if got := c.dialed(ingress.Route{ID: id, Upstreams: slices.Clone(through.Upstreams)}); !got.Edited {
		t.Errorf("a link planted at the name reads %+v", got)
	}
	// Writing it again replaces the link, and doesn't follow it.
	if _, err := c.throughFile(ingress.Route{ID: id, Upstreams: []string{"172.30.0.2:8080"}}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(file); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("it is %v, %v", fi, err)
	}
}

// The directory that holds a route's file is Caddy's to replace, so deleting a route
// takes its own file and nothing a link there leads to.
func TestDeletingARouteTakesItsFileAndNothingOutsideCaddysDirectory(t *testing.T) {
	const id = "dockwell.env_x.www"
	dir := shortDir(t)
	c := &Caddy{Dir: dir, Runtime: &process{dir: dir}}
	if err := os.MkdirAll(c.runDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	through, err := c.throughFile(ingress.Route{ID: id, Upstreams: []string{"172.30.0.2:8080"}})
	if err != nil || len(through.Upstreams) != 1 {
		t.Fatalf("throughFile = %+v, %v", through, err)
	}
	file := filepath.Join(c.runDir(), upstreamsDir, id)
	if err := c.DeleteRoute(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the route's file is still there: %v", err)
	}

	outside := t.TempDir()
	someone := filepath.Join(outside, id)
	if err := os.WriteFile(someone, []byte("someone's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Dir(file)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Dir(file)); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRoute(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(someone); err != nil {
		t.Errorf("a link in Caddy's directory took a file outside it: %v", err)
	}
}

// A route that reaches its upstream some other way is a hand edit, as Dockwell didn't
// write that, however many steps come ahead of the one that sends it on.
func TestATransportNotOursReadsAsAnEdit(t *testing.T) {
	const id = "dockwell.env_x.www"
	for name, r := range map[string]ingress.Route{
		"on its own":   {ID: id, Domain: "app.example.net", Upstreams: []string{"172.30.0.2:8080"}},
		"after a step": {ID: id, Domain: "app.example.net", Upstreams: []string{"172.30.0.2:8080"}, TrustedProxies: []string{"10.0.0.0/8"}},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(routeJSON(r))
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := readRoute(body); !ok || got.Edited {
				t.Fatalf("as Dockwell writes it, it reads %+v, %t: %s", got, ok, body)
			}
			var read map[string]any
			if err := json.Unmarshal(body, &read); err != nil {
				t.Fatal(err)
			}
			steps := read["handle"].([]any)
			steps[len(steps)-1].(map[string]any)["transport"] = map[string]any{"protocol": "http", "tls": map[string]any{}}
			edited, err := json.Marshal(read)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := readRoute(edited); !ok || !got.Edited {
				t.Errorf("with a transport of someone else's, it reads %+v, %t", got, ok)
			}
		})
	}
}

// Caddy starts from its last config, so that needs a generation of its own too, or a
// connection on what it started from would never hear that it's old.
func TestWhatCaddyStartsFromKnowsItIsOld(t *testing.T) {
	dir := shortDir(t)
	if err := EnsureDirs(dir); err != nil {
		t.Fatal(err)
	}
	c := &Caddy{Dir: dir, Runtime: &Container{}}
	config := []byte(`{"apps":{"http":{"servers":{"` + serverName + `":{"routes":[]}}}}}`)
	next, err := c.withRoutes(config, func(routes []json.RawMessage) []json.RawMessage { return routes })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(next), generationRouteID) {
		t.Errorf("what Caddy starts from has no generation: %s", next)
	}
	files, err := filepath.Glob(filepath.Join(c.generationDir(), generationPrefix+"*"))
	if err != nil || len(files) != 1 || !strings.Contains(string(next), filepath.Base(files[0])) {
		t.Errorf("the generation files are %v, %v, and what it starts from is %s", files, err, next)
	}
}
