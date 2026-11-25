// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/website"
)

// backupRef returns the agent-side resource ref where b's backup archive lives.
// Volume backups live under volume/<env>/<name>; website folder backups live under
// service/<env>/web.
func backupRef(b store.Backup) resource.Ref {
	if b.WebsiteID != "" {
		return resource.Ref{Kind: service.Kind, Scope: b.EnvironmentID, Name: b.Volume}
	}
	return resource.Ref{Kind: volume.Kind, Scope: b.EnvironmentID, Name: b.Volume}
}

type websiteTarget struct {
	ref      resource.Ref
	webSvcID string
	server   store.Server
	domain   string
	hostPath string
}

const webServiceName = "web"

// websiteFolderInTx reads a website environment's web service, server placement,
// primary domain, and mounted host folder path.
func (h *Handler) websiteFolderInTx(ctx context.Context, tx *store.Tx, envID string) (webSvc store.Resource, placed []string, domain, hostPath string, err error) {
	webSvc, err = tx.GetResource(ctx, string(service.Kind), envID, webServiceName)
	if errors.Is(err, store.ErrNotFound) || (err == nil && webSvc.Spec == nil) {
		return store.Resource{}, nil, "", "", authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("this website has no folder service to back up"), "not_folder_website")
	}
	if err != nil {
		return store.Resource{}, nil, "", "", err
	}
	spec, err := h.keeper.Decode(ctx, tx, store.InRevision(webSvc.ID, webSvc.Kind, webSvc.Revision), webSvc.Spec)
	if err != nil {
		return store.Resource{}, nil, "", "", err
	}
	svc := spec.(*resourcev1.Service)
	for _, m := range svc.GetApp().GetMounts() {
		if m.GetTarget() == website.FolderTarget || m.GetTarget() == website.PHPTarget {
			if hp := m.GetHostPath(); hp != "" {
				hostPath = hp
				break
			}
		}
	}
	if hostPath == "" {
		return store.Resource{}, nil, "", "", authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("this website doesn't serve from a folder on the server, so there's no website folder to back up"), "not_folder_website")
	}
	placed, err = tx.Placement(ctx, webSvc.ID, webSvc.Revision)
	if err != nil {
		return store.Resource{}, nil, "", "", err
	}
	routes, err := tx.ListResources(ctx, string(route.Kind), envID)
	if err == nil {
		for i := range routes {
			r := routes[i]
			plain, oerr := tx.Open(ctx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if oerr != nil {
				continue
			}
			decoded, derr := h.engine.Kinds().Decode(route.Kind, plain)
			if derr != nil {
				continue
			}
			rt := decoded.(*resourcev1.Route)
			if rt.GetDomain() != "" && (domain == "" || rt.GetPath() == "") {
				domain = rt.GetDomain()
			}
		}
	}
	return webSvc, placed, domain, hostPath, nil
}

// websiteOf resolves a website project ID to its backup target and checks the
// caller's role in that project.
func (h *Handler) websiteOf(ctx context.Context, websiteID, serverName, minRole string) (websiteTarget, error) {
	var out websiteTarget
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		var proj *scope.Project
		for i := range tree.Projects {
			if tree.Projects[i].ID == websiteID {
				proj = &tree.Projects[i]
				break
			}
		}
		if proj == nil || len(proj.Environments) == 0 {
			return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no website %s", websiteID), "not_found")
		}
		env := proj.Environments[0]
		if c, ok := place.CallerFrom(ctx); ok {
			held := place.Holds(c.Grants, place.Place{Project: proj.ID, Environment: env.ID, Protected: env.Protected})
			if held == "" {
				return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no website %s", websiteID), "not_found")
			}
			need := minRole
			if env.Protected && store.RoleRank(need) < store.RoleRank(store.RoleAdmin) && store.RoleRank(need) > store.RoleRank(store.RoleViewer) {
				need = store.RoleAdmin
			}
			if store.RoleRank(held) < store.RoleRank(need) {
				return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("this needs %s access on this website", need))
			}
		}
		webSvc, placed, domain, hostPath, err := h.websiteFolderInTx(ctx, tx, env.ID)
		if err != nil {
			return err
		}
		srv, err := chooseServer(ctx, tx, webServiceName, placed, serverName)
		if err != nil {
			return err
		}
		out = websiteTarget{
			ref:      resource.Ref{Kind: service.Kind, Scope: env.ID, Name: webServiceName},
			webSvcID: webSvc.ID,
			server:   srv,
			domain:   domain,
			hostPath: hostPath,
		}
		return nil
	})
	return out, err
}

