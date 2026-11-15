// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/ingress"
)

// A redirect is written as a static_response with its status and Location, and reads
// back as the same route, so a redirect Dockwell wrote is never drift (ADR 0070).
func TestARedirectReadsBackAsItWasWritten(t *testing.T) {
	cases := map[string]ingress.Route{
		"permanent, keeping the path": {ID: "dockwell.env.www", Domain: "www.example.com", Redirect: &ingress.Redirect{To: "https://example.com"}},
		"temporary, to a path":        {ID: "dockwell.env.old", Domain: "old.example.com", Redirect: &ingress.Redirect{To: "https://example.com/blog", Temporary: true}},
		"dropping the path":           {ID: "dockwell.env.promo", Domain: "promo.example.com", Path: "/sale", Redirect: &ingress.Redirect{To: "https://example.com/?from=promo", DropPath: true}},
		"behind a tunnel":             {ID: "dockwell.env.tun", Domain: "www.example.net", Plain: true, TrustedProxies: []string{"127.0.0.1/32"}, Redirect: &ingress.Redirect{To: "https://example.net"}},
		"plain for anyone":            {ID: "dockwell.env.lan", Domain: "www.lan.example.org", Plain: true, PlainForAnyone: true, Redirect: &ingress.Redirect{To: "http://lan.example.org"}},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(routeJSON(want))
			if err != nil {
				t.Fatal(err)
			}
			got, ok := readRoute(raw)
			if !ok {
				t.Fatalf("%s doesn't read as one of Dockwell's routes", raw)
			}
			if got.Edited {
				t.Fatalf("a redirect Dockwell wrote reads as edited by hand: %s", raw)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("read back %+v (redirect %+v), wrote %+v (redirect %+v), from %s", got, got.Redirect, want, want.Redirect, raw)
			}
		})
	}
}

// What answers is the status and the Location a browser follows: the path and query
// after where it sends, unless it drops them.
func TestARedirectAnswersWithItsStatusAndLocation(t *testing.T) {
	for _, c := range []struct {
		redirect ingress.Redirect
		status   int
		location string
	}{
		{ingress.Redirect{To: "https://example.com"}, 301, "https://example.com{http.request.uri}"},
		{ingress.Redirect{To: "https://example.com/blog", Temporary: true}, 302, "https://example.com/blog{http.request.uri}"},
		{ingress.Redirect{To: "https://example.com/", DropPath: true}, 301, "https://example.com/"},
	} {
		h := redirectHandler(&c.redirect)
		if h.Handler != "static_response" || h.StatusCode != c.status || h.Headers == nil || len(h.Headers.Location) != 1 || h.Headers.Location[0] != c.location {
			t.Errorf("%+v answers %+v, want %d to %s", c.redirect, h, c.status, c.location)
		}
	}
}

// Anything else in a static_response, by hand, reads as an edit: another status,
// another header, or a body.
func TestAHandEditedRedirectReadsAsEdited(t *testing.T) {
	ours, err := json.Marshal(routeJSON(ingress.Route{ID: "dockwell.env.www", Domain: "www.example.com", Redirect: &ingress.Redirect{To: "https://example.com"}}))
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string][2]string{
		"another status":  {`"status_code":301`, `"status_code":307`},
		"another header":  {`"Location":[`, `"X-Extra":["1"],"Location":[`},
		"a body":          {`"status_code":301`, `"status_code":301,"body":"moved"`},
		"two locations":   {`{http.request.uri}"]`, `{http.request.uri}","https://evil.example"]`},
		"a placeholder":   {`"https://example.com{http.request.uri}"`, `"https://{http.request.host}{http.request.uri}"`},
		"an upstream too": {`"handler":"static_response"`, `"handler":"static_response","upstreams":[{"dial":"10.0.0.1:80"}]`},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(string(ours), edit[0], edit[1], 1)
			if raw == string(ours) {
				t.Fatalf("the edit %q found nothing to change in %s", edit[0], ours)
			}
			got, ok := readRoute(json.RawMessage(raw))
			if !ok || !got.Edited {
				t.Fatalf("%s reads as %+v, not as edited by hand", raw, got)
			}
		})
	}
}

