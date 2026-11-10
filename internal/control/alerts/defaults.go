// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// settingDefaults records that an install's first rules were made. Rules someone
// deleted stay deleted as the store remembers every rule it ever managed.
const settingDefaults = "alerts.defaults"

// DefaultsSummary is what the change that makes the first rules says.
const DefaultsSummary = "Set up the default alert rules"

// MakeDefaults makes an install's default rules, one for each condition, as
// Dockwell's own change applied at once, as a control plane with alerts starts,
// whether the install is new or upgraded (ADR 0044). A default added in a later
// release, such as restore-drill-failed (ADR 0068), comes to an install that
// upgrades the same way. A rule of a default's name that's there, or ever was, stays
// as it is, so one someone deleted stays deleted.
func (e *Evaluator) MakeDefaults(ctx context.Context) error {
	e.init()
	var org string
	var taken map[string]bool
	err := e.DB.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		org = tree.Organization.ID
		existing, err := tx.EverManaged(ctx, string(alert.RuleKind))
		if err != nil {
			return err
		}
		taken = map[string]bool{}
		for _, r := range existing {
			taken[r.Name] = true
		}
		return nil
	})
	if err != nil || org == "" {
		return err
	}
	var edits []revision.Edit
	for _, d := range alert.Defaults() {
		if taken[d.Name] {
			continue
		}
		edits = append(edits, revision.Edit{Ref: resource.Ref{Kind: alert.RuleKind, Scope: org, Name: d.Name}, Spec: d.Rule})
	}
	if len(edits) > 0 {
		plan, err := e.Engine.Propose(ctx, revision.Proposal{Summary: DefaultsSummary, Actor: revision.SystemActor, Via: revision.ViaAutomatic, Edits: edits})
		if err != nil {
			return fmt.Errorf("can't plan the first alert rules: %w", err)
		}
		if err := e.Engine.Apply(ctx, plan.Change); err != nil {
			return fmt.Errorf("can't apply the first alert rules: %w", err)
		}
		e.Logger.InfoContext(ctx, "made the install's first alert rules", slog.Int("rules", len(edits)))
	}
	return e.DB.Update(ctx, func(tx *store.Tx) error { return tx.PutSetting(ctx, settingDefaults, []byte("made")) })
}
