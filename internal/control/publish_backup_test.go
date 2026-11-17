// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	"github.com/dockwell/dockwell/internal/control/dnsproviders/cftest"
	"github.com/dockwell/dockwell/internal/control/publish"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// What Dockwell keeps of a record made by hand that it replaced, its backup, is
// never lost: on every path a step can leave, it's either kept in a row or put back
// at Cloudflare (ADR 0104, and the re-check's H1a and H1b). Each test runs on SQLite,
// and on Postgres where DOCKWELL_TEST_POSTGRES names one.

// byHandCNAME is the record made by hand these tests replace.
var byHandCNAME = cftest.Record{Type: "CNAME", Name: "vpn.example.com", Content: "elsewhere.example.net", TTL: 300, Comment: "by hand"}

// withPublicAddress gives the stand's server a public address, so Own HTTPS can
// publish it.
func withPublicAddress(s *publishStand) {
	pub := s.f.server.publisher
	read := pub.Server
	pub.Server = func(ctx context.Context, server string) (publish.Facts, error) {
		f, err := read(ctx, server)
		f.Public = []netip.Addr{netip.MustParseAddr("198.51.100.7")}
		return f, err
	}
}

// backupHeld says the record made by hand with content is kept in a row of the
// store, the route's own or a pending one, or is at Cloudflare as its owner made it.
func (s *publishStand) backupHeld(t *testing.T, routeID, content string) (inRow, atCloudflare bool) {
	t.Helper()
	if err := s.f.store.View(t.Context(), func(tx *store.Tx) error {
		rows, err := tx.CloudflareRecordsAll(t.Context())
		if err != nil {
			return err
		}
		for i := range rows {
			inRow = inRow || strings.Contains(rows[i].Replaced, content)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kept := s.pendingRows(t, routeID)
	for i := range kept {
		inRow = inRow || strings.Contains(kept[i].Replaced, content)
	}
	for _, r := range s.cf.Records("example.com") {
		atCloudflare = atCloudflare || r.Content == content && r.Comment == "by hand"
	}
	return inRow, atCloudflare
}

// applyAndWait applies ch and returns how it ended, applied or not.
func (s *publishStand) applyAndWait(t *testing.T, ch *dockwellv1.Change) *dockwellv1.Change {
	t.Helper()
	w := watch(t, s.person, ch.GetId())
	if _, err := applyChange(t, s.person, &dockwellv1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}); err != nil {
		t.Fatal(err)
	}
	return waitWatch(t, w).last()
}

// A route on Own HTTPS that replaced a CNAME made by hand moves to the server's
// tunnel. The replace of its own A deletes the A at Cloudflare and stops before its
// write, as a restart cuts it short. Planned again, the write carries the backup on:
// the record made by hand is never lost, nor put back beside Dockwell's, and
// removing the route puts it back (the re-check's H1a).
func TestAReplaceAcrossTypesCutShortKeepsTheRecord(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		withPublicAddress(s)
		id := s.cf.Put("example.com", byHandCNAME)
		own := s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id})
		own.GetSpec().GetRoute().GetPublish().Way = resourcev1.RouteWay_ROUTE_WAY_OWN_HTTPS
		own.GetSpec().GetRoute().GetPublish().Tunnel = ""
		s.applied(t, propose(t, s.person, "Publish vpn on its own HTTPS", own))
		recs := s.cf.Records("example.com")
		if len(recs) != 1 || recs[0].Type != "A" {
			t.Fatalf("after Own HTTPS: %+v", recs)
		}
		routeID := routeIDOf(t, recs[0])
		// The first half of the replace of its own A: gone at Cloudflare, its row kept.
		s.cf.Remove("example.com", recs[0].ID)

		again := s.applyAndWait(t, propose(t, s.person, "Publish vpn through the tunnel", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id})))
		if inRow, there := s.backupHeld(t, routeID, byHandCNAME.Content); !inRow && !there {
			t.Fatalf("the record made by hand is lost: in no row and not at Cloudflare (the change ended %v: %s)", again.GetState(), again.GetError())
		}
		if again.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("planned again, the change ended %v: %s", again.GetState(), again.GetError())
		}
		if got := s.cf.Records("example.com"); len(got) != 1 || got[0].Content != webOne+".cfargotunnel.com" {
			t.Fatalf("Cloudflare after: %+v", got)
		}
		s.applied(t, propose(t, s.person, "Remove vpn", s.removed("vpn")))
		got := s.cf.Records("example.com")
		if len(got) != 1 || got[0].Content != byHandCNAME.Content || got[0].Comment != "by hand" || got[0].TTL != 300 {
			t.Fatalf("removing the route should put the record made by hand back: %+v", got)
		}
	})
}

