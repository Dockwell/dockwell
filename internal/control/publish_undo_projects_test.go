// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"testing"

	"connectrpc.com/connect"

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
