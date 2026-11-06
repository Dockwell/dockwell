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
