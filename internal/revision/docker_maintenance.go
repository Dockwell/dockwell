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
	"github.com/dockwell/dockwell/internal/resource/volume"
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

// DockerCleanupProtection reads desired images, networks, volumes, backups and
// references kept for undo on server.
func DockerCleanupProtection(ctx context.Context, db *store.DB, server string, now time.Time, networks, volumes bool) (resource.DockerProtection, error) {
	return dockerCleanupProtection(ctx, db, server, now, true, networks, volumes, nil)
}

// DockerCleanupProtectionKinds reads only the protection sets needed for the
// requested target kinds, scoped to server.
func DockerCleanupProtectionKinds(ctx context.Context, db *store.DB, server string, now time.Time, images, networks, volumes bool) (resource.DockerProtection, error) {
	return dockerCleanupProtection(ctx, db, server, now, images, networks, volumes, nil)
}

func dockerCleanupProtection(ctx context.Context, db *store.DB, server string, now time.Time, images, networks, volumes bool, onlyVolumes map[string]bool) (resource.DockerProtection, error) {
	if !images && !networks && !volumes {
		return resource.DockerProtection{}, nil
	}
	var held resource.DockerProtection
	if images {
		pin, err := snapshot.CloudflaredImage()
		if err != nil {
			return resource.DockerProtection{}, err
		}
		held.Images = []string{pin}
	}
	cutoff := now.Add(-UndoWindow)
	err := db.View(ctx, func(tx *store.Tx) error {
		if images {
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
		}
		placedVolumeKeys := map[string]bool{}
		if networks || volumes {
			rows, err := tx.ListPlaced(ctx, server)
			if err != nil {
				return err
			}
			for _, row := range rows {
				switch row.Kind {
				case string(network.Kind):
					if networks {
						ref := resource.Ref{Kind: network.Kind, Scope: row.Scope, Name: row.Name}
						held.Networks = append(held.Networks, network.TargetOf(ref).Name)
					}
				case string(volume.Kind):
					if volumes {
						ref := resource.Ref{Kind: volume.Kind, Scope: row.Scope, Name: row.Name}
						name := volume.TargetOf(ref).Name
						placedVolumeKeys[row.Scope+"\x00"+row.Name] = true
						if onlyVolumes == nil || onlyVolumes[name] {
							held.Volumes = append(held.Volumes, name)
						}
					}
				case string(service.Kind), string(container.Kind):
					app, err := deploymentContainer(ctx, tx, row.ID, row.Kind, row.Revision, row.Spec)
					if err != nil {
						return err
					}
					if networks {
						held.Networks = append(held.Networks, app.GetExternalNetworks()...)
					}
					if volumes {
						held.Volumes = appendMountedVolumes(held.Volumes, row.Kind, row.Scope, app, onlyVolumes)
					}
				}
			}
		}
		if !volumes {
			return nil
		}
		// Any volume superseded or deleted on this server within the undo window stays protected.
		oldVolumes, err := tx.SupersededSince(ctx, string(volume.Kind), cutoff)
		if err != nil {
			return err
		}
		if len(oldVolumes) > 0 {
			volIDs := make([]string, 0, len(oldVolumes))
			for _, old := range oldVolumes {
				volIDs = append(volIDs, old.ResourceID)
			}
			given, err := tx.RevisionsGiven(ctx, server, volIDs)
			if err != nil {
				return err
			}
			for _, old := range oldVolumes {
				if !slices.Contains(given[old.ResourceID], old.Revision) {
					continue
				}
				ref := resource.Ref{Kind: volume.Kind, Scope: old.Scope, Name: old.Name}
				name := volume.TargetOf(ref).Name
				placedVolumeKeys[old.Scope+"\x00"+old.Name] = true
				if onlyVolumes == nil || onlyVolumes[name] {
					held.RollbackVolumes = append(held.RollbackVolumes, name)
				}
			}
		}
		for _, kind := range []string{string(service.Kind), string(container.Kind)} {
			oldWorkloads, err := tx.SupersededSince(ctx, kind, cutoff)
			if err != nil {
				return err
			}
			if len(oldWorkloads) == 0 {
				continue
			}
			workloadIDs := make([]string, 0, len(oldWorkloads))
			for _, old := range oldWorkloads {
				workloadIDs = append(workloadIDs, old.ResourceID)
			}
			given, err := tx.RevisionsGiven(ctx, server, workloadIDs)
			if err != nil {
				return err
			}
			for _, old := range oldWorkloads {
				if !slices.Contains(given[old.ResourceID], old.Revision) {
					continue
				}
				app, err := deploymentContainer(ctx, tx, old.ResourceID, kind, old.Revision, old.Spec)
				if err != nil {
					return err
				}
				held.RollbackVolumes = appendMountedVolumes(held.RollbackVolumes, kind, old.Scope, app, onlyVolumes)
			}
		}
		// Any volume referenced by a backup schedule for a volume placed on this server,
		// or by a recorded backup on this server, stays protected.
		policies, err := tx.ListKind(ctx, string(policyKind))
		if err != nil {
			return err
		}
		for _, row := range policies {
			raw, err := tx.Open(ctx, store.InRevision(row.ID, row.Kind, row.Revision), row.Spec)
			if err != nil {
				return err
			}
			var policy spec.BackupPolicy
			uerr := proto.Unmarshal(raw, &policy)
			clear(raw)
			if uerr != nil {
				return uerr
			}
			name := policy.GetVolume()
			if name == "" {
				continue
			}
			ref := resource.Ref{Kind: volume.Kind, Scope: row.Scope, Name: name}
			volName := volume.TargetOf(ref).Name
			if onlyVolumes != nil && !onlyVolumes[volName] {
				continue
			}
			if !placedVolumeKeys[row.Scope+"\x00"+name] {
				volRes, rerr := tx.GetResource(ctx, string(volume.Kind), row.Scope, name)
				if rerr != nil {
					if errors.Is(rerr, store.ErrNotFound) {
						continue
					}
					return rerr
				}
				given, gerr := tx.RevisionsGiven(ctx, server, []string{volRes.ID})
				if gerr != nil {
					return gerr
				}
				if len(given[volRes.ID]) == 0 {
					continue
				}
			}
			held.BackupVolumes = append(held.BackupVolumes, volName)
		}
		backups, err := tx.ListBackups(ctx, store.BackupFilter{ServerID: server})
		if err != nil {
			return err
		}
		for i := range backups {
			b := &backups[i]
			if b.Volume == "" || b.EnvironmentID == "" {
				continue
			}
			ref := resource.Ref{Kind: volume.Kind, Scope: b.EnvironmentID, Name: b.Volume}
			volName := volume.TargetOf(ref).Name
			if onlyVolumes == nil || onlyVolumes[volName] {
				held.BackupVolumes = append(held.BackupVolumes, volName)
			}
		}
		return nil
	})
	compact := func(s []string) []string {
		slices.Sort(s)
		return slices.Compact(s)
	}
	held.Images = compact(held.Images)
	held.Networks = compact(held.Networks)
	held.Volumes = compact(held.Volumes)
	held.BackupVolumes = compact(held.BackupVolumes)
	held.RollbackVolumes = compact(held.RollbackVolumes)
	if len(held.Images)+len(held.Networks)+len(held.Volumes)+len(held.BackupVolumes)+len(held.RollbackVolumes) > 6000 {
		return resource.DockerProtection{}, errors.New("too many retained dependencies to review Docker cleanup")
	}
	return held, err
}

