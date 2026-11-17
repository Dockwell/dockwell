// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// takingOver is wiki's publish through web-1 with consent to take over the tunnel's
// rule for wiki.example.com, made by hand.
func (s *publishStand) takingOver(name, domain string) *dockwellv1.Edit {
	e := s.published(name, domain)
	pub := e.GetSpec().GetRoute().GetPublish()
	pub.TakesOver = append(pub.TakesOver, &resourcev1.TakeOver{Tunnel: webOne, Hostname: domain})
	return e
}

func stepsOf(ch *dockwellv1.Change, action dockwellv1.ProviderAction) []*dockwellv1.ProviderStep {
	return slices.DeleteFunc(providerSteps(ch), func(p *dockwellv1.ProviderStep) bool { return p.GetAction() != action })
}

func takesOver(ch *dockwellv1.Change, name string) bool {
	return slices.ContainsFunc(ch.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
		return slices.ContainsFunc(r.GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
			return a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_CLOUDFLARE_TAKEOVER && a.GetPath() == name
		})
	})
}

// withAppsPort makes 8080 a port the stand's app publishes on its server.
func withAppsPort(s *publishStand) {
	s.f.server.publisher.HostPorts = func(context.Context, string, string) ([]uint32, error) { return []uint32{8080}, nil }
}