// The same across the other way: an A made by hand, replaced through the tunnel, and
// the route moving to Own HTTPS, cut short after its CNAME is gone. The A made by
// hand is never put back beside Dockwell's, so the name answers with one address.
func TestAReplaceAcrossTypesCutShortNeverPutsTwoAddresses(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		withPublicAddress(s)
		id := s.cf.Put("example.com", cftest.Record{Type: "A", Name: "vpn.example.com", Content: "203.0.113.9", TTL: 300, Comment: "by hand"})
		s.applied(t, propose(t, s.person, "Publish vpn", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id})))
		recs := s.cf.Records("example.com")
		if len(recs) != 1 || recs[0].Type != "CNAME" {
			t.Fatalf("after the tunnel: %+v", recs)
		}
		routeID := routeIDOf(t, recs[0])
		s.cf.Remove("example.com", recs[0].ID)

		own := s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id})
		own.GetSpec().GetRoute().GetPublish().Way = resourcev1.RouteWay_ROUTE_WAY_OWN_HTTPS
		own.GetSpec().GetRoute().GetPublish().Tunnel = ""
		again := s.applyAndWait(t, propose(t, s.person, "Publish vpn on its own HTTPS", own))
		if again.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("planned again, the change ended %v: %s", again.GetState(), again.GetError())
		}
		got := s.cf.Records("example.com")
		if len(got) != 1 || got[0].Content != "198.51.100.7" {
			t.Fatalf("the name should answer with Dockwell's address alone: %+v", got)
		}
		if inRow, _ := s.backupHeld(t, routeID, "203.0.113.9"); !inRow {
			t.Fatal("the A made by hand should be kept in a row")
		}
		s.applied(t, propose(t, s.person, "Remove vpn", s.removed("vpn")))
		if got := s.cf.Records("example.com"); len(got) != 1 || got[0].Content != "203.0.113.9" || got[0].Comment != "by hand" {
			t.Fatalf("removing the route should put the A made by hand back: %+v", got)
		}
	})
}

// Dockwell replaced a CNAME made by hand with consent, and someone turns the orange
// cloud off on Dockwell's record. Removing the route lets that record go as changed
// in Cloudflare and keeps its backup; putting it back skips, saying why, and the
// change applies (the re-check's H1b).
func TestARecordLetGoKeepsWhatItReplaced(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		id := s.cf.Put("example.com", byHandCNAME)
		s.applied(t, propose(t, s.person, "Publish vpn", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id})))
		recs := s.cf.Records("example.com")
		if len(recs) != 1 || recs[0].Type != "CNAME" {
			t.Fatalf("after publishing: %+v", recs)
		}
		routeID := routeIDOf(t, recs[0])
		s.cf.EditInPlace("example.com", recs[0].ID, func(r *cftest.Record) { r.Proxied = false })

		done := s.applyAndWait(t, propose(t, s.person, "Remove vpn", s.removed("vpn")))
		inRow, there := s.backupHeld(t, routeID, byHandCNAME.Content)
		if !inRow && !there {
			t.Fatalf("the record made by hand is lost: in no row and not at Cloudflare (the change ended %v: %s)", done.GetState(), done.GetError())
		}
		if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the removal ended %v: %s", done.GetState(), done.GetError())
		}
		var said strings.Builder
		for _, st := range done.GetSteps() {
			said.WriteString(st.GetMessage() + "\n")
		}
		if !strings.Contains(said.String(), "vpn.example.com was changed in Cloudflare, so the record it had before Dockwell wasn't put back") {
			t.Fatalf("the steps should say why it wasn't put back:\n%s", said.String())
		}
		got := s.cf.Records("example.com")
		if len(got) != 1 || got[0].Proxied || got[0].Content != webOne+".cfargotunnel.com" {
			t.Fatalf("the record let go should stay as it was edited, alone: %+v", got)
		}
		if !inRow {
			t.Fatal("the record made by hand should be kept in a row")
		}
		// Undone, the route takes its own record back, and the backup goes with it.
		undo, err := s.person.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: done.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		back := s.applyAndWait(t, undo.Msg.GetChange())
		if inRow, there := s.backupHeld(t, routeID, byHandCNAME.Content); !inRow && !there {
			t.Fatalf("undo lost the record made by hand (it ended %v: %s)", back.GetState(), back.GetError())
		}
		if back.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("undo ended %v: %s", back.GetState(), back.GetError())
		}
	})
}