// websiteProjectForEnv finds the project ID that owns envID.
func websiteProjectForEnv(ctx context.Context, tx *store.Tx, envID string) string {
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return ""
	}
	for _, p := range tree.Projects {
		for _, e := range p.Environments {
			if e.ID == envID {
				return p.ID
			}
		}
	}
	return ""
}

// websiteDomain returns the cached primary domain for a website backup's environment.
func (f *rowFacts) websiteDomain(ctx context.Context, tx *store.Tx, h *Handler, b *store.Backup) string {
	if b.WebsiteID == "" {
		return ""
	}
	if f.websiteDomains == nil {
		f.websiteDomains = map[string]string{}
	}
	if d, ok := f.websiteDomains[b.EnvironmentID]; ok {
		return d
	}
	var domain string
	routes, err := tx.ListResources(ctx, string(route.Kind), b.EnvironmentID)
	if err == nil {
		for i := range routes {
			r := routes[i]
			plain, oerr := tx.Open(ctx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if oerr != nil {
				continue
			}
			decoded, derr := h.engine.Kinds().Decode(route.Kind, plain)
			if derr != nil {
				continue
			}
			rt := decoded.(*resourcev1.Route)
			if rt.GetDomain() != "" && (domain == "" || rt.GetPath() == "") {
				domain = rt.GetDomain()
			}
		}
	}
	f.websiteDomains[b.EnvironmentID] = domain
	return domain
}

// verifyWebsiteRestoreTarget checks that a website folder backup's target website still
// exists on the same server, still serves the exact folder recorded in the backup, and
// does not overlap with any folder mounted by another project on that server (M5).
func (h *Handler) verifyWebsiteRestoreTarget(ctx context.Context, b *store.Backup) (string, error) {
	var verified string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		var proj *scope.Project
		for i := range tree.Projects {
			if tree.Projects[i].ID == b.WebsiteID {
				proj = &tree.Projects[i]
				break
			}
		}
		if proj == nil || !slices.ContainsFunc(proj.Environments, func(e scope.Environment) bool { return e.ID == b.EnvironmentID }) {
			return authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("this backup belongs to a website that no longer exists, so %s can't be restored automatically", b.WebsitePath),
				"website_gone")
		}
		_, placed, _, currentPath, err := h.websiteFolderInTx(ctx, tx, b.EnvironmentID)
		if err != nil {
			return err
		}
		if !slices.Contains(placed, b.ServerID) {
			return authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("this website no longer runs on the server where backup %s was taken", b.ID),
				"website_moved_server")
		}
		cleanCurrent := path.Clean(currentPath)
		cleanBackup := path.Clean(b.WebsitePath)
		if cleanCurrent != cleanBackup {
			return authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("this backup was taken of %s; the website now serves %s. Move the website back to %s or take a fresh backup of %s before restoring",
					cleanBackup, cleanCurrent, cleanBackup, cleanCurrent),
				"website_folder_changed")
		}
		// Refuse if any other project's website or service on this server mounts an
		// overlapping host path.
		for _, p := range tree.Projects {
			if p.ID == b.WebsiteID {
				continue
			}
			for _, e := range p.Environments {
				svcs, err := tx.ListResources(ctx, e.ID, string(service.Kind))
				if err != nil {
					return err
				}
				for i := range svcs {
					svcPlaced, err := tx.Placement(ctx, svcs[i].ID, svcs[i].Revision)
					if err != nil {
						return err
					}
					if !slices.Contains(svcPlaced, b.ServerID) {
						continue
					}
					decoded, err := h.keeper.Decode(ctx, tx, store.InRevision(svcs[i].ID, svcs[i].Kind, svcs[i].Revision), svcs[i].Spec)
					if err != nil {
						continue
					}
					svc, ok := decoded.(*resourcev1.Service)
					if !ok {
						continue
					}
					for _, m := range svc.GetApp().GetMounts() {
						hp := strings.TrimSpace(m.GetHostPath())
						if hp == "" {
							continue
						}
						cleanOther := path.Clean(hp)
						if hostPathsOverlap(cleanCurrent, cleanOther) {
							return authapi.ReasonError(connect.CodeFailedPrecondition,
								fmt.Errorf("%s overlaps with %s used by another project on this server, so Dockwell refused to overwrite it", cleanCurrent, cleanOther),
								"folder_shared")
						}
					}
				}
			}
		}
		verified = cleanCurrent
		return nil
	})
	return verified, err
}

func hostPathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") ||
		strings.HasPrefix(b, strings.TrimSuffix(a, "/")+"/")
}
