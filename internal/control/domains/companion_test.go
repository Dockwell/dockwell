// SPDX-License-Identifier: AGPL-3.0-only

package domains

import (
	"cmp"
	"context"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/certificates"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A registered domain's companion is its www name, and the other way round, by the
// public suffix list; any other name has none.
func TestTheCompanionIsTheOtherHalfOfARegisteredDomain(t *testing.T) {
	for domain, want := range map[string]string{
		"example.com":          "www.example.com",
		"www.example.com":      "example.com",
		"example.co.uk":        "www.example.co.uk",
		"www.example.co.uk":    "example.co.uk",
		"blog.example.com":     "",
		"www.blog.example.com": "",
		"co.uk":                "",
	} {
		if got := Companion(domain); got != want {
			t.Errorf("Companion(%q) = %q, want %q", domain, got, want)
		}
	}
}

func TestTwoNamesAnswerAlike(t *testing.T) {
	here := func(found ...string) *resource.DomainCheck {
		return &resource.DomainCheck{State: resource.DomainHere, Found: found}
	}
	elsewhere := func(proxied bool, found ...string) *resource.DomainCheck {
		return &resource.DomainCheck{State: resource.DomainElsewhere, Proxied: proxied, Found: found}
	}
	for _, c := range []struct {
		name string
		a, b *resource.DomainCheck
		want bool
	}{
		{"both here", here("203.0.113.50"), here("203.0.113.50"), true},
		{"one here", here("203.0.113.50"), elsewhere(false, "198.51.100.7"), false},
		{"the same addresses elsewhere", elsewhere(false, "198.51.100.7", "198.51.100.8"), elsewhere(false, "198.51.100.8", "198.51.100.7"), true},
		{"other addresses elsewhere", elsewhere(false, "198.51.100.7"), elsewhere(false, "198.51.100.9"), false},
		{"both behind Cloudflare's proxy", elsewhere(true, "198.51.100.7"), elsewhere(true, "198.51.100.9"), true},
		{"no record", here("203.0.113.50"), &resource.DomainCheck{State: resource.DomainNoRecord}, false},
	} {
		if got := answersAlike(c.a, c.b); got != c.want {
			t.Errorf("%s: alike %t, want %t", c.name, got, c.want)
		}
	}
}

// A shared wildcard that serves the project covers its companion, and nothing else
// does: a server's own certificate, one for other projects, or a name it doesn't cover.
func TestAWildcardCoversTheCompanion(t *testing.T) {
	list := []certificates.Standing{
		{Holder: "srv_A", Names: []string{"*.example.com"}, Projects: []string{"prj_A"}},
		{Holder: "crt_other", Names: []string{"*.example.com"}, Projects: []string{"prj_B"}},
		{Holder: "crt_A", Names: []string{"example.com", "*.example.com"}, Projects: []string{"prj_A"}},
	}
	if got := wildcardFor(list, "prj_A", "www.example.com"); got != "*.example.com" {
		t.Errorf("www.example.com is covered by %q", got)
	}
	for _, c := range []struct{ project, host string }{
		{"prj_A", "example.com"},
		{"prj_A", "www.blog.example.com"},
		{"prj_C", "www.example.com"},
		{"", "www.example.com"},
	} {
		if got := wildcardFor(list, c.project, c.host); got != "" {
			t.Errorf("%s in %s is covered by %q", c.host, c.project, got)
		}
	}
}

// CheckDomain reads the companion beside the domain: what DNS says of it, whether it
// answers as the domain does, and which project serves it already, named only to a
// caller who can read that project.
func TestACompanionSaysWhereItStands(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	logger := slog.New(slog.DiscardHandler)
	if _, err := revision.Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateServer(t.Context(), store.Server{ID: "srv_on", Name: "srv_on", CreatedAt: now, CertSerial: "srv_on", CertNotAfter: now})
	}); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(&service.Driver{}, &route.Driver{})
	e := revision.New(db, kinds, agents{}, secretstest.Keeper(db, kinds), logger)
	var tr scope.Tree
	if err := db.View(t.Context(), func(tx *store.Tx) (err error) {
		tr, err = scope.Load(t.Context(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	project := tr.Projects[0]
	env := project.Environments[0].ID
	p, err := e.Propose(t.Context(), revision.Proposal{Summary: "Serve the shop at www", Actor: "usr_test", Edits: []revision.Edit{
		{Ref: resource.Ref{Kind: route.Kind, Scope: env, Name: "www"}, Spec: &resourcev1.Route{Domain: "www.example.com", HostPort: 8080}, Servers: []string{"srv_on"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(t.Context(), p.Change); err != nil {
		t.Fatal(err)
	}
	h := New(kinds, db, reports{}, aServer{}, logger)
	h.Lookup = names(map[string]string{"example.com": "203.0.113.50", "www.example.com": "203.0.113.50", "example.org": "203.0.113.50"})
	ask := func(ctx context.Context, domain string) *dockwellv1.CheckDomainResponse {
		t.Helper()
		res, err := h.CheckDomain(ctx, connect.NewRequest(&dockwellv1.CheckDomainRequest{ServerId: "srv_A", Domain: domain}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}

	got := ask(t.Context(), "example.com")
	cc := got.GetCompanionCheck()
	if got.GetCompanion() != "www.example.com" || cc.GetCheck().GetState() != dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_POINTS_HERE || !cc.GetAnswersAlike() {
		t.Errorf("example.com's companion reads %q, %v", got.GetCompanion(), cc)
	}
	if want := cmp.Or(project.DisplayName, project.Name); cc.GetHeldBy() != want || cc.GetHeldByEnvironmentId() != env || cc.GetHeldFor() != "port 8080 on the server" {
		t.Errorf("www.example.com is held by %q in %q, want %q in %q", cc.GetHeldBy(), cc.GetHeldByEnvironmentId(), want, env)
	}

	// A developer of another project learns only that another project holds it.
	other := place.WithCaller(t.Context(), place.Caller{UserID: "usr_dev", Grants: []store.Grant{{UserID: "usr_dev", ProjectID: "prj_other", Role: store.RoleDeveloper}}})
	if cc := ask(other, "example.com").GetCompanionCheck(); cc.GetHeldBy() != "another project" || cc.GetHeldByEnvironmentId() != "" || cc.GetHeldFor() != "" {
		t.Errorf("to a developer of another project, it's held by %q in %q", cc.GetHeldBy(), cc.GetHeldByEnvironmentId())
	}

	// A companion nobody serves, with no record.
	if cc := ask(t.Context(), "example.org").GetCompanionCheck(); cc.GetHeldBy() != "" || cc.GetAnswersAlike() || cc.GetCheck().GetState() != dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_NO_RECORD {
		t.Errorf("www.example.org reads %v", cc)
	}
	if got := ask(t.Context(), "blog.example.com"); got.GetCompanion() != "" || got.GetCompanionCheck() != nil {
		t.Errorf("blog.example.com has a companion: %v", got)
	}
}
