// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/reconciled"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A fill that sent a server's agent values is a server event on the timeline,
// with its counts and whether the cache was new, and never the cache's ID (ADR 0054).
func TestAFillIsOnTheServersTimeline(t *testing.T) {
	storetest.Each(t, testAFillIsOnTheServersTimeline)
}

func testAFillIsOnTheServersTimeline(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	s := f.addNamedServer(t, "oci-1", false)
	detail, err := json.Marshal(revision.DesiredSent{Resources: 4, WithValues: 3, Cache: "cache-id-stays-behind", NewCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		return tx.RecordEvent(t.Context(), store.Event{
			ID: id.New(id.Event), At: time.Now(), Actor: revision.SystemActor, Action: revision.ActionDesiredSent,
			Subject: s.id.ServerID, Target: s.id.ServerID, Surface: revision.ViaAutomatic, Detail: string(detail),
		})
	}); err != nil {
		t.Fatal(err)
	}
	events := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SERVER}, ServerId: s.id.ServerID,
	})
	var sent *dockwellv1.Event
	for _, e := range events {
		if e.GetAction() == revision.ActionDesiredSent {
			sent = e
		}
	}
	got := sent.GetDesiredSent()
	if sent == nil || sent.GetKind() != dockwellv1.EventKind_EVENT_KIND_SERVER || sent.GetSubject().GetName() != "oci-1" ||
		got.GetResources() != 4 || got.GetWithValues() != 3 || !got.GetNewCache() || got.GetServerName() != "oci-1" {
		t.Fatalf("the timeline has %v", sent)
	}
	b2, err := protojson.Marshal(sent)
	if err != nil || strings.Contains(string(b2), "cache-id-stays-behind") {
		t.Errorf("the event carries the cache's ID: %s, %v", b2, err)
	}

	// What the agent keeps that the store doesn't know reads with its count.
	detail, err = json.Marshal(revision.DesiredSent{Unknown: 2, Cache: "cache-id-stays-behind"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		return tx.RecordEvent(t.Context(), store.Event{
			ID: id.New(id.Event), At: time.Now(), Actor: revision.SystemActor, Action: revision.ActionDesiredUnknown,
			Subject: s.id.ServerID, Target: s.id.ServerID, Surface: revision.ViaAutomatic, Detail: string(detail),
		})
	}); err != nil {
		t.Fatal(err)
	}
	events = listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SERVER}, ServerId: s.id.ServerID,
	})
	var unknown *dockwellv1.Event
	for _, e := range events {
		if e.GetAction() == revision.ActionDesiredUnknown {
			unknown = e
		}
	}
	if unknown.GetDesiredSent().GetUnknown() != 2 || unknown.GetKind() != dockwellv1.EventKind_EVENT_KIND_SERVER {
		t.Errorf("the timeline has %v", unknown)
	}
}

// What a server's agent did on its own reads through the API as server events, each
// with the resource and why, as the recorder kept them (ADR 0054).
func TestWhatAnAgentDidOnItsOwnIsOnTheTimeline(t *testing.T) {
	storetest.Each(t, testWhatAnAgentDidOnItsOwnIsOnTheTimeline)
}

func testWhatAnAgentDidOnItsOwnIsOnTheTimeline(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	s := f.addNamedServer(t, "oci-1", false)
	web := &dockwellv1.ResourceRef{Kind: "container", Scope: s.id.ServerID, Name: "web"}
	if err := (&reconciled.Recorder{DB: f.store}).Heard(t.Context(), s.id.ServerID, &agentv1.Reconciled{Dropped: 2, Entries: []*agentv1.LocalEntry{
		{Id: "boot-1", Resource: web, Action: agentv1.LocalAction_LOCAL_ACTION_MADE, Time: timestamppb.Now()},
		{Id: "boot-2", Resource: web, Action: agentv1.LocalAction_LOCAL_ACTION_WAITING, Reason: "its image is gone", Time: timestamppb.Now()},
	}}); err != nil {
		t.Fatal(err)
	}
	events := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SERVER}, ServerId: s.id.ServerID,
	})
	got := map[string]*dockwellv1.Event{}
	for _, e := range events {
		got[e.GetAction()] = e
	}
	made, waiting, dropped := got[reconciled.ActionMade], got[reconciled.ActionWaiting], got[reconciled.ActionDropped]
	if made.GetKind() != dockwellv1.EventKind_EVENT_KIND_SERVER || made.GetReconciled().GetName() != "web" || made.GetReconciled().GetServerName() != "oci-1" {
		t.Errorf("made: %v", made)
	}
	if waiting.GetReconciled().GetReason() != "its image is gone" || dropped.GetReconciled().GetDropped() != 2 {
		t.Errorf("waiting %v, dropped %v", waiting, dropped)
	}
}

