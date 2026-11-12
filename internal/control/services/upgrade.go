// SPDX-License-Identifier: AGPL-3.0-only

package services

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// Installs plans a catalog install's upgrade, for an update of one of its
// services, which upgrades the whole install (ADR 0080).
type Installs interface {
	// IsInstall says a project, by its ID, is an install from the catalog.
	IsInstall(ctx context.Context, project string) (bool, error)
	// UpgradeInstall plans upgrading the install a project is, by its ID, as
	// UpgradeCatalogInstall does. Empty summary says what it upgrades.
	UpgradeInstall(ctx context.Context, project string, skipBackup bool, summary string) (*dockwellv1.Change, error)
}

// Newer is a newer image for a service, as Dockwell's checks found it (ADR 0079).
type Newer struct {
	// Image is what an update deploys, such as "postgres:16.4", or the tag the
	// service runs, for one that moved.
	Image string
	// Kind is how far it moves: "patch", "minor", "major", "digest" for a new image
	// of the tag it runs, or "release" for a catalog release of the same version.
	Kind string
	// Catalog says it's a catalog install's newer release, which upgrades the
	// install and takes no image.
	Catalog bool
}

// Updates says what newer image each service has, as Dockwell's checks found
// them (ADR 0079).
type Updates interface {
	// Of is the newer image a service has, by its ID, and false for none known.
	Of(ctx context.Context, serviceID string) (Newer, bool, error)
}

// UpgradeService plans updating a service to a newer version of what it runs,
// safe by default (ADR 0080): a service of a catalog install upgrades its install,
// and any other that runs an image deploys the newer one, with a backup of each
// volume it writes to first. The revision engine refuses a database's move across
// release series, and the agent checks the room to pull before anything stops.
func (h *Handler) UpgradeService(ctx context.Context, req *connect.Request[dockwellv1.UpgradeServiceRequest]) (*connect.Response[dockwellv1.UpgradeServiceResponse], error) {
	m := req.Msg
	r, err := h.find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	if c, ok, err := h.upgradeInstall(ctx, r, m); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&dockwellv1.UpgradeServiceResponse{Change: c}), nil
	}
	var spec *resourcev1.Service
	var digest string
	err = h.db.View(ctx, func(tx *store.Tx) error {
		decoded, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		if spec, err = asService(decoded); err != nil {
			return err
		}
		deployed, err := tx.LatestDeployments(ctx, []string{r.ID})
		digest = ranDigest(deployed[r.ID])
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "reading a service", err)
	}
	app := spec.GetApp()
	switch {
	case spec.GetBuild() != nil:
		return nil, authapi.Invalid(fmt.Errorf("%s builds its image from Git, so it has no newer image to update to. Deploy it to build its branch's newest commit", r.Name))
	case spec.GetRuntime() != nil:
		return nil, authapi.Invalid(fmt.Errorf("%s runs from a folder on %s, so it has no image of its own to update. Change its runtime's version to move it on", r.Name, spec.GetRuntime().GetFolder()))
	case app == nil:
		return nil, authapi.Invalid(fmt.Errorf("%s doesn't run an image, so it has nothing to update", r.Name))
	}
	from := app.GetImage()
	to := m.GetImage()
	if to == "" && h.Updates != nil {
		newer, ok, err := h.Updates.Of(ctx, r.ID)
		if err != nil {
			return nil, h.internal(ctx, "reading what newer versions there are", err)
		}
		if ok && !newer.Catalog {
			to = newer.Image
		}
	}
	if to == "" {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("dockwell knows of no newer version of %s yet. Pass the image to update it to, such as %s", r.Name, from), "up_to_date")
	}
	// A tag can move, so updating to the one it runs pulls it again, and replaces
	// it only if that's a newer image. A digest can't.
	again := resource.BareImage(from) == to && resource.BareImage(to) == to
	app.Image = to
	ref := resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}
	backUp, unbacked := toBackUp(r.Scope, app)
	notes := upgradeNotes(ref, from, backUp, unbacked, m.GetSkipBackup(), again)
	if m.GetSkipBackup() {
		backUp = nil
	}
	summary := cmp.Or(m.GetSummary(), fmt.Sprintf("Update %s to %s", r.Name, to))
	if again {
		summary = cmp.Or(m.GetSummary(), fmt.Sprintf("Update %s to the newest %s", r.Name, to))
	}
	intent := &revision.Intent{Kind: revision.IntentUpgrade, Resource: r.ID, Image: to, FromImage: from, FromDigest: digest}
	c, err := h.changes.Propose(ctx, summary, []*dockwellv1.Edit{{
		Resource: resourcepb.Ref(ref),
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}},
	}}, nil, changes.Also{
		Base: r.Revision, Pull: again, Run: true, Notes: notes, Intent: intent, BackUp: backUp, SkipBackup: m.GetSkipBackup(),
		IfNothing: fault.New(revision.ErrInvalid, "%s already runs %s, which can't change, as a digest names one image. Name another image, such as a new tag", r.Name, to),
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.UpgradeServiceResponse{Change: c}), nil
}

