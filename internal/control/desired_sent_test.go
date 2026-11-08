// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
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
}