// What an agent kept and did on its own names resources by kind and name,
// whichever project they're in, so those entries are the install's: an admin's
// and the owner's. A role below admin reads none of them, on the install or in
// a project, whether they ask for everything or for the server (ADR 0049, ADR
// 0054).
func TestWhatAnAgentDidOnItsOwnIsAnAdminsToRead(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		other := &dockwellv1.ResourceRef{Kind: "service", Scope: s.blogID, Name: "another-projects-app"}
		if err := (&reconciled.Recorder{DB: s.f.store}).Heard(ctx, s.server, &agentv1.Reconciled{Dropped: 1, Entries: []*agentv1.LocalEntry{
			{Id: "boot-1", Resource: other, Action: agentv1.LocalAction_LOCAL_ACTION_MADE, Time: timestamppb.Now()},
			{Id: "boot-2", Resource: other, Action: agentv1.LocalAction_LOCAL_ACTION_WAITING, Reason: "its image is gone", Time: timestamppb.Now()},
		}}); err != nil {
			t.Fatal(err)
		}
		detail, err := json.Marshal(revision.DesiredSent{Resources: 4, WithValues: 3})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.f.store.Update(ctx, func(tx *store.Tx) error {
			return tx.RecordEvent(ctx, store.Event{
				ID: id.New(id.Event), At: time.Now(), Actor: revision.SystemActor, Action: revision.ActionDesiredSent,
				Subject: s.server, Target: s.server, Surface: revision.ViaAutomatic, Detail: string(detail),
			})
		}); err != nil {
			t.Fatal(err)
		}
		own := func(e *dockwellv1.Event) bool {
			return e.GetReconciled() != nil || e.GetDesiredSent() != nil || strings.HasPrefix(e.GetAction(), "server.reconciled") || strings.HasPrefix(e.GetAction(), "server.desired")
		}
		events := eventClient(s.b, s.b.origin)
		count := func(req *dockwellv1.ListEventsRequest) (int, error) {
			res, err := events.ListEvents(ctx, connect.NewRequest(req))
			if err != nil {
				return 0, err
			}
			n := 0
			for _, e := range res.Msg.GetEvents() {
				if own(e) {
					n++
				}
			}
			return n, nil
		}
		// The owner reads all four: made, waiting, the dropped count and the fill.
		if n, err := count(&dockwellv1.ListEventsRequest{}); err != nil || n != 4 {
			t.Fatalf("the owner reads %d of them, %v; want 4", n, err)
		}
		for _, who := range []struct {
			name   string
			grants []store.Grant
		}{
			{"a viewer on the install", []store.Grant{onInstall(store.RoleViewer)}},
			{"a developer on the install", []store.Grant{onInstall(store.RoleDeveloper)}},
			{"a viewer of the project the resource is in", []store.Grant{{ProjectID: s.blogID, Role: store.RoleViewer}}},
			{"a developer of another project", []store.Grant{{ProjectID: s.defaultID, Role: store.RoleDeveloper}}},
		} {
			s.hold(who.grants...)
			if n, err := count(&dockwellv1.ListEventsRequest{}); err != nil || n != 0 {
				t.Errorf("%s reads %d of them in everything, %v; want none", who.name, n, err)
			}
			// Asked for by server, which is the install's, they're refused or shown none.
			if n, err := count(&dockwellv1.ListEventsRequest{ServerId: s.server}); err == nil && n != 0 {
				t.Errorf("%s reads %d of them asking for the server; want none", who.name, n)
			}
			if n, err := count(&dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SERVER}}); err == nil && n != 0 {
				t.Errorf("%s reads %d of them asking for server events; want none", who.name, n)
			}
		}
		s.hold(onInstall(store.RoleAdmin))
		if n, err := count(&dockwellv1.ListEventsRequest{}); err != nil || n != 4 {
			t.Errorf("an admin reads %d of them, %v; want 4", n, err)
		}
	})
}