// upgradeInstall plans the upgrade of the catalog install a service is part of,
// and says false for a service that isn't part of one.
func (h *Handler) upgradeInstall(ctx context.Context, r store.Resource, m *dockwellv1.UpgradeServiceRequest) (*dockwellv1.Change, bool, error) {
	if h.Installs == nil {
		return nil, false, nil
	}
	var project string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		for _, p := range tree.Projects {
			if slices.ContainsFunc(p.Environments, func(e scope.Environment) bool { return e.ID == r.Scope }) {
				project = p.ID
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, h.internal(ctx, "reading a service's project", err)
	}
	is, err := h.Installs.IsInstall(ctx, project)
	if err != nil || !is {
		return nil, false, err
	}
	if m.GetImage() != "" {
		return nil, true, authapi.Invalid(fmt.Errorf("%s is part of an app installed from the catalog, which updates to its catalog's newer release as a whole, so it takes no image. Leave out the image", r.Name))
	}
	c, err := h.Installs.UpgradeInstall(ctx, project, m.GetSkipBackup(), m.GetSummary())
	return c, true, err
}

// ranDigest is the digest a service's latest revision ran, where every server that
// said ran the same one.
func ranDigest(list []store.Deployment) string {
	digest := ""
	for i := range list {
		switch d := list[i].Digest; {
		case d == "":
		case digest == "":
			digest = d
		case digest != d:
			return ""
		}
	}
	return digest
}

// toBackUp is what an update of app backs up first: each volume of its
// environment it writes to. What it can't back up, it names: a server path, or a
// volume Dockwell didn't make.
func toBackUp(env string, app *resourcev1.Container) (backUp []resource.Ref, unbacked []string) {
	for _, mount := range app.GetMounts() {
		if mount.GetReadOnly() {
			continue
		}
		switch {
		case mount.GetHostPath() != "":
			unbacked = append(unbacked, mount.GetHostPath())
		case mount.GetExternal():
			unbacked = append(unbacked, mount.GetVolume())
		case mount.GetVolume() != "":
			ref := resource.Ref{Kind: volumeKind, Scope: env, Name: mount.GetVolume()}
			if !slices.Contains(backUp, ref) {
				backUp = append(backUp, ref)
			}
		}
	}
	return backUp, unbacked
}

// volumeKind is the volumes' kind, which a backup is of.
const volumeKind resource.Kind = "volume"

// upgradeNotes say what an update backs up first, or that it doesn't, and what it
// can't, as no step says what stays as it is.
func upgradeNotes(ref resource.Ref, from string, backUp []resource.Ref, unbacked []string, skip, again bool) []revision.Note {
	var notes []revision.Note
	if again {
		notes = append(notes, revision.Note{Reason: "pulls_again", Ref: ref, Message: fmt.Sprintf("Pulls %s again, and replaces %s only if that's a newer image", from, ref.Name)})
	}
	names := make([]string, len(backUp))
	for i, v := range backUp {
		names[i] = v.Name
	}
	switch {
	case len(backUp) > 0 && skip:
		notes = append(notes, revision.Note{
			Reason: "no_backup", Ref: ref, IfItChanges: true,
			Message: fmt.Sprintf("This update takes no backup, as you asked. Undoing it puts back %s, but not what the new version changed in %s. Back up what you can't lose first", from, human.List(names)),
		})
	case len(backUp) > 0:
		notes = append(notes, revision.Note{
			Reason: "backup_first", Ref: ref, IfItChanges: true,
			Message: fmt.Sprintf("Dockwell backs up %s before it changes anything. If a backup fails, the update stops there, with nothing else done. Undoing it puts back %s, not what the new version changed in its data: restore the backup for that", human.List(names), from),
		})
	}
	if len(unbacked) > 0 {
		notes = append(notes, revision.Note{
			Reason: "unbacked_paths", Ref: ref, Names: unbacked, Named: true, IfItChanges: true,
			Message: fmt.Sprintf("%s keeps data in %s, which no backup takes first. Undoing this puts back %s, not what the new version changed there", ref.Name, human.List(unbacked), from),
		})
	}
	return notes
}
