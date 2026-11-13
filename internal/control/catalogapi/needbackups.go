// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/store"
)

// An app's own database, alone, is the app's to back up and restore from its page,
// with a role in its project and none in the server's, which may be another
// project's: a dump of that one database as its owner, and a restore of it alone that
// keeps the app's user out while it loads (ADR 0084).

// Backups backs up, lists and restores an app's database alone, as the backups
// handler does.
type Backups interface {
	BackUpOne(ctx context.Context, volumeID, database, forLink, destination string) (*dockwellv1.Backup, error)
	ListFor(ctx context.Context, forLink string, pageSize int32, pageToken string) (*dockwellv1.ListBackupsResponse, error)
	RestoreOne(ctx context.Context, backupID, forLink string, link resource.Ref, summary string) (*dockwellv1.Change, error)
}

// BackUpWith gives the handler what backs up an app's database.
func (h *Handler) BackUpWith(b Backups) { h.backups = b }

// needDatabase is the database an install's need has: the link that meets it, by ref
// and ID, the volume its server keeps its data in, by ID, and the database's name.
type needDatabase struct {
	link     resource.Ref
	linkID   string
	volumeID string
	database string
}

// databaseOf finds the database an install's need has.
func (h *Handler) databaseOf(ctx context.Context, id, need string) (needDatabase, error) {
	var out needDatabase
	if h.backups == nil {
		return out, connect.NewError(connect.CodeUnavailable, errors.New("this control plane runs no backups"))
	}
	s, err := h.catalog(ctx)
	if err != nil {
		return out, err
	}
	found, err := h.find(ctx, s, id)
	if err != nil {
		return out, err
	}
	install := found.install
	env := install.GetEnvironmentId()
	err = h.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResource(ctx, string(link.Kind), env, need)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("%s has no need %s met by a database. Name one of its needs, such as db", install.GetProject(), need), "not_found")
		}
		if err != nil {
			return err
		}
		l := &resourcev1.Link{}
		// A link is never sealed: it names values, and holds none.
		if err := proto.Unmarshal(r.Spec, l); err != nil {
			return err
		}
		if l.GetDatabaseName() == "" {
			return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s's %s has no database of its own on %s, so it's backed up with its server, from the server's page", install.GetProject(), need, l.GetService()), "no_database")
		}
		server := cmp.Or(l.GetMeetsEnvironment(), env)
		svc, err := tx.GetResource(ctx, "service", server, l.GetService())
		if errors.Is(err, store.ErrNotFound) || err == nil && svc.Spec == nil {
			return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s, which meets %s's %s, isn't there any more", l.GetService(), install.GetProject(), need), "no_server")
		}
		if err != nil {
			return err
		}
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(svc.ID, svc.Kind, svc.Revision), svc.Spec)
		if err != nil {
			return err
		}
		svcSpec, _ := spec.(*resourcev1.Service)
		vol := database.DataVolume(svcSpec)
		if vol == "" {
			return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s keeps its data in no volume Dockwell can back up, so %s's database can't be backed up alone", l.GetService(), install.GetProject()), "no_volume")
		}
		v, err := tx.GetResource(ctx, "volume", server, vol)
		if err != nil {
			return err
		}
		out = needDatabase{link: resource.Ref{Kind: link.Kind, Scope: env, Name: need}, linkID: r.ID, volumeID: v.ID, database: l.GetDatabaseName()}
		return nil
	})
	return out, err
}

// BackUpCatalogNeed backs up an install's database alone, and returns it running.
func (h *Handler) BackUpCatalogNeed(ctx context.Context, req *connect.Request[dockwellv1.BackUpCatalogNeedRequest]) (*connect.Response[dockwellv1.BackUpCatalogNeedResponse], error) {
	db, err := h.databaseOf(ctx, req.Msg.GetId(), req.Msg.GetNeed())
	if err != nil {
		return nil, err
	}
	b, err := h.backups.BackUpOne(ctx, db.volumeID, db.database, db.linkID, req.Msg.GetDestination())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.BackUpCatalogNeedResponse{Backup: b}), nil
}

// ListCatalogNeedBackups lists the backups of an install's database taken for it.
func (h *Handler) ListCatalogNeedBackups(ctx context.Context, req *connect.Request[dockwellv1.ListCatalogNeedBackupsRequest]) (*connect.Response[dockwellv1.ListCatalogNeedBackupsResponse], error) {
	db, err := h.databaseOf(ctx, req.Msg.GetId(), req.Msg.GetNeed())
	if err != nil {
		return nil, err
	}
	page, err := h.backups.ListFor(ctx, db.linkID, req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ListCatalogNeedBackupsResponse{Backups: page.GetBackups(), NextPageToken: page.GetNextPageToken()}), nil
}

// RestoreCatalogNeed plans bringing an install's database back alone.
func (h *Handler) RestoreCatalogNeed(ctx context.Context, req *connect.Request[dockwellv1.RestoreCatalogNeedRequest]) (*connect.Response[dockwellv1.RestoreCatalogNeedResponse], error) {
	db, err := h.databaseOf(ctx, req.Msg.GetId(), req.Msg.GetNeed())
	if err != nil {
		return nil, err
	}
	c, err := h.backups.RestoreOne(ctx, req.Msg.GetBackupId(), db.linkID, db.link, req.Msg.GetSummary())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.RestoreCatalogNeedResponse{Change: c}), nil
}