func appendMountedVolumes(dst []string, kind, scope string, app *spec.Container, only map[string]bool) []string {
	for _, m := range app.GetMounts() {
		name := m.GetVolume()
		if name == "" {
			continue
		}
		if kind != string(container.Kind) && !m.GetExternal() {
			ref := resource.Ref{Kind: volume.Kind, Scope: scope, Name: name}
			name = volume.TargetOf(ref).Name
		}
		if only == nil || only[name] {
			dst = append(dst, name)
		}
	}
	return dst
}

func (e *Engine) applyStep(ctx context.Context, agent resource.Agent, server string, step resource.Step, desired proto.Message) error {
	if step.DockerMaintenance != nil && step.DockerMaintenance.GetResumeCleanupToken() == "" && step.DockerMaintenance.GetImageAction().GetPush() == nil {
		var wantImages, wantNetworks, wantVolumes bool
		var onlyVolumes map[string]bool
		if step.DockerMaintenance.GetImageAction() != nil {
			wantImages = true
		} else {
			for _, t := range step.DockerMaintenance.GetTargets() {
				switch t.GetKind() {
				case v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE:
					wantImages = true
				case v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK:
					wantNetworks = true
				case v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME:
					wantVolumes = true
					if onlyVolumes == nil {
						onlyVolumes = map[string]bool{}
					}
					onlyVolumes[t.GetName()] = true
				default:
				}
			}
		}
		if wantImages || wantNetworks || wantVolumes {
			held, err := dockerCleanupProtection(ctx, e.db, server, e.now(), wantImages, wantNetworks, wantVolumes, onlyVolumes)
			if err != nil {
				return err
			}
			ctx = resource.WithDockerProtection(ctx, held)
		}
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
