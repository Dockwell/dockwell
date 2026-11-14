// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/dockwell/dockwell/internal/ingress"
)

// A route Caddy kept from before Client-Ip and its like were stripped reads as
// Dockwell's own, so an upgrade isn't drift, and one that lets anything more
// through than any build wrote is a hand edit.
func TestARouteFromBeforeTheListsGrewReadsAsOurs(t *testing.T) {
	if !slices.Contains(stripped(), "X-Scheme") || len(strippedThird) != len(stripped())-1 {
		t.Fatalf("strippedThird is %d names, and a route strips %d: it should be all of them but X-Scheme", len(strippedThird), len(stripped()))
	}
	plain := ingress.Route{ID: "dockwell.env.www", Domain: "app.example.net", Upstreams: []string{"172.30.0.2:80"}, Plain: true}
	believing := plain
	believing.ID, believing.TrustedProxies = "dockwell.env.tunnel", []string{"127.0.0.1/32"}
	for name, r := range map[string]ingress.Route{"naming no proxy": plain, "believing a proxy": believing} {
		t.Run(name, func(t *testing.T) {
			read := func(del []string) ingress.Route {
				t.Helper()
				written := routeJSON(r)
				last := &written.Handle[len(written.Handle)-1]
				last.Headers = strippingOnly(last.Headers, del)
				b, err := json.Marshal(written)
				if err != nil {
					t.Fatal(err)
				}
				got, ok := readRoute(b)
				if !ok {
					t.Fatalf("%s doesn't read as Dockwell's", b)
				}
				return got
			}
			if got := read(stripped()); got.Edited {
				t.Error("a route as this build writes it reads as a hand edit")
			}
			if got := read(strippedSecond); got.Edited {
				t.Error("a route from before Client-Ip was stripped reads as a hand edit")
			}
			if got := read(strippedThird); got.Edited {
				t.Error("a route from before X-Scheme was stripped reads as a hand edit")
			}
			if got := read(slices.DeleteFunc(stripped(), func(h string) bool { return h == "Client-Ip" })); !got.Edited {
				t.Error("a route that lets Client-Ip through reads as Dockwell's own")
			}
		})
	}
}

// A route from before every route took Dockwell's sign-in cookies out of what goes to
// its app, and out of what its app sets, reads as Dockwell's own, and the next change
// on its host writes over it; a route that signs people in without taking them out is
// a hand edit (#598's review, M4 and New 3).
func TestARouteFromBeforeCookiesWereTakenOutReadsAsOurs(t *testing.T) {
	read := func(r ingress.Route, keep bool) ingress.Route {
		t.Helper()
		written := routeJSON(r)
		if !keep {
			h := written.Handle[len(written.Handle)-1].Headers
			h.Request.Replace = nil
			if h.Response.Replace = nil; len(h.Response.Add) == 0 {
				h.Response = nil
			}
		}
		b, err := json.Marshal(written)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := readRoute(b)
		if !ok {
			t.Fatalf("%s doesn't read as Dockwell's", b)
		}
		return got
	}
	public := ingress.Route{ID: "dockwell.env.www", Domain: "app.example.net", Upstreams: []string{"172.30.0.2:80"}}
	signsIn := public
	signsIn.ID, signsIn.SignIn = "dockwell.env.admin", "unix//run/dockwell/signin.sock"
	if got := read(public, true); got.Edited {
		t.Error("a public route as this build writes it reads as a hand edit")
	}
	if got := read(public, false); got.Edited {
		t.Error("a public route from before cookies were taken out reads as a hand edit")
	}
	if got := read(signsIn, true); got.Edited || got.SignIn != signsIn.SignIn {
		t.Errorf("a route that signs in reads as %+v", got)
	}
	if got := read(signsIn, false); !got.Edited {
		t.Error("a route that signs in and passes Dockwell's cookies on reads as Dockwell's own")
	}
}
