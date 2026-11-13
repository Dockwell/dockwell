// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/ingress"
)

// A held route answers that the app isn't open yet, ahead of where it would send, and
// reads back held, with where it sends, as this build wrote it rather than as a hand
// edit; one whose answer someone changed reads as edited (ADR 0084).
func TestAHeldRouteAnswersFirstAndReadsBackHeld(t *testing.T) {
	plain := ingress.Route{ID: "dockwell.env.www", Domain: "app.example.net", Upstreams: []string{"172.30.0.2:80"}}
	believing := plain
	believing.ID, believing.TrustedProxies = "dockwell.env.tunnel", []string{"127.0.0.1/32"}
	for name, r := range map[string]ingress.Route{"naming no proxy": plain, "believing a proxy": believing} {
		t.Run(name, func(t *testing.T) {
			r.Held = true
			written := routeJSON(r)
			if first := written.Handle[0]; first.Handler != "static_response" || first.StatusCode != http.StatusServiceUnavailable || first.Body != heldBody {
				t.Fatalf("a held route's first step is %+v", first)
			}
			b, err := json.Marshal(written)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := readRoute(b)
			if !ok || got.Edited || !got.Held || !slices.Equal(got.Upstreams, r.Upstreams) {
				t.Errorf("a held route reads back as %+v", got)
			}
			r.Held = false
			open, _ := json.Marshal(routeJSON(r))
			if got, _ := readRoute(open); got.Held || got.Edited {
				t.Errorf("an open route reads back as %+v", got)
			}
			changed := strings.Replace(string(b), "isn't open yet", "is closed", 1)
			if got, _ := readRoute(json.RawMessage(changed)); got.Held || !got.Edited {
				t.Errorf("a held route whose answer was changed by hand reads back as %+v", got)
			}
		})
	}
}

// On real Caddy, a held route answers 503 with its words and sends nothing on, and
// once it's opened, it sends on as any route does (ADR 0084).
func TestAHeldRouteSendsNothingOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	app := backend(t, "app")
	r := aRoute("dockwell.env.www", "app.example.net", "", app)
	r.Held = true
	put(t, c, r)
	res := c.waitServing(t, "app.example.net", "/who")
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || string(body) != heldBody {
		t.Errorf("a held route answers %d %q", res.StatusCode, body)
	}
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(routes, func(got ingress.Route) bool { return got.ID == r.ID }); i < 0 || !routes[i].Held || routes[i].Edited {
		t.Errorf("Caddy reads the held route back as %+v", routes)
	}
	r.Held = false
	put(t, c, r)
	if got := c.whoAt(t, "app.example.net", "/who"); got["name"] != "app" {
		t.Errorf("an opened route reaches %v", got)
	}
}
