// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// providerProjects is the projects the stand's provider serves.
func providerProjects(t *testing.T, s *publishStand) []string {
	t.Helper()
	dc := dockwellv1connect.NewDomainServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	listed, err := dc.ListDnsProviders(t.Context(), connect.NewRequest(&dockwellv1.ListDnsProvidersRequest{}))
	if err != nil || len(listed.Msg.GetProviders()) != 1 {
		t.Fatalf("listed: %v %v", listed, err)
	}
	return listed.Msg.GetProviders()[0].GetProjects()
}

// Two websites published through one provider each add their project to it. Undoing
// the first, after the second was added, takes out only the first's project, as a
// set, and keeps the second's and every other; then the second undoes too (#792's
// review, M2). An install's project is added the same way.
func TestUndoingOneOfTwoPublishedWebsitesTakesOutOnlyItsProject(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		wc := websiteClient(s.b, s.b.origin)
		made := func(domain string) (*dockwellv1.Change, string) {
			t.Helper()
			res, err := wc.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
				Domain: domain, Website: onPort3000(), Tls: "off", TrustedProxies: []string{"127.0.0.1"}, Publish: throughWebOne(),
			}))
			if err != nil {
				t.Fatal(err)
			}
			ch := res.Msg.GetChange()
			s.applied(t, ch)
			var project string
			for _, r := range ch.GetPlan().GetResources() {
				if r.GetResource().GetKind() == "project" {
					project = r.GetId()
				}
			}
			if project == "" {
				t.Fatalf("no project in %v", ch.GetPlan().GetResources())
			}
			return ch, project
		}
		first, a := made("docs.example.com")
		second, b := made("blog.example.com")
		if got := providerProjects(t, s); !slices.Contains(got, a) || !slices.Contains(got, b) || !slices.Contains(got, s.project) {
			t.Fatalf("after both, the provider serves %v", got)
		}
		undo := func(ch *dockwellv1.Change) {
			t.Helper()
			res, err := s.person.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: ch.GetId()}))
			if err != nil {
				t.Fatalf("undo %s: %v", ch.GetSummary(), err)
			}
			for _, r := range res.Msg.GetChange().GetPlan().GetResources() {
				if r.GetResource().GetKind() != "dns.provider" {
					continue
				}
				for _, c := range r.GetChanges() {
					if c.GetPath() != "projects" {
						t.Errorf("undoing %s changes the provider's %s", ch.GetSummary(), c.GetPath())
					}
				}
			}
			s.applied(t, res.Msg.GetChange())
		}
		undo(first)
		if got := providerProjects(t, s); slices.Contains(got, a) || !slices.Contains(got, b) || !slices.Contains(got, s.project) {
			t.Fatalf("after undoing the first, the provider serves %v, want %s and %s without %s", got, s.project, b, a)
		}
		if got := recordNames(s); !slices.Equal(got, []string{"blog.example.com"}) {
			t.Fatalf("records after undoing the first: %v", got)
		}
		undo(second)
		if got := providerProjects(t, s); !slices.Equal(got, []string{s.project}) {
			t.Fatalf("after undoing both, the provider serves %v, want only %s", got, s.project)
		}
	})
}

// An existing website can publish a new alias without changing how its main name
// or its other aliases are reached. Later website edits keep the alias published.
func TestAWebsiteAliasHasItsOwnWayIn(t *testing.T) {
	t.Parallel()
	s := newPublishStand(t)
	s.applied(t, propose(t, s.person, "Use Cloudflare for every project", s.provider(func(p *resourcev1.DnsProvider) {
		p.EveryProject, p.Projects = true, nil
	})))
	wc := websiteClient(s.b, s.b.origin)
	spec := onPort3000()
	spec.RedirectFrom = []string{"old.example.org"}
	made, err := wc.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
		Domain: "site.example.org", Website: spec,
	}))
	if err != nil {
		t.Fatal(err)
	}
	s.applied(t, made.Msg.GetChange())
	site := ""
	for _, r := range made.Msg.GetChange().GetPlan().GetResources() {
		if r.GetResource().GetKind() == "project" {
			site = r.GetId()
		}
	}
	if site == "" {
		t.Fatal("the website has no project")
	}
	spec.RedirectFrom = append(spec.RedirectFrom, "site.example.com")
	update := func(front *dockwellv1.WebsiteDomainFront) {
		t.Helper()
		changed, err := wc.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{
			Id: site, Website: spec, DomainFront: front,
		}))
		if err != nil {
			t.Fatal(err)
		}
		s.applied(t, changed.Msg.GetChange())
		if got := recordNames(s); !slices.Equal(got, []string{"site.example.com"}) {
			t.Fatalf("the alias should be the only published record: %v", got)
		}
	}
	update(&dockwellv1.WebsiteDomainFront{
		Domain: "site.example.com", Tls: "off", TrustedProxies: []string{"127.0.0.1"}, Publish: throughWebOne(),
	})
	spec.GetProxy().Port = 3001
	update(nil)
}

func TestWebsiteDomainFrontRefusesAProviderOutsideItsProject(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		wc := websiteClient(s.b, s.b.origin)
		spec := onPort3000()
		made, err := wc.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
			Domain: "site.example.org", Website: spec,
		}))
		if err != nil {
			t.Fatal(err)
		}
		ch := made.Msg.GetChange()
		s.applied(t, ch)
		var site string
		for _, r := range ch.GetPlan().GetResources() {
			if r.GetResource().GetKind() == "project" {
				site = r.GetId()
			}
		}
		if site == "" {
			t.Fatal("the website has no project")
		}
		spec.RedirectFrom = []string{"site.example.com"}
		before := len(s.cf.Calls())
		_, err = wc.UpdateWebsite(t.Context(), connect.NewRequest(&dockwellv1.UpdateWebsiteRequest{
			Id: site, Website: spec, DomainFront: &dockwellv1.WebsiteDomainFront{
				Domain: "site.example.com", Tls: "off", TrustedProxies: []string{"127.0.0.1"}, Publish: throughWebOne(),
			},
		}))
		if err == nil || !strings.Contains(err.Error(), "doesn't serve this route's project") {
			t.Fatalf("domainFront should be outside the provider's reach: %v", err)
		}
		if len(s.cf.Calls()) != before {
			t.Fatalf("the website plan contacted Cloudflare: %v", s.cf.Calls()[before:])
		}
	})
}