// A rule made by hand that sends wiki.example.com to the server's proxy already is
// taken on with an admin's consent, writing nothing; removing the route takes it
// out, and undo puts it back whole, where it was. Consent taken away lets go of it,
// leaving it as it is (ADR 0104, section 8).
func TestARuleMadeByHandIsTakenOnAndPutBack(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		byHand := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},{"hostname":"wiki.example.com","service":"http://127.0.0.1:80","originRequest":{"noTLSVerify":true}},{"service":"http_status:404"}],"originRequest":{"connectTimeout":10}}`
		s.cf.SetDocument(webOne, json.RawMessage(byHand))

		// Without consent, it's left alone: nothing in the tunnel to write.
		if steps := stepsOf(propose(t, s.person, "Publish wiki", s.published("wiki", "wiki.example.com")), dockwellv1.ProviderAction_PROVIDER_ACTION_TAKE_ON_RULE); len(steps) != 0 {
			t.Fatalf("taken on without consent: %v", steps)
		}
		ch := propose(t, s.person, "Publish wiki", s.takingOver("wiki", "wiki.example.com"))
		if steps := stepsOf(ch, dockwellv1.ProviderAction_PROVIDER_ACTION_TAKE_ON_RULE); len(steps) != 1 || steps[0].GetWas() != "http://127.0.0.1:80" || !takesOver(ch, "wiki.example.com") {
			t.Fatalf("taking it on: %v", providerSteps(ch))
		}
		s.applied(t, ch)
		if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(byHand)) {
			t.Fatalf("taking on wrote: %s", doc)
		}

		gone := propose(t, s.person, "Remove wiki", s.removed("wiki"))
		if steps := stepsOf(gone, dockwellv1.ProviderAction_PROVIDER_ACTION_REMOVE_RULE); len(steps) != 1 || !strings.Contains(steps[0].GetWas(), "made in Cloudflare and taken on") {
			t.Fatalf("removing it: %v", providerSteps(gone))
		}
		removed := s.applied(t, gone)
		if got := rulesOf(t, s.cf); slices.Contains(got, "wiki.example.com=http://127.0.0.1:80") || len(got) != 2 {
			t.Fatalf("after removal: %v", got)
		}
		undo, err := s.person.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: removed.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		if steps := stepsOf(undo.Msg.GetChange(), dockwellv1.ProviderAction_PROVIDER_ACTION_RESTORE_RULE); len(steps) != 1 {
			t.Fatalf("undo's plan: %v", providerSteps(undo.Msg.GetChange()))
		}
		s.applied(t, undo.Msg.GetChange())
		if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(byHand)) {
			t.Fatalf("undo should put it back whole, where it was: %s", doc)
		}

		// The route without consent lets go of it, as it is.
		without := propose(t, s.person, "Publish wiki as it is", s.published("wiki", "wiki.example.com"))
		if steps := stepsOf(without, dockwellv1.ProviderAction_PROVIDER_ACTION_KEEP_RULE); len(steps) != 1 || !strings.Contains(steps[0].GetRouting(), "lets go of it") {
			t.Fatalf("consent taken away: %v", providerSteps(without))
		}
		s.applied(t, without)
		if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(byHand)) {
			t.Fatalf("letting go wrote: %s", doc)
		}
		if steps := stepsOf(propose(t, s.person, "Remove wiki", s.removed("wiki")), dockwellv1.ProviderAction_PROVIDER_ACTION_REMOVE_RULE); len(steps) != 0 {
			t.Fatalf("a rule let go is removed: %v", steps)
		}
	})
}

// A rule made by hand that points at the app's own port is moved to Dockwell with
// an admin's consent: taken out where the zone's rule serves the name already, and
// put back whole, where it was, when the route goes. Without consent, the plan says
// what an admin can do (ADR 0104, section 8).
func TestMovingARuleToDockwellByTakingItOut(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		withAppsPort(s)
		s.applied(t, propose(t, s.person, "Publish docs", s.published("docs", "docs.example.com")))
		doc, _ := s.cf.Document(webOne)
		withHand := strings.Replace(string(doc), `{"hostname":"*.example.com"`, `{"hostname":"wiki.example.com","service":"http://127.0.0.1:8080","originRequest":{"noTLSVerify":true}},{"hostname":"*.example.com"`, 1)
		s.cf.SetDocument(webOne, json.RawMessage(withHand))

		_, err := s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Publish wiki", Edits: []*dockwellv1.Edit{s.published("wiki", "wiki.example.com")}}))
		if err == nil || !strings.Contains(err.Error(), "points at the app's own port, http://127.0.0.1:8080") || !strings.Contains(err.Error(), "move it to Dockwell") {
			t.Fatalf("without consent: %v", err)
		}
		ch := propose(t, s.person, "Publish wiki", s.takingOver("wiki", "wiki.example.com"))
		if steps := stepsOf(ch, dockwellv1.ProviderAction_PROVIDER_ACTION_MOVE_RULE); len(steps) != 1 || !strings.HasPrefix(steps[0].GetRouting(), "Takes it out") || !takesOver(ch, "wiki.example.com") {
			t.Fatalf("moving it: %v", providerSteps(ch))
		}
		s.applied(t, ch)
		if got := rulesOf(t, s.cf); slices.ContainsFunc(got, func(r string) bool { return strings.HasPrefix(r, "wiki.example.com=") }) {
			t.Fatalf("the rule should be out: %v", got)
		}
		s.applied(t, propose(t, s.person, "Remove wiki", s.removed("wiki")))
		if after, _ := s.cf.Document(webOne); !jsonEqual(t, after, json.RawMessage(withHand)) {
			t.Fatalf("removing the route should put the rule back whole, where it was: %s", after)
		}
	})
}

// With no other rule to serve the name, Move it to Dockwell points the rule at the
// server's proxy, keeping the rest of it, and removing the route points it back.
func TestMovingARuleToDockwellByPointingIt(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		withAppsPort(s)
		withHand := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},{"hostname":"wiki.example.com","service":"http://127.0.0.1:8080","originRequest":{"noTLSVerify":true}},{"service":"http_status:404"}]}`
		s.cf.SetDocument(webOne, json.RawMessage(withHand))
		ch := propose(t, s.person, "Publish wiki", s.takingOver("wiki", "wiki.example.com"))
		if steps := stepsOf(ch, dockwellv1.ProviderAction_PROVIDER_ACTION_MOVE_RULE); len(steps) != 1 || !strings.HasPrefix(steps[0].GetRouting(), "Points it at the server's proxy") {
			t.Fatalf("moving it: %v", providerSteps(ch))
		}
		s.applied(t, ch)
		doc, _ := s.cf.Document(webOne)
		if got := rulesOf(t, s.cf); !slices.Contains(got, "wiki.example.com=http://127.0.0.1:80") || !strings.Contains(string(doc), `"noTLSVerify":true`) {
			t.Fatalf("the rule should point at the proxy, keeping the rest: %s", doc)
		}
		gone := propose(t, s.person, "Remove wiki", s.removed("wiki"))
		if steps := stepsOf(gone, dockwellv1.ProviderAction_PROVIDER_ACTION_RESTORE_RULE); len(steps) != 1 {
			t.Fatalf("removing it: %v", providerSteps(gone))
		}
		s.applied(t, gone)
		if after, _ := s.cf.Document(webOne); !jsonEqual(t, after, json.RawMessage(withHand)) {
			t.Fatalf("removing the route should point it back: %s", after)
		}
	})
}
