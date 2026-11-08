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
	// Grouped by when each moves: in this plan, later once Dockwell has what it
	// needs, or not at all.
	type group struct {
		title string
		in    func(*dockwellv1.ImportStep) bool
	}
	groups := []group{
		{"In this plan, taken on as it runs, with no restart", func(s *dockwellv1.ImportStep) bool {
			return s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON && s.GetWaitsFor() == 0
		}},
		{"In this plan, made again in Dockwell", func(s *dockwellv1.ImportStep) bool {
			return s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_MAKE && s.GetWaitsFor() == 0
		}},
	}
	for _, wf := range []dockwellv1.ImportWaitsFor{
		dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_WEBSITES, dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_DATABASES,
		dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_SCHEDULED_TASKS, dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_FIREWALL,
	} {
		groups = append(groups, group{"Later, once Dockwell has " + waitsForWords(wf), func(s *dockwellv1.ImportStep) bool {
			return s.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE && s.GetWaitsFor() == wf
		}})
	}
	groups = append(groups, group{"Stays with 1Panel", func(s *dockwellv1.ImportStep) bool {
		return s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_LEAVE
	}})
	for _, g := range groups {
		var steps []*dockwellv1.ImportStep
		for _, s := range res.GetSteps() {
			if g.in(s) {
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
		}
	}
}

// waitsForWords names a part of Dockwell an imported thing waits for.
func waitsForWords(wf dockwellv1.ImportWaitsFor) string {
	switch wf {
	case dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_WEBSITES:
		return "websites"
	case dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_DATABASES:
		return "databases"
	case dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_SCHEDULED_TASKS:
		return "scheduled tasks"
	case dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_FIREWALL:
		return "firewall rules"
	case dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_UNSPECIFIED:
	}
	return "what it needs"
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
