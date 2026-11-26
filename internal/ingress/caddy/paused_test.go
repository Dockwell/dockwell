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

// A paused route answers 503 ahead of its upstream and reads back paused with
// its upstreams intact (never as a hand edit); changing its answer by hand
// reads back as edited.
func TestAPausedRouteAnswers503AndReadsBackPaused(t *testing.T) {
	plain := ingress.Route{ID: "dockwell.env.www", Domain: "app.example.net", Upstreams: []string{"172.30.0.2:80"}}
	believing := plain
	believing.ID, believing.TrustedProxies = "dockwell.env.tunnel", []string{"127.0.0.1/32"}
	for name, r := range map[string]ingress.Route{"naming no proxy": plain, "believing a proxy": believing} {
		t.Run(name, func(t *testing.T) {
			r.Paused = true
			written := routeJSON(r)
			if first := written.Handle[0]; first.Handler != "static_response" || first.StatusCode != http.StatusServiceUnavailable || first.Body != pausedBody {
				t.Fatalf("a paused route's first step is %+v", first)
			}
			b, err := json.Marshal(written)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := readRoute(b)
			if !ok || got.Edited || !got.Paused || got.Held || !slices.Equal(got.Upstreams, r.Upstreams) {
				t.Errorf("a paused route reads back as %+v", got)
			}
			r.Paused = false
			active, _ := json.Marshal(routeJSON(r))
			if got, _ := readRoute(active); got.Paused || got.Edited {
				t.Errorf("an unpaused route reads back as %+v", got)
			}
			changed := strings.Replace(string(b), "is paused", "is offline", 1)
			if got, _ := readRoute(json.RawMessage(changed)); got.Paused || !got.Edited {
				t.Errorf("a paused route whose answer was changed by hand reads back as %+v", got)
			}
		})
	}
}

// On real Caddy, a paused route answers 503 with its short page immediately
// instead of dialing the frozen container, and unpausing restores traffic.
func TestAPausedRouteAnswers503AndUnpauseRestoresOnRealCaddy(t *testing.T) {
	c := startCaddy(t)
	app := backend(t, "app")
	r := aRoute("dockwell.env.www", "app.example.net", "", app)
	r.Paused = true
	put(t, c, r)
	res := c.waitServing(t, "app.example.net", "/who")
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || string(body) != pausedBody {
		t.Errorf("a paused route answers %d %q", res.StatusCode, body)
	}
	routes, err := c.Routes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(routes, func(got ingress.Route) bool { return got.ID == r.ID }); i < 0 || !routes[i].Paused || routes[i].Edited {
		t.Errorf("Caddy reads the paused route back as %+v", routes)
	}
	r.Paused = false
	put(t, c, r)
	if got := c.whoAt(t, "app.example.net", "/who"); got["name"] != "app" {
		t.Errorf("an unpaused route reaches %v", got)
	}
}
