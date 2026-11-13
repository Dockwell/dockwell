// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// asked is a proxy that answers each route as answers says, and keeps what it was
// asked.
type asked struct {
	answers map[string]int
	headers map[string]http.Header
	down    bool
	calls   int
}

func (a *asked) Ask(_ context.Context, domain, path string, _ bool, header http.Header) (int, string, error) {
	a.calls++
	if a.down {
		return 0, "", errors.New("nothing listens on port 443")
	}
	if a.headers == nil {
		a.headers = map[string]http.Header{}
	}
	a.headers[domain+path] = header
	status := a.answers[domain+path]
	return status, http.StatusText(status), nil
}

// A stranger is asked for as someone no list names: from this server, or, through a
// tunnel here the route believes, as a documentation address. Where that's on the
// list, or the route doesn't answer this server, it says it can't tell.
func TestAStrangerIsSomeoneNoListNames(t *testing.T) {
	for _, tc := range []struct {
		name     string
		r        Route
		who      string
		cantTell string
	}{
		{"with a certificate", Route{Domain: "a.example.com", Allow: []string{"203.0.113.0/24"}}, "127.0.0.1", ""},
		{"through a tunnel here", Route{Domain: "a.example.com", Plain: true, TrustedProxies: []string{"127.0.0.1/32"}, Allow: []string{"203.0.113.0/24"}}, "192.0.2.1", ""},
		{"answering anyone", Route{Domain: "a.example.com", Plain: true, PlainForAnyone: true, Users: []User{{Name: "ci"}}}, "127.0.0.1", ""},
		{"only another proxy", Route{Domain: "a.example.com", Plain: true, TrustedProxies: []string{"10.0.0.0/8"}, Users: []User{{Name: "ci"}}}, "", "answers only the proxies"},
		{"this server on the list", Route{Domain: "a.example.com", Allow: []string{"127.0.0.0/8"}}, "", "127.0.0.1 is on its list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header, who, cantTell := stranger(tc.r)
			if who != tc.who || !strings.Contains(cantTell, tc.cantTell) || (tc.cantTell == "") != (cantTell == "") {
				t.Errorf("stranger = %q, %q; want %q, %q", who, cantTell, tc.who, tc.cantTell)
			}
			if tc.who == "192.0.2.1" && header.Get("X-Forwarded-For") != "192.0.2.1" {
				t.Errorf("through the tunnel, it says it passes on %q", header.Get("X-Forwarded-For"))
			}
			if tc.who == "127.0.0.1" && header.Get("X-Forwarded-For") != "" {
				t.Errorf("with no tunnel, it writes X-Forwarded-For %q", header.Get("X-Forwarded-For"))
			}
		})
	}
}

// Each private route gets its reading, a public one none, and once the proxy doesn't
// answer, nothing more is asked.
func TestEachPrivateRouteIsAskedAsAStranger(t *testing.T) {
	routes := []Route{
		{Domain: "a.example.com", Allow: []string{"203.0.113.0/24"}},
		{Domain: "a.example.com", Path: "/admin", Users: []User{{Name: "ci"}}},
		{Domain: "b.example.com"},
		{Domain: "c.example.com", Users: []User{{Name: "ci"}}},
		{Domain: "d.example.com", Allow: []string{"127.0.0.1/32"}},
		{Domain: "e.example.com", Users: []User{{Name: "ci"}}, Edited: true},
	}
	a := &asked{answers: map[string]int{"a.example.com/": 403, "a.example.com/admin": 401, "c.example.com/": 200}}
	got := AskStrangers(t.Context(), a, routes)
	want := map[string][]dockwellv1.StrangerOutcome{
		"a.example.com": {dockwellv1.StrangerOutcome_STRANGER_OUTCOME_KEPT_OUT, dockwellv1.StrangerOutcome_STRANGER_OUTCOME_ASKED_FOR_PASSWORD},
		"c.example.com": {dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN},
		"d.example.com": {dockwellv1.StrangerOutcome_STRANGER_OUTCOME_CANT_TELL},
	}
	if len(got) != len(want) {
		t.Fatalf("readings for %v", got)
	}
	for domain, outcomes := range want {
		for i, o := range outcomes {
			if got[domain][i].GetOutcome() != o {
				t.Errorf("%s: %v, want %v", domain, got[domain], outcomes)
			}
		}
	}
	if s := got["a.example.com"][0]; s.GetPath() != "" || s.GetStatus() != 403 || s.GetSaid() != "Forbidden" {
		t.Errorf("the whole domain reads %v", s)
	}

	down := &asked{down: true}
	got = AskStrangers(t.Context(), down, routes)
	if down.calls != 1 || got["c.example.com"][0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_OTHER || !strings.Contains(got["c.example.com"][0].GetSaid(), "nothing listens") {
		t.Errorf("with the proxy down, it asked %d times, and reads %v", down.calls, got)
	}
}

// A private path is asked with ";x" after it too, as an app may read that as the path:
// a public route above it that answers it is a stranger let in (#606's review, H2).
func TestAPrivatePathIsAskedAsAnotherSpellingToo(t *testing.T) {
	routes := []Route{{Domain: "a.example.com", Path: "/admin", Allow: []string{"203.0.113.0/24"}}}
	a := &asked{answers: map[string]int{"a.example.com/admin": 403, "a.example.com/admin;x": 200}}
	got := AskStrangers(t.Context(), a, routes)["a.example.com"]
	if len(got) != 1 || got[0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN || !strings.Contains(got[0].GetSaid(), "/admin;x") {
		t.Errorf("a path whose other spelling is public reads %v", got)
	}
	a.answers["a.example.com/admin;x"] = 403
	if got := AskStrangers(t.Context(), a, routes)["a.example.com"]; got[0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_KEPT_OUT {
		t.Errorf("a path kept out either way reads %v", got)
	}
}
