// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog/snapshot"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
)

func dockerMaintenanceIn(plan *planRead, ref resource.Ref) *v1.DockerMaintenance {
	if r := plan.first(ref, func(r *ResourcePlan) bool { return r.DockerMaintenance != nil }); r != nil {
		return r.DockerMaintenance
	}
	return nil
}

func (e *Engine) dockerReceipts(change string, index int, receipts []*v1.DockerReceipt) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	progress := e.live[change]
	if progress == nil || index < 0 || index >= len(progress.Steps) {
		return
	}
	step := &progress.Steps[index]
	saved := slices.Clone(step.DockerReceipts)
	for _, receipt := range receipts {
		i := slices.IndexFunc(saved, func(existing *v1.DockerReceipt) bool {
			return existing.GetTarget().GetEngineId() == receipt.GetTarget().GetEngineId()
		})
		if i < 0 {
			saved = append(saved, receipt)
		} else {
			saved[i] = receipt
		}
	}
	step.DockerReceipts = saved
	update := *step
	e.publishLocked(change, Update{Index: index, Step: &update})
}

// DockerDeployment describes the image names attached to a retained revision.
type DockerDeployment struct {
	store.DockerDeployment
	Reference string
}

// RetainedDockerImages reads the immutable revisions that keep local images for undo.
func RetainedDockerImages(ctx context.Context, tx *store.Tx, server string, now time.Time) ([]DockerDeployment, error) {
	rows, err := tx.RetainedDockerDeployments(ctx, server, now.Add(-UndoWindow))
	if err != nil {
		return nil, err
	}
	out := make([]DockerDeployment, 0, len(rows))
	for _, row := range rows {
		app, err := deploymentContainer(ctx, tx, row.ResourceID, row.Kind, row.Revision, row.Spec)
		if err != nil {
			return nil, err
		}
		row.Spec = nil
		out = append(out, DockerDeployment{DockerDeployment: row, Reference: app.GetImage()})
	}
	return out, nil
}

func deploymentContainer(ctx context.Context, tx *store.Tx, id, kind string, revision int, sealed []byte) (*spec.Container, error) {
	raw, err := tx.Open(ctx, store.InRevision(id, kind, revision), sealed)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	if kind == string(service.Kind) {
		var svc spec.Service
		err := proto.Unmarshal(raw, &svc)
		return svc.GetApp(), err
	}
	app := &spec.Container{}
	return app, proto.Unmarshal(raw, app)
}

// DockerCleanupProtection reads desired networks and the image references kept for undo.
func DockerCleanupProtection(ctx context.Context, db *store.DB, server string, now time.Time, networks bool) (resource.DockerProtection, error) {
	pin, err := snapshot.CloudflaredImage()
	if err != nil {
		return resource.DockerProtection{}, err
	}
	held := resource.DockerProtection{Images: []string{pin}}
	err = db.View(ctx, func(tx *store.Tx) error {
		deployments, err := RetainedDockerImages(ctx, tx, server, now)
		if err != nil {
			return err
		}
		for i := range deployments {
			d := &deployments[i]
			for _, ref := range []string{d.Reference, d.Digest, d.LocalImageID} {
				if ref != "" {
					held.Images = append(held.Images, ref)
				}
			}
		}
		if !networks {
			return nil
		}
		rows, err := tx.ListPlaced(ctx, server)
		if err != nil {
			return err
		}
		for _, row := range rows {
			switch row.Kind {
			case string(network.Kind):
				ref := resource.Ref{Kind: network.Kind, Scope: row.Scope, Name: row.Name}
				held.Networks = append(held.Networks, network.TargetOf(ref).Name)
			case string(service.Kind), string(container.Kind):
				app, err := deploymentContainer(ctx, tx, row.ID, row.Kind, row.Revision, row.Spec)
				if err != nil {
					return err
				}
				held.Networks = append(held.Networks, app.GetExternalNetworks()...)
			}
		}
		return nil
	})
	slices.Sort(held.Images)
	slices.Sort(held.Networks)
	held.Images = slices.Compact(held.Images)
	held.Networks = slices.Compact(held.Networks)
	if len(held.Images)+len(held.Networks) > 3000 {
		return resource.DockerProtection{}, errors.New("too many retained dependencies to review Docker cleanup")
	}
	return held, err
}

func (e *Engine) applyStep(ctx context.Context, agent resource.Agent, server string, step resource.Step, desired proto.Message) error {
	if step.DockerMaintenance != nil && step.DockerMaintenance.GetResumeCleanupToken() == "" && step.DockerMaintenance.GetImageAction().GetPush() == nil {
		held, err := DockerCleanupProtection(ctx, e.db, server, e.now(), step.DockerMaintenance.GetImageAction() == nil)
		if err != nil {
			return err
		}
		ctx = resource.WithDockerProtection(ctx, held)
	}
	return agent.Apply(ctx, step, desired)
}

func (e *Engine) dockerImageReceipts(change string, index int, receipts []*v1.DockerImageReceipt) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	progress := e.live[change]
	if progress == nil || index < 0 || index >= len(progress.Steps) {
		return
	}
	step := &progress.Steps[index]
	step.DockerImageReceipts = slices.Clone(receipts)
	update := *step
	e.publishLocked(change, Update{Index: index, Step: &update})
}
