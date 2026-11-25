// SPDX-License-Identifier: AGPL-3.0-only

package events

import (
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/firewall"
	"github.com/dockwell/dockwell/internal/store"
)

// No action is listed under two kinds, so an event's count under kindOfAction
// is the view ListEvents lists it in (#642's review, N2).
func TestNoActionIsOfTwoKinds(t *testing.T) {
	kinds := map[dockwellv1.EventKind][]string{
		dockwellv1.EventKind_EVENT_KIND_SERVER:   serverActions,
		dockwellv1.EventKind_EVENT_KIND_ACCESS:   accessActions,
		dockwellv1.EventKind_EVENT_KIND_HEALTH:   healthActions,
		dockwellv1.EventKind_EVENT_KIND_BACKUP:   backupActions,
		dockwellv1.EventKind_EVENT_KIND_SETTINGS: settingsActions,
		dockwellv1.EventKind_EVENT_KIND_TASK:     taskActions,
		dockwellv1.EventKind_EVENT_KIND_ALERT:    alertActions,
	}
	of := map[string]dockwellv1.EventKind{}
	for kind, actions := range kinds {
		for _, a := range actions {
			if was, ok := of[a]; ok && was != kind {
				t.Errorf("%s is listed under %v and %v", a, was, kind)
			}
			of[a] = kind
			if got := kindOfAction(a); got != kind {
				t.Errorf("kindOfAction(%s) = %v, want %v", a, got, kind)
			}
		}
	}
}

func TestUnconfirmedFail2banActivityNamesTheAttempt(t *testing.T) {
	lookup := actors.New().In(t.Context(), nil)
	for _, op := range []string{"ban", "unban"} {
		e := FromAudit(&store.Event{Action: firewall.EventFail2banUnconfirmed, Subject: "203.0.113.7", Detail: op + " recidive"}, lookup)
		if got := e.GetFail2Ban(); got.GetOperation() != op || got.GetJail() != "recidive" || got.GetAddress() != "203.0.113.7" {
			t.Errorf("%s event has %v", op, got)
		}
	}
	old := FromAudit(&store.Event{Action: firewall.EventFail2banUnconfirmed, Subject: "203.0.113.7", Detail: "sshd"}, lookup)
	if old.GetFail2Ban() != nil {
		t.Errorf("an existing jail-only event claims an operation: %v", old.GetFail2Ban())
	}
}