// A developer who proposes publishing through the tunnel, refused by the routing
// check, is told the tunnel's name and a fixed reason, never another rule's name or
// service (the re-check's M7a).
func TestBelowAnAdminAPlanRefusalSaysLittle(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		s.cf.SetDocument(webOne, []byte(`{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},{"service":"http://192.0.2.5:8080"}]}`))
		s.cf.Put("example.com", cftest.Record{Type: "CNAME", Name: "ftp.example.com", Content: webOne + ".cfargotunnel.com", Proxied: true, Comment: "by hand"})
		edit := s.published("wiki", "wiki.example.com")
		// An admin is told why.
		_, err := s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Publish wiki", Edits: []*dockwellv1.Edit{edit}}))
		if err == nil || !strings.Contains(err.Error(), "ftp.example.com") {
			t.Fatalf("an admin's refusal: %v", err)
		}
		hold := aMember(t, s.f)
		hold(store.Grant{ProjectID: s.project, Role: store.RoleDeveloper})
		_, err = s.person.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Publish wiki", Edits: []*dockwellv1.Edit{edit}}))
		if err == nil || strings.Contains(err.Error(), "192.0.2.5") || strings.Contains(err.Error(), "ftp.example.com") ||
			!strings.Contains(err.Error(), "can't publish wiki.example.com through web-1 now. An admin can see why") {
			t.Fatalf("a developer's refusal: %v", err)
		}
	})
}

// A replace step run again after its write was kept, as a resume may run one, finds
// the record it wrote and keeps what its row holds: the backup stays.
func TestAReplaceRunAgainKeepsTheBackup(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		id := s.cf.Put("example.com", byHandCNAME)
		s.applied(t, propose(t, s.person, "Publish vpn", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id})))
		recs := s.cf.Records("example.com")
		if len(recs) != 1 {
			t.Fatalf("after publishing: %+v", recs)
		}
		routeID := routeIDOf(t, recs[0])
		pub := s.f.server.publisher
		again := resource.ProviderStep{
			Provider: "cloudflare", Action: resource.ProviderReplaceRecord, Target: "example.com", Name: "vpn.example.com", ZoneID: cftest.ZoneID("example.com"),
			Record: &resource.ProviderRecord{ID: id, Type: "CNAME", Name: "vpn.example.com", Content: recs[0].Content, Proxied: true, TTL: 1, Comment: pub.Comment(routeID)},
		}
		if _, err := pub.Apply(t.Context(), "chg_again", revision.PublishRoute{ID: routeID}, again); err != nil {
			t.Fatal(err)
		}
		if inRow, _ := s.backupHeld(t, routeID, byHandCNAME.Content); !inRow {
			t.Fatal("running the replace again dropped the backup")
		}
	})
}

// Two records made by hand kept at one name, as a dual-stack name's A and AAAA
// replaced and then given back leave: removing the route keeps both, puts back the
// one the name has room for, and keeps the other listed, with no change failing on
// the store (migration 0047).
func TestTwoKeptBackupsAtOneName(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newPublishStandOn(t, backend)
		id := s.cf.Put("example.com", byHandCNAME)
		s.applied(t, propose(t, s.person, "Publish vpn", s.published("vpn", "vpn.example.com", &resourcev1.TakeOver{RecordId: id})))
		recs := s.cf.Records("example.com")
		if len(recs) != 1 {
			t.Fatalf("after publishing: %+v", recs)
		}
		routeID := routeIDOf(t, recs[0])
		// A second record made by hand, kept at the same name by a step a crash cut
		// short: an A, which can't sit beside the CNAME.
		other, err := json.Marshal(dnsproviders.Record{ID: "rec-hand-a", Type: "A", Name: "vpn.example.com", Content: "203.0.113.9", TTL: 300, Comment: "by hand too"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.f.store.Update(t.Context(), func(tx *store.Tx) error {
			return tx.PutCloudflareRecord(t.Context(), store.CloudflareRecord{
				ID: "pending:rec-hand-a", Provider: "cloudflare", ZoneID: cftest.ZoneID("example.com"), Zone: "example.com", Name: "vpn.example.com",
				Type: store.CloudflarePendingType, Content: "203.0.113.9", RouteID: routeID, ChangeID: "chg_cut", Replaced: string(other), State: "pending",
			})
		}); err != nil {
			t.Fatal(err)
		}
		done := s.applyAndWait(t, propose(t, s.person, "Remove vpn", s.removed("vpn")))
		cnameKept, cnameThere := s.backupHeld(t, routeID, byHandCNAME.Content)
		aKept, aThere := s.backupHeld(t, routeID, "203.0.113.9")
		got := s.cf.Records("example.com")
		t.Logf("the removal ended %v: %s; Cloudflare holds %+v", done.GetState(), done.GetError(), got)
		if !cnameKept && !cnameThere || !aKept && !aThere {
			t.Fatalf("a record made by hand is lost: CNAME kept %t there %t, A kept %t there %t", cnameKept, cnameThere, aKept, aThere)
		}
		if done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the removal ended %v: %s", done.GetState(), done.GetError())
		}
		if len(got) != 1 || got[0].Comment != "by hand" {
			t.Fatalf("the CNAME made by hand should be back, alone: %+v", got)
		}
		if !aKept {
			t.Fatal("the A made by hand should still be kept")
		}
	})
}
