// SPDX-License-Identifier: AGPL-3.0-only

package signin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// An address the provider hasn't verified is neither kept nor told to the app,
// whatever lets the account in, as a group does here (#598's review, M2).
func TestAnUnverifiedAddressIsNeverTheUser(t *testing.T) {
	p := newProvider(t)
	s, route := stand(t, p)
	w := signIn(t, s, route, p, map[string]any{"email": "eve@elsewhere.example", "email_verified": false})
	if w.Code != http.StatusFound {
		t.Fatalf("someone in a group the route names gets %d", w.Code)
	}
	sessionName, _ := Names(route.ID)
	got := ask(s, http.MethodGet, "/admin", html, cookie(t, w, sessionName))
	if got.Code != http.StatusOK || got.Header().Values(UserHeader) != nil || got.Header().Get(SubjectHeader) == "" {
		t.Errorf("signed in with an unverified address, the app is told %d, %v", got.Code, got.Header())
	}
	sess, err := OpenSession(route.Key, route.ID, route.Generation, cookie(t, w, sessionName).Value, now)
	if err != nil || sess.Email != "" || sess.Domain != "" {
		t.Errorf("the session keeps %+v, %v", sess, err)
	}
}

// An address in letters outside ASCII names no one: the Kelvin sign lowers to k, so it
// would read as another domain's (#598's review, L1).
func TestAnAddressOutsideASCIINamesNoOne(t *testing.T) {
	p := newProvider(t)
	s, route := stand(t, p)
	route.EmailDomains, route.Groups = []string{"kiwi.example"}, nil
	if w := signIn(t, s, route, p, map[string]any{"email": "eve@Kiwi.example", "groups": nil}); w.Code != http.StatusForbidden {
		t.Errorf("an address with the Kelvin sign for k gets %d, want refused", w.Code)
	}
	if w := signIn(t, s, route, p, map[string]any{"email": "Eve@KIWI.example", "groups": nil}); w.Code != http.StatusFound {
		t.Errorf("an ASCII address in capitals gets %d, want let in", w.Code)
	}
	raw := map[string]json.RawMessage{}
	for k, v := range goodClaims() {
		raw[k], _ = json.Marshal(v)
	}
	raw["email"], _ = json.Marshal("\u0130nfo@example.com")
	c, err := checkClaims(raw, want, now)
	if err != nil || c.Email != "" || c.EmailVerified {
		t.Errorf("an address with a dotted capital I reads as %+v, %v", c, err)
	}
}

// At Google, a domain counts by hd alone, as a Google account can carry an address at
// a domain that doesn't manage it (#598's review, L2).
func TestAtGoogleADomainIsHostedDomainAlone(t *testing.T) {
	r := Route{Issuer: googleIssuer, EmailDomains: []string{"example.com"}}
	unmanaged := Claims{Email: "mira@example.com", EmailVerified: true}
	if d := r.vouched(unmanaged); r.letsIn(unmanaged.Email, d, nil) {
		t.Error("a Google account with an example.com address and no hd is let in by domain")
	}
	managed := Claims{Email: "mira@example.com", EmailVerified: true, HostedDomain: "example.com"}
	if d := r.vouched(managed); !r.letsIn(managed.Email, d, nil) {
		t.Error("a Google Workspace account at example.com isn't let in by domain")
	}
	elsewhere := Route{Issuer: issuer, EmailDomains: []string{"example.com"}}
	if d := elsewhere.vouched(unmanaged); !elsewhere.letsIn(unmanaged.Email, d, nil) {
		t.Error("elsewhere, a verified example.com address isn't let in by domain")
	}
}

