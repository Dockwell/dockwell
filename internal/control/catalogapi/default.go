// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog/snapshot"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/catalogsource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// settingDefault records that an install was given Dockwell's own catalog, so one a
// person removes, or a change of it a person undoes, stays so.
const settingDefault = "catalog.default-source"

// DefaultName is what Dockwell's own catalog is called, unless a source of another
// repository has the name.
const DefaultName = "dockwell"

// MakeDefault gives the install Dockwell's own catalog, once, as Dockwell's own change
// applied at once, the first time a control plane with Dockwell/catalog starts,
// whether the install is new or upgraded (ADR 0077), as ADR 0044 gives an install
// its first alert rules. The change adds the source dockwell, following
// Dockwell/catalog's main, at the commit this build holds, so it reads no network,
// unless a source reads Dockwell/catalog already. In the same change, each install
// from the catalog built into Dockwell moves its pin to that source, which its
// upgrades come from since, with the build's commit kept in the pin.
func (f *Follower) MakeDefault(ctx context.Context) error {
	h := f.h
	var made bool
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		_, err := tx.GetSetting(ctx, settingDefault)
		switch {
		case err == nil:
			made = true
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		return nil
	}); err != nil || made {
		return err
	}
	list, err := h.storedSources(ctx)
	if err != nil {
		return err
	}
	name, spec, adding := DefaultName, snapshotSpec(), true
	if i := slices.IndexFunc(list, func(s stored) bool { return isOwn(s.spec) }); i >= 0 {
		name, spec, adding = list[i].name, list[i].spec, false
	} else if slices.ContainsFunc(list, func(s stored) bool { return s.name == name }) {
		name = DefaultName + "-catalog"
		if slices.ContainsFunc(list, func(s stored) bool { return s.name == name }) {
			h.logger.WarnContext(ctx, "can't add Dockwell's own catalog, as sources of other repositories are named dockwell and dockwell-catalog. Add https://github.com/Dockwell/catalog, folder entries, on Catalog sources")
			return h.markDefault(ctx)
		}
	}
	org, moves, moved, err := h.builtInPins(ctx, name, spec)
	if err != nil {
		return err
	}
	if !adding && len(moves) == 0 {
		return h.markDefault(ctx)
	}
	ref := resource.Ref{Kind: catalogsource.Kind, Scope: org, Name: name}
	var edits []revision.Edit
	var notes []revision.Note
	summary := "Move what's installed from the catalog built into Dockwell to the catalog " + name
	if adding {
		plan, err := h.planSource(ctx, name, spec, snapshot.Branch)
		if err != nil {
			return fmt.Errorf("reading Dockwell's own catalog at %s: %w", spec.GetCommit(), err)
		}
		summary = "Add Dockwell's own catalog, following " + snapshot.Branch
		notes = plan.notes
		edits = append(edits, revision.Edit{Ref: ref, Spec: spec})
	}
	if len(moves) > 0 {
		notes = append(notes, revision.Note{
			Reason: "catalog_source_moves_installs", Ref: resource.Ref{Kind: catalogsource.Kind, Name: name}, Names: moved,
			Message: fmt.Sprintf("It moves what's installed from the catalog built into Dockwell to it: %s. Their upgrades come from Dockwell's own catalog from now on, and nothing they run changes.", strings.Join(moved, ", ")),
		})
		edits = append(edits, moves...)
	}
	plan, err := f.engine.Propose(ctx, revision.Proposal{Summary: summary, Actor: revision.SystemActor, Via: revision.ViaAutomatic, Edits: edits, Notes: notes})
	if err != nil {
		return fmt.Errorf("can't plan Dockwell's own catalog: %w", err)
	}
	if err := f.engine.Apply(ctx, plan.Change); err != nil {
		_ = f.engine.Discard(context.WithoutCancel(ctx), plan.Change, revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic})
		return fmt.Errorf("can't apply Dockwell's own catalog: %w", err)
	}
	h.logger.InfoContext(ctx, "gave the install Dockwell's own catalog", slog.String("source", name), slog.Bool("added", adding), slog.Int("moved", len(moves)))
	return h.markDefault(ctx)
}

// markDefault records that the install was given Dockwell's own catalog.
func (h *Handler) markDefault(ctx context.Context) error {
	return h.db.Update(ctx, func(tx *store.Tx) error { return tx.PutSetting(ctx, settingDefault, []byte("made")) })
}

// builtInPins plans moving each install from the catalog built into Dockwell to the
// source name, at spec's commit, keeping the build's commit it came from, and names
// the projects it moves, in their order.
func (h *Handler) builtInPins(ctx context.Context, name string, spec *resourcev1.CatalogSource) (string, []revision.Edit, []string, error) {
	var org string
	var edits []revision.Edit
	var moved []string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		org = tree.Organization.ID
		for _, p := range tree.Projects {
			r, err := tx.GetResource(ctx, string(scope.ProjectKind), org, p.Name)
			if err != nil {
				return err
			}
			decoded, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			project, _ := decoded.(*resourcev1.Project)
			pin := project.GetCatalog()
			if pin == nil || pin.GetCatalog() != "" {
				continue
			}
			next := proto.CloneOf(project)
			next.Catalog.BuiltInCommit = pin.GetCommit()
			next.Catalog.Catalog, next.Catalog.Repository, next.Catalog.Path, next.Catalog.Commit = name, spec.GetRepository(), spec.GetPath(), spec.GetCommit()
			edits = append(edits, revision.Edit{Ref: resource.Ref{Kind: scope.ProjectKind, Scope: org, Name: p.Name}, Spec: next, Base: r.Revision})
			moved = append(moved, p.Name)
		}
		return nil
	})
	return org, edits, moved, err
}
