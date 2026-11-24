// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"fmt"
	"slices"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/store"
)

// The SSH step has already installed these rules atomically with the kept
// listener. Record the same specs without sending another guarded step to the
// agent. The change holds this server's access claim while this runs.
func (e *Engine) keepSSHRulePorts(ctx context.Context, c store.Change, server string, move *v1.SshPortMove) error {
	if len(move.GetFirewallRules()) == 0 {
		return nil
	}
	now := e.now()
	return e.db.Revise(ctx, func(tx *store.RevisionTx) error {
		revs, err := tx.RevisionsOf(ctx, c.ID)
		if err != nil {
			return err
		}
		position := len(c.Items) + len(revs)
		for _, name := range move.GetFirewallRules() {
			ref := resource.Ref{Kind: firewall.Kind, Scope: server, Name: name}
			// Resume can repeat this after the rules were recorded but before
			// the SSH step's completion was recorded.
			if slices.ContainsFunc(revs, func(r store.Revision) bool {
				return r.Kind == string(ref.Kind) && r.Scope == ref.Scope && r.Name == ref.Name
			}) {
				continue
			}
			it, err := e.load(ctx, tx.Tx, ref)
			if err != nil {
				return err
			}
			rule, ok := it.current.(*resourcev1.FirewallRule)
			if !ok || !slices.Contains(it.placed, server) {
				return fmt.Errorf("can't record the kept SSH port: %s is no longer placed on this server", ref)
			}
			if rule.GetPort() == move.GetToPort() {
				continue
			}
			rule.Port = move.GetToPort()
			spec, err := resource.Encode(rule)
			if err != nil {
				return err
			}
			rev, err := tx.AddRevision(ctx, store.NewRevision{
				ID: it.id, Kind: string(ref.Kind), Scope: ref.Scope, Name: ref.Name, Number: it.base + 1,
				Spec: spec, Change: c.ID, Position: position, At: now,
			})
			if err != nil {
				return err
			}
			position++
			if err := tx.PlaceRevision(ctx, rev.ID, rev.Revision, []string{server}, now); err != nil {
				return err
			}
			if err := tx.SetDeploymentState(ctx, store.Deployment{
				ResourceID: rev.ID, Revision: rev.Revision, Server: server, State: store.DeploymentLive, UpdatedAt: now,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// The guarded undo moves the current rules with the old listener, including
// any source edits that have since been made. It needs only the SSH edit.
func markSSHRuleFollowers(plan *Plan, followed map[resource.Ref]bool) {
	for i := range plan.Servers {
		server := &plan.Servers[i]
		for j := range server.Steps {
			step := &server.Steps[j]
			for _, name := range step.SSHPortMove.GetFirewallRules() {
				followed[resource.Ref{Kind: firewall.Kind, Scope: server.Server, Name: name}] = true
			}
		}
	}
}