// What a server trusts is written into each route that sends a request on, and a
// redirect sends none: its answer's headers stay its Location alone.
func TestTrustLeavesARedirectsAnswerAlone(t *testing.T) {
	raw, err := json.Marshal(routeJSON(ingress.Route{ID: "dockwell.env.www", Domain: "www.example.com", Redirect: &ingress.Redirect{To: "https://example.com"}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, trusts := range []bool{false, true} {
		got := withTrust([]json.RawMessage{raw}, trusts)[0]
		if !sameJSON(got, raw) {
			t.Errorf("with the server trusting a proxy %v, the redirect became %s", trusts, got)
		}
	}
}

// A path route strips its own path before what follows goes after where it sends,
// with a rewrite ahead of the answer, unless it keeps its prefix; a permanent redirect
// says how long browsers keep it, and a temporary one says nothing of caching. Each
// reads back as written (ADR 0070).
func TestARedirectStripsItsPrefixAndSaysHowLongItsKept(t *testing.T) {
	strip := ingress.Route{ID: "dockwell.env.old", Domain: "example.com", Path: "/old-blog", Redirect: &ingress.Redirect{To: "https://blog.example.com"}}
	raw, err := json.Marshal(routeJSON(strip))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `{"handler":"rewrite","strip_path_prefix":"/old-blog"}`) || !strings.Contains(string(raw), `"Cache-Control":["max-age=86400"]`) {
		t.Errorf("a path route's permanent redirect is written %s", raw)
	}
	for name, want := range map[string]ingress.Route{
		"stripping its prefix":                strip,
		"keeping its prefix":                  {ID: "dockwell.env.keep", Domain: "example.com", Path: "/old-blog", Redirect: &ingress.Redirect{To: "https://blog.example.com", KeepPrefix: true}},
		"stripping, behind a proxy it trusts": {ID: "dockwell.env.cf", Domain: "example.com", Path: "/old", TrustedProxies: []string{"203.0.113.0/24"}, Redirect: &ingress.Redirect{To: "https://example.org"}},
		"temporary":                           {ID: "dockwell.env.tmp", Domain: "www.example.com", Redirect: &ingress.Redirect{To: "https://example.com", Temporary: true}},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(routeJSON(want))
			if err != nil {
				t.Fatal(err)
			}
			got, ok := readRoute(raw)
			if !ok || got.Edited || !reflect.DeepEqual(got, want) {
				t.Fatalf("read back %+v (redirect %+v), wrote %+v, from %s", got, got.Redirect, want, raw)
			}
			if want.Redirect.Temporary == strings.Contains(string(raw), "Cache-Control") {
				t.Errorf("temporary %v says %s", want.Redirect.Temporary, raw)
			}
		})
	}
	ours := string(raw)
	for name, edit := range map[string][2]string{
		"no cache header":        {`,"Cache-Control":["max-age=86400"]`, ``},
		"kept for a year":        {`max-age=86400`, `max-age=31536000`},
		"stripping another path": {`"strip_path_prefix":"/old-blog"`, `"strip_path_prefix":"/old"`},
		"more in the rewrite":    {`"strip_path_prefix":"/old-blog"`, `"strip_path_prefix":"/old-blog","uri":"/x"`},
	} {
		t.Run(name, func(t *testing.T) {
			edited := strings.Replace(ours, edit[0], edit[1], 1)
			if edited == ours {
				t.Fatalf("the edit %q found nothing to change in %s", edit[0], ours)
			}
			if got, ok := readRoute(json.RawMessage(edited)); !ok || !got.Edited {
				t.Fatalf("%s reads as %+v, not as edited by hand", edited, got)
			}
		})
	}
}
