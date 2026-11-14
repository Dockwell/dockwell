// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// asked is a proxy that answers each route as answers says, and keeps what it was
// asked.
type asked struct {
	answers   map[string]int
	locations map[string]string
	headers   map[string]http.Header
	down      bool
	calls     int
}

func (a *asked) Ask(_ context.Context, domain, path string, _ bool, header http.Header) (Answer, error) {
	a.calls++
	if a.down {
		return Answer{}, errors.New("nothing listens on port 443")
	}
	if a.headers == nil {
		a.headers = map[string]http.Header{}
	}
	a.headers[domain+path] = header
	status := a.answers[domain+path]
	return Answer{Status: status, Said: http.StatusText(status), Location: a.locations[domain+path]}, nil
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
	routes := []Route{{Domain: "a.example.com", Path: "/admin", Allow: []string{"203.0.113.0/24"}}, {Domain: "a.example.com"}}
	for _, spelling := range []string{
		"/admin;x", "/;x/admin", "/x/..;/admin", "/%5cadmin", "/x/;/../admin", "/x/.;/../admin", "/x/%3b/../admin", "/x%c0%af..%c0%afadmin",
		"//x/admin", "/x/%ef%b8%b0/admin",
	} {
		a := &asked{answers: map[string]int{"a.example.com/admin": 403, "a.example.com" + spelling: 200}}
		got := AskStrangers(t.Context(), a, routes)["a.example.com"]
		if len(got) != 1 || got[0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN || !strings.Contains(got[0].GetSaid(), spelling) {
			t.Errorf("a path public as %s reads %v", spelling, got)
		}
	}
	// 400 for an odd spelling, as a domain with a private route answers, keeps out too.
	a := &asked{answers: map[string]int{"a.example.com/admin": 403}}
	for _, spelling := range Spellings("/admin", nil) {
		a.answers["a.example.com"+spelling] = 400
	}
	if got := AskStrangers(t.Context(), a, routes)["a.example.com"]; got[0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_KEPT_OUT {
		t.Errorf("a path kept out every way reads %v", got)
	}
}

// A private root is asked its spellings too, and from below each other path on its
// domain, as /api/..;/ reaches the root through a public /api in an app that takes
// ";" off (#606's final check of H2).
func TestAPrivateRootIsAskedFromBelowAPublicPath(t *testing.T) {
	routes := []Route{
		{Domain: "a.example.com", Allow: []string{"203.0.113.0/24"}},
		{Domain: "a.example.com", Path: "/api"},
		{Domain: "a.example.com", Path: "/hooks/"},
		{Domain: "b.example.com", Path: "/elsewhere"},
	}
	for _, spelling := range []string{"/api/..;/", "/api/.;/../", "/api/x%5c..%5c..%5c", "/hooks/..;/", "/x/;/../", "/x%c0%af..%c0%af", "//x/", "/x/%ef%b8%b0/"} {
		a := &asked{answers: map[string]int{"a.example.com/": 403, "a.example.com" + spelling: 200}}
		got := AskStrangers(t.Context(), a, routes)["a.example.com"]
		if len(got) != 1 || got[0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN || !strings.Contains(got[0].GetSaid(), spelling) {
			t.Errorf("a root public as %s reads %v", spelling, got)
		}
	}
	a := &asked{answers: map[string]int{"a.example.com/": 403}}
	_ = AskStrangers(t.Context(), a, routes)
	for path := range a.headers {
		if strings.Contains(path, "elsewhere") {
			t.Errorf("another domain's path is asked: %s", path)
		}
	}
	if !slices.Contains(Spellings("", []string{"/api"}), "/api/..;/") {
		t.Errorf("a root's spellings are %v", Spellings("", []string{"/api"}))
	}
}

// A route that signs people in is asked as a browser navigates, and a stranger sent
// to another origin is sent to sign in there; sent elsewhere on its own host, or let
// through, isn't (ADR 0087).
func TestAStrangerIsSentToSignIn(t *testing.T) {
	routes := []Route{
		{Domain: "s.example.com", SignIn: "unix//run/dockwell/signin.sock"},
		{Domain: "t.example.com", SignIn: "unix//run/dockwell/signin.sock"},
		{Domain: "u.example.com", SignIn: "unix//run/dockwell/signin.sock"},
	}
	a := &asked{
		answers: map[string]int{"s.example.com/": 302, "t.example.com/": 302, "u.example.com/": 200},
		locations: map[string]string{
			"s.example.com/": "https://auth.example.com/authorize?client_id=dockwell",
			"t.example.com/": "https://t.example.com/elsewhere",
		},
	}
	got := AskStrangers(t.Context(), a, routes)
	if c := got["s.example.com"]; len(c) != 1 || c[0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_SENT_TO_SIGN_IN || c[0].GetSaid() != "auth.example.com" {
		t.Errorf("a stranger sent to the provider reads %v", c)
	}
	if c := got["t.example.com"]; len(c) != 1 || c[0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN {
		t.Errorf("a stranger sent on along its own host reads %v", c)
	}
	if c := got["u.example.com"]; len(c) != 1 || c[0].GetOutcome() != dockwellv1.StrangerOutcome_STRANGER_OUTCOME_LET_IN {
		t.Errorf("a stranger let through reads %v", c)
	}
	if accept := a.headers["s.example.com/"].Get("Accept"); accept != "text/html" {
		t.Errorf("it asks a route that signs in with Accept %q, not as a navigation", accept)
	}
}
