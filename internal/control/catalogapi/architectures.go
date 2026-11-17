// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
)

// runsWhereItIs says why a release can't be planned where an install runs, or ""
// where it can (ADR 0102, #771). It reads only the release's own services, the ones
// the upgrade sets the release's architectures on, as the companions the install
// runs choose them; a service a person added beside them, or one the release drops,
// isn't the release's to place (#771's review, H1). A server that hasn't said its
// architecture isn't a reason yet: the plan says so when it's made, and an automatic
// update tries again. The list of updates and the upgrade both ask it, so they say
// the same.
func runsWhereItIs(ctx context.Context, tx *store.Tx, env string, release *catalogv1.Entry, pin *resourcev1.CatalogPin) (string, error) {
	arch := release.GetArchitectures()
	if len(arch) == 0 {
		return "", nil
	}
	chosen, err := catalog.Choose(release, catalog.Resolve(release, pin))
	if err != nil {
		return "", err
	}
	names, err := catalog.ServiceNames(chosen.GetCompose())
	if err != nil {
		return "", err
	}
	list, err := tx.ListResources(ctx, env, string(service.Kind))
	if err != nil {
		return "", err
	}
	reports, err := tx.ServerReports(ctx)
	if err != nil {
		return "", err
	}
	for i := range list {
		if !slices.Contains(names, list[i].Name) {
			continue
		}
		servers, err := tx.Placement(ctx, list[i].ID, list[i].Revision)
		if err != nil {
			return "", err
		}
		slices.Sort(servers)
		for _, id := range servers {
			var inv dockwellv1.ServerInventory
			r, ok := reports[id]
			if !ok || proto.Unmarshal(r.Inventory, &inv) != nil || inv.GetArchitecture() == "" || slices.Contains(arch, inv.GetArchitecture()) {
				continue
			}
			name := id
			if s, err := tx.GetServer(ctx, id); err == nil && s.Name != "" {
				name = s.Name
			}
			return catalog.UnfitWords(releaseWords(release, pin), arch, list[i].Name, name, inv.GetArchitecture()), nil
		}
	}
	return "", nil
}

// releaseWords is how a reader knows a release: its version, or "Release 2 of 1.5.0"
// where the version stays (#771's review, L3).
func releaseWords(release *catalogv1.Entry, pin *resourcev1.CatalogPin) string {
	if release.GetVersion() == pin.GetVersion() {
		return fmt.Sprintf("Release %d of %s", release.GetRelease(), release.GetVersion())
	}
	return release.GetVersion()
}

// installAdmin says the caller holds an admin's role on the whole install, or is the
// install itself, as local access and Dockwell's own runs are: who may read a
// server's architecture (ADR 0049).
func installAdmin(ctx context.Context) bool {
	caller, ok := place.CallerFrom(ctx)
	return !ok || store.RoleRank(place.Holds(caller.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin)
}

// upgradesThatRun takes the upgrade away from a way to meet a need by upgrading
// another install, so its server can be shared, where that install's newer release
// can't be planned where it runs, and adds why to the way's reason: the form shows
// it as a way it can't take, with no Upgrade that would be refused (#771's review, L6).
func (h *Handler) upgradesThatRun(ctx context.Context, s sourced, options []*dockwellv1.CatalogNeedOption) error {
	if !slices.ContainsFunc(options, func(o *dockwellv1.CatalogNeedOption) bool { return o.GetUpgradeInstall() != "" }) {
		return nil
	}
	return h.db.View(ctx, func(tx *store.Tx) error {
		installs, err := h.installs(ctx, tx, s)
		if err != nil {
			return err
		}
		for _, o := range options {
			if o.GetUpgradeInstall() == "" {
				continue
			}
			i := slices.IndexFunc(installs, func(in *dockwellv1.CatalogInstall) bool { return in.GetProjectId() == o.GetUpgradeInstall() })
			if i < 0 {
				continue
			}
			release, ok := s.entryFor(installs[i].GetPin())
			if !ok || release.GetRelease() != o.GetUpgradeRelease() {
				continue
			}
			why, err := runsWhereItIs(ctx, tx, installs[i].GetEnvironmentId(), release, installs[i].GetPin())
			if err != nil {
				return err
			}
			if why == "" {
				continue
			}
			if !installAdmin(ctx) {
				why = catalog.UnfitBrief(why)
			}
			// The server stays kept apart, now with no way out to offer: the row says
			// both, and has no Upgrade.
			o.Why = o.GetWhy() + ". " + why
			o.UpgradeInstall, o.UpgradeRelease, o.UpgradeApp = "", 0, ""
		}
		return nil
	})
}
