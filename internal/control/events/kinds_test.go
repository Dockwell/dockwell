// SPDX-License-Identifier: AGPL-3.0-only

package events

import (
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
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
