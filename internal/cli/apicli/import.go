// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"fmt"
	"io"
	"strings"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// importView writes what an import found and what it would do, grouped by what it
// does, each with what it becomes and why (docs/adr/0056-importing-a-1panel-server.md).
func importView(w io.Writer, res *dockwellv1.ImportOnePanelResponse) {
	found := res.GetFound()
	fmt.Fprintf(w, "1Panel %s, under %s, read from %s. Nothing was changed.\n", found.GetVersion(), found.GetBaseDir(), found.GetRecords())
	groups := []struct {
		action dockwellv1.ImportAction
		title  string
	}{
		{dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON, "Take on, as it runs, with no restart"},
		{dockwellv1.ImportAction_IMPORT_ACTION_MAKE, "Make again in Dockwell"},
		{dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, "Leave as it is"},
	}
	for _, g := range groups {
		var steps []*dockwellv1.ImportStep
		for _, s := range res.GetSteps() {
			if s.GetAction() == g.action {
				steps = append(steps, s)
			}
		}
		if len(steps) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s (%d)\n", g.title, len(steps))
		for _, s := range steps {
			fmt.Fprintf(w, "\n  %s: %s\n", strings.TrimSpace(kindWord(s.GetKind())+" "+s.GetName()), s.GetSummary())
			if b := s.GetBecomes(); b != "" {
				fmt.Fprintf(w, "    Becomes %s.\n", b)
			}
			if r := s.GetReason(); r != "" {
				fmt.Fprintf(w, "    %s\n", r)
			}
			if wf := s.GetWaitsFor(); wf != "" {
				fmt.Fprintf(w, "    Waits for %s, which isn't in Dockwell yet.\n", wf)
			}
		}
	}
}

// kindWord names a kind of thing an import finds, in a word or two.
func kindWord(k dockwellv1.ImportKind) string {
	switch k {
	case dockwellv1.ImportKind_IMPORT_KIND_APP:
		return "App"
	case dockwellv1.ImportKind_IMPORT_KIND_RUNTIME:
		return "Runtime"
	case dockwellv1.ImportKind_IMPORT_KIND_WEBSITE:
		return "Website"
	case dockwellv1.ImportKind_IMPORT_KIND_CERTIFICATE:
		return "Certificate"
	case dockwellv1.ImportKind_IMPORT_KIND_DATABASE_SERVER:
		return "Database server"
	case dockwellv1.ImportKind_IMPORT_KIND_DATABASE:
		return "Database"
	case dockwellv1.ImportKind_IMPORT_KIND_CRON_JOB:
		return "Cron job"
	case dockwellv1.ImportKind_IMPORT_KIND_COMPOSE_PROJECT:
		return "Compose project"
	case dockwellv1.ImportKind_IMPORT_KIND_CONTAINERS:
		return "Containers"
	case dockwellv1.ImportKind_IMPORT_KIND_PANEL, dockwellv1.ImportKind_IMPORT_KIND_UNSPECIFIED:
	}
	return ""
}
