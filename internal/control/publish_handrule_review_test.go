// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// unpublished is a route that serves domain through the tunnel's way in, publishing
// nothing at Cloudflare, as one served by rules and records made by hand is.
func (s *publishStand) unpublished(name, domain string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(route.Kind), Scope: s.env, Name: name},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: &resourcev1.Route{
			Domain: domain, Service: "web", Port: 8080, Tls: "off", TrustedProxies: []string{"127.0.0.1"},
		}}},
	}
}

// holdsRule says the store holds a rule for hostname in web-1.
func (s *publishStand) holdsRule(t *testing.T, hostname string) bool {
	t.Helper()
	found := true
	_ = s.f.store.View(t.Context(), func(tx *store.Tx) error {
		_, err := tx.CloudflareRuleAt(t.Context(), webOne, hostname, "")
		found = err == nil
		return nil
	})
	return found
}

// cutShortMove leaves the store and the tunnel as a Move it to Dockwell cut short
// leaves them: the rule kept as moving, before its write, or after it, when moved.
func (s *publishStand) cutShortMove(t *testing.T, mode, proxy, original string, at int, moved json.RawMessage) {
	t.Helper()
	now := time.Now()
	kept, _ := json.Marshal(map[string]any{"elem": json.RawMessage(original), "at": at, "moved": true, "mode": mode})
	if err := s.f.store.Update(t.Context(), func(tx *store.Tx) error {
		return tx.PutCloudflareRule(t.Context(), store.CloudflareRule{
			TunnelID: webOne, Hostname: "wiki.example.com", Account: cftest.Account, Provider: "cloudflare", Zone: "example.com", Service: proxy,
			ChangeID: "chg_cut", TakenOnAt: &now, Replaced: string(kept), State: store.CloudflareRuleMoving, CreatedAt: now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if moved != nil {
		s.cf.SetDocument(webOne, moved)
	}
}

const wikiByHand = `{"hostname":"wiki.example.com","service":"http://127.0.0.1:8080","originRequest":{"noTLSVerify":true}}`

// A Move it to Dockwell cut short, before or after its write to the tunnel, in either
// mode: planned again, it finishes from what was kept, and removing the route puts
// the rule made by hand back as it was (the review's H1).
func TestAMoveCutShortFinishesAndPutsTheRuleBack(t *testing.T) {
	t.Parallel()
	withHand := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},` + wikiByHand + `,{"service":"http_status:404"}]}`
	pointed := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},{"hostname":"wiki.example.com","service":"http://127.0.0.1:80","originRequest":{"noTLSVerify":true}},{"service":"http_status:404"}]}`
	for _, c := range []struct {
		name  string
		moved bool
	}{{"before the write", false}, {"after the write", true}} {
		t.Run("pointed at the proxy, "+c.name, func(t *testing.T) {
			t.Parallel()
			storetest.Each(t, func(t *testing.T, backend store.Backend) {
				s := newPublishStandOn(t, backend)
				withAppsPort(s)
				s.cf.SetDocument(webOne, json.RawMessage(withHand))
				var after json.RawMessage
				if c.moved {
					after = json.RawMessage(pointed)
				}
				s.cutShortMove(t, "proxy", "http://127.0.0.1:80", wikiByHand, 1, after)
				again := propose(t, s.person, "Publish wiki", s.takingOver("wiki"))
				if steps := stepsOf(again, dockwellv1.ProviderAction_PROVIDER_ACTION_MOVE_RULE); len(steps) != 1 {
					t.Fatalf("planned again, it finishes the move: %v", providerSteps(again))
				}
				if steps := stepsOf(again, dockwellv1.ProviderAction_PROVIDER_ACTION_TAKE_ON_RULE); len(steps) != 0 {
					t.Fatalf("the moved rule was taken on as the original: %v", steps)
				}
				s.applied(t, again)
				if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(pointed)) {
					t.Fatalf("moved: %s", doc)
				}
				s.applied(t, propose(t, s.person, "Remove wiki", s.removed("wiki")))
				if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(withHand)) {
					t.Fatalf("the rule made by hand should be back as it was: %s", doc)
				}
			})
		})
		t.Run("taken out, "+c.name, func(t *testing.T) {
			t.Parallel()
			storetest.Each(t, func(t *testing.T, backend store.Backend) {
				s := newPublishStandOn(t, backend)
				withAppsPort(s)
				s.applied(t, propose(t, s.person, "Publish docs", s.published("docs", "docs.example.com")))
				doc, _ := s.cf.Document(webOne)
				zoned := string(doc)
				byHand := strings.Replace(zoned, `{"hostname":"*.example.com"`, wikiByHand+`,{"hostname":"*.example.com"`, 1)
				s.cf.SetDocument(webOne, json.RawMessage(byHand))
				var after json.RawMessage
				if c.moved {
					after = json.RawMessage(zoned)
				}
				s.cutShortMove(t, "remove", "http://127.0.0.1:80", wikiByHand, 1, after)
				again := propose(t, s.person, "Publish wiki", s.takingOver("wiki"))
				if steps := stepsOf(again, dockwellv1.ProviderAction_PROVIDER_ACTION_MOVE_RULE); len(steps) != 1 {
					t.Fatalf("planned again, it finishes the move: %v", providerSteps(again))
				}
				s.applied(t, again)
				if got := rulesOf(t, s.cf); slices.ContainsFunc(got, func(r string) bool { return strings.HasPrefix(r, "wiki.example.com=") }) {
					t.Fatalf("the rule should be out: %v", got)
				}
				s.applied(t, propose(t, s.person, "Remove wiki", s.removed("wiki")))
				if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(byHand)) {
					t.Fatalf("the rule made by hand should be back where it was: %s", doc)
				}
			})
		})
	}
}

