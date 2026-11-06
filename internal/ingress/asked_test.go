// SPDX-License-Identifier: AGPL-3.0-only

package ingress

import (
	"fmt"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Removing a route doesn't take back what Caddy asked the certificate authority,
// so names added and removed in turns, 50 at a time, stop at the limit on what
// was asked, short of Let's Encrypt's 300 new orders in 3 hours. The check of #76
// got 400 in, as each delete took its domain out of the count at once.
func TestAddingAndRemovingDomainsInTurnsStopsAtWhatWasAsked(t *testing.T) {
	s := newSetup(t)
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	s.proxy.status = Status{Exists: true, Running: true}
	batch := func(r int) []resource.Desired {
		var out []resource.Desired
		for i := range MaxPendingDomains {
			out = append(out, resource.Desired{Ref: routeRef(fmt.Sprintf("c%d-%d", r, i)), Spec: spec(fmt.Sprintf("c%d-%d.example.com", r, i), "", "web", 80)})
		}
		return out
	}
	round := func(r int) error {
		b := batch(r)
		if err := s.Check(t.Context(), b[0].Ref, b); err != nil {
			return err
		}
		for _, d := range b {
			s.apply(t, d.Ref.Name, d.Spec.(*resourcev1.Route))
		}
		for _, d := range b {
			s.apply(t, d.Ref.Name, nil)
		}
		return nil
	}
	for r := range MaxAskedDomains / MaxPendingDomains {
		if err := round(r); err != nil {
			t.Fatalf("round %d of adding and removing 50 = %v", r, err)
		}
	}
	err := round(99)
	if err == nil {
		t.Fatal("a fourth round of 50 new domains went in, after 150 asked for")
	}
	for _, want := range []string{"150 domains were added", "past the limit of 150", "removing its route doesn't take back", "300 new orders", "stops counting in 3 h"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("it says %q, want %q in it", err, want)
		}
	}
	// A step on its own holds the same, as a plan made earlier saw fewer.
	if err := s.Apply(t.Context(), routeRef("one"), spec("one.example.com", "", "web", 80), nil); err == nil || !strings.Contains(err.Error(), "limit of 150") {
		t.Errorf("a step past what was asked = %v", err)
	}
	// A name counted already isn't counted twice.
	again := routeRef("again")
	if err := s.Check(t.Context(), again, []resource.Desired{{Ref: again, Spec: spec("c0-1.example.com", "", "web", 80)}}); err != nil {
		t.Errorf("a domain asked about already = %v", err)
	}
	// Nor does one with no certificate to ask for.
	plain := spec("plain.example.com", "", "web", 80)
	plain.Tls = TLSOff
	if err := s.Check(t.Context(), routeRef("plain"), []resource.Desired{{Ref: routeRef("plain"), Spec: plain}}); err != nil {
		t.Errorf("a domain with no certificate = %v", err)
	}
	// Once the window has passed, the domains asked about then no longer count.
	clock = clock.Add(PendingWindow + time.Minute)
	if err := round(100); err != nil {
		t.Errorf("3 hours on, a round of 50 = %v", err)
	}
}

// addAndRemove adds these domains and takes them away again, as a token could.
func addAndRemove(t *testing.T, s *setup, prefix string, names int) error {
	t.Helper()
	var out []resource.Desired
	for i := range names {
		out = append(out, resource.Desired{Ref: routeRef(fmt.Sprintf("%s-%d", prefix, i)), Spec: spec(fmt.Sprintf("%s-%d.example.com", prefix, i), "", "web", 80)})
	}
	if err := s.Check(t.Context(), out[0].Ref, out); err != nil {
		return err
	}
	for _, d := range out {
		s.apply(t, d.Ref.Name, d.Spec.(*resourcev1.Route))
	}
	for _, d := range out {
		s.apply(t, d.Ref.Name, nil)
	}
	return nil
}

// A person undoing a delete asks the authority for nothing, as the certificate is on
// the server, so what a token added can't hold that up for 3 hours.
func TestPuttingBackADomainWithItsCertificateAsksForNothing(t *testing.T) {
	s := newSetup(t)
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	s.proxy.status = Status{Exists: true, Running: true}
	s.proxy.certs = map[string]CertState{"shop.example.com": {Phase: CertIssued}}
	s.apply(t, "shop", spec("shop.example.com", "", "web", 80))
	s.apply(t, "shop", nil)
	for r := range MaxAskedDomains / MaxPendingDomains {
		if err := addAndRemove(t, s, fmt.Sprintf("r%d", r), MaxPendingDomains); err != nil {
			t.Fatal(err)
		}
	}
	shop, fresh := spec("shop.example.com", "", "web", 80), spec("new.example.com", "", "web", 80)
	if err := s.Check(t.Context(), routeRef("new"), []resource.Desired{{Ref: routeRef("new"), Spec: fresh}}); err == nil {
		t.Fatal("a new domain went in past the limit, so this proves nothing")
	}
	if err := s.Check(t.Context(), routeRef("shop"), []resource.Desired{{Ref: routeRef("shop"), Spec: shop}}); err != nil {
		t.Errorf("undoing the delete of a domain with its certificate = %v", err)
	}
	if err := s.Apply(t.Context(), routeRef("shop"), shop, nil); err != nil {
		t.Errorf("the undo's step = %v", err)
	}
}

// The same goes at the limit of 50 waiting.
func TestPuttingBackADomainPastTheWaitingLimitAsksForNothing(t *testing.T) {
	s := newSetup(t)
	s.proxy.status = Status{Exists: true, Running: true}
	s.proxy.certs = map[string]CertState{"shop.example.com": {Phase: CertIssued}}
	for i := range MaxPendingDomains {
		name := fmt.Sprintf("w-%d", i)
		s.apply(t, name, spec(name+".example.com", "", "web", 80))
	}
	shop := routeRef("shop")
	if err := s.Check(t.Context(), shop, []resource.Desired{{Ref: shop, Spec: spec("shop.example.com", "", "web", 80)}}); err != nil {
		t.Errorf("putting back a domain with its certificate, with 50 waiting = %v", err)
	}
}

// What the threat model says this doesn't stop: it counts names with no certificate
// yet, not orders. A change here should change what that says.
func TestTheAskedLimitCountsNamesNotOrders(t *testing.T) {
	s := newSetup(t)
	clock := time.Now()
	s.Now = func() time.Time { return clock }
	s.proxy.status = Status{Exists: true, Running: true}
	for r := range 4 {
		if err := addAndRemove(t, s, "same", MaxPendingDomains); err != nil {
			t.Fatalf("adding the same 50 names, time %d = %v", r, err)
		}
	}
	// New names, whose certificates come.
	s.proxy.certs = map[string]CertState{}
	for r := range 4 {
		prefix := fmt.Sprintf("n%d", r)
		if err := addAndRemove(t, s, prefix, MaxPendingDomains); err != nil {
			t.Fatalf("round %d of 50 new names that got their certificates = %v", r, err)
		}
		for i := range MaxPendingDomains {
			s.proxy.certs[fmt.Sprintf("%s-%d.example.com", prefix, i)] = CertState{Phase: CertIssued}
		}
	}
}
