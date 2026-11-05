// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Setup makes an install's hidden defaults on its first start: the
// organization, its project "default", and that project's environment
// "production". The project and environment are revisions of a change by
// dockwell, applied at once, like any other; they run on no server, so
// there is nothing to plan. Setup does nothing once the organization
// exists, and reports whether it made them.
func Setup(ctx context.Context, db *store.DB, logger *slog.Logger) (bool, error) {
	var made string // the organization, once made
	now := time.Now()
	err := db.Revise(ctx, func(tx *store.RevisionTx) error {
		if _, err := tx.Organization(ctx); !errors.Is(err, store.ErrNotFound) {
			return err
		}
		org := store.Organization{ID: id.New(id.Organization), Name: scope.DefaultOrganization, CreatedAt: now}
		if err := tx.CreateOrganization(ctx, org); err != nil {
			return err
		}
		project, err := resource.Encode(&resourcev1.Project{})
		if err != nil {
			return err
		}
		environment, err := resource.Encode(&resourcev1.Environment{})
		if err != nil {
			return err
		}
		projectID := id.New(id.Project)
		revs := []store.NewRevision{
			{Kind: string(scope.ProjectKind), Scope: org.ID, Name: scope.DefaultProject, ID: projectID, Spec: project},
			{Kind: string(scope.EnvironmentKind), Scope: projectID, Name: scope.DefaultEnvironment, ID: id.New(id.Environment), Spec: environment},
		}
		items := make([]item, len(revs))
		c := store.Change{
			ID: id.New(id.Change), Summary: "Set up the default project and environment", Actor: SystemActor,
			State: store.ChangePlanned, CreatedAt: now, ProposedVia: ViaAutomatic,
		}
		specs := []proto.Message{&resourcev1.Project{}, &resourcev1.Environment{}}
		for i := range revs {
			r := &revs[i]
			items[i] = item{ref: resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}, id: r.ID, spec: specs[i], encoded: r.Spec}
			c.Items = append(c.Items, store.ChangeItem{ResourceID: r.ID, Kind: r.Kind, Scope: r.Scope, Name: r.Name, Spec: r.Spec})
		}
		// The plan is what Propose would make: two revisions, no steps.
		plan, err := (&Engine{kinds: resource.NewRegistry().With(scope.Types()...)}).plan(ctx, c.ID, items, false)
		if err != nil {
			return err
		}
		plan.Change, plan.Summary = c.ID, c.Summary
		if c.Plan, err = json.Marshal(plan); err != nil {
			return err
		}
		c.Fingerprint, c.FingerprintScheme = plan.Fingerprint, fingerprintKeyed
		if err := tx.CreateChange(ctx, c); err != nil {
			return err
		}
		if err := tx.StartApplying(ctx, c.ID, SystemActor, ViaAutomatic, []byte(`{"steps":[]}`), now); err != nil {
			return err
		}
		for i := range revs {
			r := &revs[i]
			r.Change, r.Position, r.At = c.ID, i, now
			if _, err := tx.AddRevision(ctx, *r); err != nil {
				return err
			}
		}
		if err := tx.SetChangeState(ctx, c.ID, store.ChangeApplied, "", now); err != nil {
			return err
		}
		if err := tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: SystemActor, Action: "change.applied", Subject: c.ID, Detail: c.Summary, Surface: ViaAutomatic,
		}); err != nil {
			return err
		}
		made = org.ID
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("can't set up the default project and environment: %w", err)
	}
	if made != "" {
		logger.InfoContext(ctx, "made the default organization, project and environment", slog.String("organization", made))
	}
	return made != "", nil
}