// A route that serves wiki.example.com through a rule and a record made by hand,
// publishing nothing, takes both on; undoing that lets go of both and writes
// nothing, so the name goes on working. An edit that stops publishing but keeps the
// domain does the same (the review's H2).
func TestUndoingATakeOnLeavesTheNameWorking(t *testing.T) {
	t.Parallel()
	byHand := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},{"hostname":"wiki.example.com","service":"http://127.0.0.1:80"},{"service":"http_status:404"}]}`
	for _, takesRecord := range []bool{true, false} {
		t.Run(map[bool]string{true: "with the record taken over", false: "with the record written"}[takesRecord], func(t *testing.T) {
			t.Parallel()
			storetest.Each(t, func(t *testing.T, backend store.Backend) {
				s := newPublishStandOn(t, backend)
				s.cf.SetDocument(webOne, json.RawMessage(byHand))
				id := ""
				if takesRecord {
					id = s.cf.Put("example.com", cftest.Record{Type: "CNAME", Name: "wiki.example.com", Content: webOne + ".cfargotunnel.com", Proxied: true, TTL: 1, Comment: "by hand"})
				}
				s.applied(t, propose(t, s.person, "Serve wiki", s.unpublished("wiki", "wiki.example.com")))
				edit := s.takingOver("wiki")
				if id != "" {
					pub := edit.GetSpec().GetRoute().GetPublish()
					pub.TakesOver = append(pub.TakesOver, &resourcev1.TakeOver{RecordId: id})
				}
				done := s.applied(t, propose(t, s.person, "Manage wiki in Dockwell", edit))
				undo, err := s.person.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: done.GetId()}))
				if err != nil {
					t.Fatal(err)
				}
				steps := providerSteps(undo.Msg.GetChange())
				if removes := stepsOf(undo.Msg.GetChange(), dockwellv1.ProviderAction_PROVIDER_ACTION_REMOVE_RULE); len(removes) != 0 {
					t.Fatalf("undo takes the rule out: %v", steps)
				}
				s.applied(t, undo.Msg.GetChange())
				if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(byHand)) {
					t.Fatalf("the rule made by hand should stay as it is: %s", doc)
				}
				if s.holdsRule(t, "wiki.example.com") {
					t.Fatal("the rule should be let go")
				}
				records := s.cf.Records("example.com")
				if takesRecord && (len(records) != 1 || records[0].Comment != "by hand") {
					t.Fatalf("the record made by hand should stay: %+v", records)
				}
				if !takesRecord && len(records) != 0 {
					t.Fatalf("undo should take out the record its change wrote: %+v", records)
				}
			})
		})
	}
	t.Run("an edit that stops publishing", func(t *testing.T) {
		t.Parallel()
		storetest.Each(t, func(t *testing.T, backend store.Backend) {
			s := newPublishStandOn(t, backend)
			s.cf.SetDocument(webOne, json.RawMessage(byHand))
			s.applied(t, propose(t, s.person, "Publish wiki", s.takingOver("wiki")))
			ch := propose(t, s.person, "Stop publishing wiki", s.unpublished("wiki", "wiki.example.com"))
			if removes := append(stepsOf(ch, dockwellv1.ProviderAction_PROVIDER_ACTION_REMOVE_RULE), stepsOf(ch, dockwellv1.ProviderAction_PROVIDER_ACTION_DELETE_RECORD)...); slices.ContainsFunc(removes, func(p *dockwellv1.ProviderStep) bool {
				return !strings.Contains(p.GetRouting(), "lets go of it")
			}) {
				t.Fatalf("an edit that keeps the domain takes something out: %v", providerSteps(ch))
			}
			s.applied(t, ch)
			if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(byHand)) {
				t.Fatalf("the rule should stay: %s", doc)
			}
			if records := s.cf.Records("example.com"); len(records) != 1 {
				t.Fatalf("the record the name needs should stay: %+v", records)
			}
		})
	})
}

// One change moves wiki.example.com from wiki to wiki2: the rule made by hand that
// wiki took on stays as it is, and Dockwell's record goes on with wiki2 (the
// review's M1).
func TestADomainMovedToAnotherRouteKeepsWhatsThere(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		byHand := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},{"hostname":"wiki.example.com","service":"http://127.0.0.1:80"},{"service":"http_status:404"}]}`
		s.cf.SetDocument(webOne, json.RawMessage(byHand))
		s.applied(t, propose(t, s.person, "Publish wiki", s.takingOver("wiki")))
		before := s.cf.Records("example.com")
		if len(before) != 1 {
			t.Fatalf("before: %+v", before)
		}
		s.applied(t, propose(t, s.person, "Move wiki's domain", s.removed("wiki"), s.takingOver("wiki2")))
		if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(byHand)) {
			t.Fatalf("the rule should stay: %s", doc)
		}
		after := s.cf.Records("example.com")
		if len(after) != 1 || after[0].ID != before[0].ID {
			t.Fatalf("the record should go on as it is: %+v", after)
		}
		// It's wiki2's now: removing wiki2 removes it.
		s.applied(t, propose(t, s.person, "Remove wiki2", s.removed("wiki2")))
		if left := s.cf.Records("example.com"); len(left) != 0 {
			t.Fatalf("removing wiki2 should take its record: %+v", left)
		}
	})
}