// A provider that can't be read stays so for 30 seconds, so a crowd waits on one
// read, and a visitor waiting on a read under way stops waiting when they leave
// (#598's review, L4).
func TestAProviderThatCantBeReadIsAskedOnce(t *testing.T) {
	var reads atomic.Int32
	hold := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		<-hold
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	clock := now
	s := &Server{Client: srv.Client(), Now: func() time.Time { return clock }}
	issuer := srv.URL + "/"
	first := make(chan error, 1)
	go func() { _, err := s.discover(t.Context(), issuer); first <- err }()
	for reads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.discover(gone, issuer); !errors.Is(err, context.Canceled) {
		t.Errorf("a visitor who left while a read was under way gets %v", err)
	}
	close(hold)
	if err := <-first; err == nil {
		t.Fatal("a provider answering 503 reads as found")
	}
	for range 5 {
		if _, err := s.discover(t.Context(), issuer); err == nil {
			t.Fatal("a provider that couldn't be read reads as found")
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("a provider that couldn't be read is asked %d times in 30 seconds, want once", n)
	}
	clock = clock.Add(31 * time.Second)
	_, _ = s.discover(t.Context(), issuer)
	if n := reads.Load(); n != 2 {
		t.Errorf("after 30 seconds it's asked %d times in all, want twice", n)
	}
}

// A route's cookies are bound to its issuer and client, so another of either signs
// everyone out (#598's review, L5).
func TestAnotherClientSignsEveryoneOut(t *testing.T) {
	keep := &Keep{Dir: t.TempDir()}
	spec := func(client string) *resourcev1.RouteSignIn {
		return &resourcev1.RouteSignIn{
			Provider: "ours", Emails: []string{"mira@example.com"},
			Filled: &resourcev1.RouteSignInProvider{Issuer: issuer, ClientId: client},
		}
	}
	if err := keep.Put("rte_admin", "app.example.net", "/admin", spec("dockwell")); err != nil {
		t.Fatal(err)
	}
	route, _ := keep.Route("rte_admin")
	value, err := SealSession(route.Key, Session{Route: route.ID, Subject: "1", Email: "mira@example.com", Made: now, Ends: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := keep.Put("rte_admin", "app.example.net", "/admin", spec("dockwell")); err != nil {
		t.Fatal(err)
	}
	if again, _ := keep.Route("rte_admin"); func() error { _, err := OpenSession(again.Key, again.ID, 0, value, now); return err }() != nil {
		t.Error("the same client again signs everyone out")
	}
	if err := keep.Put("rte_admin", "app.example.net", "/admin", spec("dockwell-2")); err != nil {
		t.Fatal(err)
	}
	moved, _ := keep.Route("rte_admin")
	if _, err := OpenSession(moved.Key, moved.ID, 0, value, now); err == nil {
		t.Error("a session made with one client opens with another")
	}
}

// A visitor is who Caddy says, never what they write, and an IPv6 visitor counts by
// their /64 (#598's review, L3).
func TestAVisitorIsWhoCaddySays(t *testing.T) {
	ask := func(client, forwarded string) string {
		r := httptest.NewRequest(http.MethodGet, "/route/rte_admin", http.NoBody)
		r.Header.Set(ClientHeader, client)
		r.Header.Set("X-Forwarded-For", forwarded)
		return clientOf(r)
	}
	if got := ask("198.51.100.7", "203.0.113.9"); got != "198.51.100.7" {
		t.Errorf("an IPv4 visitor counts as %q", got)
	}
	if a, b := ask("2001:db8:1:2::7", ""), ask("2001:db8:1:2:ffff::1", "203.0.113.9"); a != b || a != "2001:db8:1:2::/64" {
		t.Errorf("two addresses of one /64 count as %q and %q", a, b)
	}
	if got := ask("2001:db8:1:3::7", ""); got == ask("2001:db8:1:2::7", "") {
		t.Error("another /64 counts as the same visitor")
	}
	if got := ask("::ffff:198.51.100.7", ""); got != "198.51.100.7" {
		t.Errorf("an IPv4 visitor written in IPv6 counts as %q", got)
	}
	if got := ask("", "198.51.100.7"); got != "" {
		t.Errorf("with nothing from Caddy, a visitor counts by what they wrote: %q", got)
	}
}
