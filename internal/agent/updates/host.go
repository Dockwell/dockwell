// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"context"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Host installs a server's updates for its host (ADR 0064): the package manager
// this reader runs, and the init system's units.
type Host struct {
	Installer *Installer
}

// Plan simulates installing the updates waiting.
func (h Host) Plan(ctx context.Context, all bool) (*resource.Install, error) {
	p, err := h.Installer.Reader.PlanInstall(ctx, all)
	if err != nil {
		return nil, err
	}
	return &resource.Install{Packages: packagesOf(p.Packages), Reboot: p.Reboot, DockerRestarts: p.DockerRestarts, KeepsRunning: p.KeepsRunning}, nil
}

// Left are the planned packages not at their version yet, and whether the
// change's install still runs.
func (h Host) Left(ctx context.Context, change string, planned []resource.InstallPackage) ([]resource.InstallPackage, bool, error) {
	running, err := h.Installer.Units.Active(ctx, unit(change))
	if err != nil {
		return nil, false, err
	}
	left, err := h.Installer.left(ctx, updatesOf(planned))
	if err != nil {
		return nil, false, err
	}
	return packagesOf(left), running, nil
}

// Install installs the planned packages for the change.
func (h Host) Install(ctx context.Context, change string, planned []resource.InstallPackage, say func(string)) error {
	return h.Installer.Install(ctx, change, updatesOf(planned), say)
}

func packagesOf(ps []*dockwellv1.PackageUpdate) []resource.InstallPackage {
	out := make([]resource.InstallPackage, 0, len(ps))
	for _, p := range ps {
		out = append(out, resource.InstallPackage{
			Name: p.GetName(), Architecture: p.GetArchitecture(), From: p.GetInstalled(), To: p.GetAvailable(), Origin: p.GetOrigin(), Security: p.GetSecurity(),
			Reboot: p.GetEffect() == dockwellv1.PackageEffect_PACKAGE_EFFECT_REBOOT, Docker: p.GetEffect() == dockwellv1.PackageEffect_PACKAGE_EFFECT_DOCKER_RESTART,
		})
	}
	return out
}

func updatesOf(ps []resource.InstallPackage) []*dockwellv1.PackageUpdate {
	out := make([]*dockwellv1.PackageUpdate, 0, len(ps))
	for _, p := range ps {
		out = append(out, &dockwellv1.PackageUpdate{Name: p.Name, Architecture: p.Architecture, Installed: p.From, Available: p.To, Origin: p.Origin, Security: p.Security})
	}
	return out
}
