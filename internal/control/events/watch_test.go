// SPDX-License-Identifier: AGPL-3.0-only

package events

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// Reading the store again skips what a watch has sent that can't have
// changed: an audit event, and a change that can't move on again from
// where it was. Open changes, and changes that moved in time, are read.
func TestAWatchSkipsWhatCantHaveChanged(t *testing.T) {
	at := time.Now()
	change := func(id string, state dockwellv1.ChangeState) *dockwellv1.Event {
		return &dockwellv1.Event{Id: id, Change: &dockwellv1.Change{Id: id, State: state, UndoDeadline: timestamppb.New(at.Add(time.Hour))}}
	}
	w := &watch{sent: map[string]sent{}}
	for _, e := range []*dockwellv1.Event{
		{Id: "evt_1"},
		change("chg_planned", dockwellv1.ChangeState_CHANGE_STATE_PLANNED),
		change("chg_applied", dockwellv1.ChangeState_CHANGE_STATE_APPLIED),
		change("chg_failed", dockwellv1.ChangeState_CHANGE_STATE_FAILED),
	} {
		w.saw(at, e)
	}
	for _, c := range []struct {
		p    store.TimelinePlace
		want bool
	}{
		{store.TimelinePlace{At: at, ID: "evt_1"}, true},
		{store.TimelinePlace{At: at, ID: "evt_2"}, false},
		{store.TimelinePlace{At: at, ID: "chg_planned", Change: true}, false},
		{store.TimelinePlace{At: at, ID: "chg_applied", Change: true}, false}, // it may still be undone
		{store.TimelinePlace{At: at, ID: "chg_failed", Change: true}, true},
		{store.TimelinePlace{At: at.Add(time.Second), ID: "chg_failed", Change: true}, false},
	} {
		if got := w.known(c.p); got != c.want {
			t.Errorf("known(%s at %v) = %v, want %v", c.p.ID, c.p.At.Sub(at), got, c.want)
		}
	}
}
