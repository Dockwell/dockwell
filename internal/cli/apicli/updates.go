// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"fmt"
	"io"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// updatesTable lists apps with what's newer than what each runs (ADR 0079):
// the first that isn't skipped, then the rest, smallest step first.
func updatesTable(w io.Writer, rows []*dockwellv1.ServiceUpdates, now time.Time) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No apps to say of. Each is checked every 6 hours, and dockwell app-updates check checks one now.")
		return
	}
	tw := newTable(w)
	row(tw, "App", "Runs", "Newer", "Checked")
	for _, r := range rows {
		u := r.GetUpdate()
		row(tw, r.GetResource().GetName(), orDash(r.GetImage()), newerWords(u, now), orDash(when(u.GetCheckTime(), now)))
	}
	_ = tw.Flush()
}

// newerWords says what's newer than what an app runs, in a cell.
func newerWords(u *dockwellv1.ServiceUpdate, now time.Time) string {
	if u == nil || u.GetCheck() == dockwellv1.UpdateCheck_UPDATE_CHECK_UNSPECIFIED {
		return "Not checked yet"
	}
	var offers, skipped []string
	for _, a := range u.GetAvailable() {
		step := a.GetToVersion() + " " + word(a.GetKind())
		if a.GetKind() == dockwellv1.UpdateKind_UPDATE_KIND_DIGEST {
			step = word(a.GetKind()) + " of " + a.GetToVersion()
		}
		if a.GetSkipped() {
			skipped = append(skipped, a.GetToVersion())
			continue
		}
		offers = append(offers, step)
	}
	var out string
	switch {
	case len(offers) > 0:
		out = strings.Join(offers, ", ")
	case u.GetCheck() == dockwellv1.UpdateCheck_UPDATE_CHECK_FAILED || u.GetCheck() == dockwellv1.UpdateCheck_UPDATE_CHECK_NOT_CHECKED:
		out = "Can't check: " + strings.ReplaceAll(word(u.GetProblem()), "_", " ")
	default:
		out = "Up to date"
	}
	if len(skipped) > 0 {
		out += " (skipped " + strings.Join(skipped, ", ") + ")"
	}
	if t := u.GetSnoozeTime(); t != nil && t.AsTime().After(now) {
		out += ", snoozed until " + t.AsTime().Local().Format("2 Jan 15:04")
	}
	return out
}
