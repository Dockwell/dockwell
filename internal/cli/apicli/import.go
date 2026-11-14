// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// importView writes what an import found and what it would do, grouped by what it
// does, each with what it becomes and why (docs/adr/0056-importing-a-1panel-server.md).
func importView(w io.Writer, res *dockwellv1.ImportOnePanelResponse) {
	found := res.GetFound()
	fmt.Fprintf(w, "1Panel %s, under %s, read from %s. Nothing was changed.\n", found.GetVersion(), found.GetBaseDir(), found.GetRecords())
	stepsView(w, "1Panel", res.GetSteps(), res.GetChange())
}

// panelView writes what an import from Coolify or Dokploy found and what it would
// do, as importView does (docs/adr/0089-importing-from-coolify-and-dokploy.md).
func panelView(w io.Writer, found *dockwellv1.Panel, steps []*dockwellv1.ImportStep, change *dockwellv1.Change) {
	name := "Coolify"
	if found.GetKind() == dockwellv1.PanelKind_PANEL_KIND_DOKPLOY {
		name = "Dokploy"
	}
	version := found.GetVersion()
	if version == "" {
		version = "of a version its containers don't say"
	}
	fmt.Fprintf(w, "%s %s, read from Docker and %s. Nothing was changed.\n", name, version, found.GetFolder())
	stepsView(w, name, steps, change)
}

// stepsView writes an import's steps, grouped by when each moves, each with what
// it becomes and why, and says when nothing was planned for want of a person.
func stepsView(w io.Writer, panel string, all []*dockwellv1.ImportStep, change *dockwellv1.Change) {
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
	groups = append(groups, group{"Later, once " + panel + "'s proxy hands over ports 80 and 443 (dockwell import check-domains)", func(s *dockwellv1.ImportStep) bool {
		return s.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE && s.GetWaitsFor() == dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_PORTS
	}}, group{"In Dockwell already", func(s *dockwellv1.ImportStep) bool {
		return s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_DONE
	}}, group{"Stays with " + panel, func(s *dockwellv1.ImportStep) bool {
		return s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_LEAVE
	}})
	for _, g := range groups {
		var steps []*dockwellv1.ImportStep
		for _, s := range all {
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
	// A token reads what's there, and a person plans it, as a plan that takes a
	// container on shows what a token can't read.
	if change == nil && slices.ContainsFunc(all, func(s *dockwellv1.ImportStep) bool {
		return s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON && !strings.HasPrefix(s.GetReason(), "Dockwell can't read what runs")
	}) {
		fmt.Fprintln(w, "\nNothing was planned: the CLI signs in with an API token, and a plan that takes containers on takes a person. Sign in to the panel to plan it, from the server's page.")
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
	case dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_PORTS:
		return "ports 80 and 443"
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
	case dockwellv1.ImportKind_IMPORT_KIND_DOMAIN:
		return "Domain"
	case dockwellv1.ImportKind_IMPORT_KIND_PANEL, dockwellv1.ImportKind_IMPORT_KIND_UNSPECIFIED:
	}
	return ""
}

// domainChecksView writes what was checked of each domain a panel's proxy serves:
// those nothing was found for, which can be handed over, with what the proxy
// answered, then those that stay behind, each with why (ADR 0089).
func domainChecksView(w io.Writer, checks []*dockwellv1.PanelDomainCheck) {
	if len(checks) == 0 {
		fmt.Fprintln(w, "No domain is left to hand over: Dockwell routes each already.")
		return
	}
	var clean, held []*dockwellv1.PanelDomainCheck
	for _, c := range checks {
		if c.GetHeld() == "" {
			clean = append(clean, c)
		} else {
			held = append(held, c)
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(clean) > 0 {
		fmt.Fprintln(tw, "No login or address limit found, so each can be handed over (dockwell import domains --domains):")
		for _, c := range clean {
			var answers []string
			for _, p := range c.GetProbes() {
				answers = append(answers, p.GetPath()+" "+p.GetAnswer())
			}
			fmt.Fprintf(tw, "  %s\t%s\n", c.GetDomain(), strings.Join(answers, ", "))
		}
	}
	if len(held) > 0 {
		fmt.Fprintln(tw, "Stays behind:")
		for _, c := range held {
			fmt.Fprintf(tw, "  %s\t%s\n", c.GetDomain(), c.GetHeld())
		}
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "Checked %s, and asked the proxy for each.\n", checks[0].GetScanned())
}