// A rule put back finds the name taken by a rule made again by hand since: it's left
// as it is and not held, and the step says why (the review's M4).
func TestPuttingARuleBackLeavesOneMadeAgainByHand(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		now := time.Now()
		kept, _ := json.Marshal(map[string]any{"elem": json.RawMessage(`{"hostname":"wiki.example.com","service":"http://127.0.0.1:80"}`), "at": 1})
		if err := s.f.store.Update(t.Context(), func(tx *store.Tx) error {
			return tx.PutCloudflareRule(t.Context(), store.CloudflareRule{
				TunnelID: webOne, Hostname: "wiki.example.com", Account: cftest.Account, Provider: "cloudflare", Zone: "example.com", Service: "http://127.0.0.1:80",
				ChangeID: "chg_old", TakenOnAt: &now, Replaced: string(kept), State: store.CloudflareRuleKept, CreatedAt: now,
			})
		}); err != nil {
			t.Fatal(err)
		}
		madeAgain := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},{"hostname":"wiki.example.com","service":"http://192.0.2.10:3000"},{"service":"http_status:404"}]}`
		s.cf.SetDocument(webOne, json.RawMessage(madeAgain))
		said, err := s.f.server.publisher.Apply(t.Context(), "chg_undo", revision.PublishRoute{ID: "rte_wiki"}, resource.ProviderStep{
			Provider: "cloudflare", Action: resource.ProviderRestoreRule, Target: "web-1", Name: "wiki.example.com", Account: cftest.Account, Tunnel: webOne,
			ZoneID: cftest.ZoneID("example.com"), Mode: "hold", Rule: &resource.ProviderRule{Hostname: "wiki.example.com", Service: "http://127.0.0.1:80"},
		})
		if err != nil || !strings.Contains(said, "was made again in Cloudflare, so it stays as it is") {
			t.Fatalf("putting it back: %q, %v", said, err)
		}
		if s.holdsRule(t, "wiki.example.com") {
			t.Fatal("a rule made again by hand was taken on")
		}
		if doc, _ := s.cf.Document(webOne); !jsonEqual(t, doc, json.RawMessage(madeAgain)) {
			t.Fatalf("the rule made again should stay: %s", doc)
		}
	})
}
