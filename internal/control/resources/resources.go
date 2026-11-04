// SPDX-License-Identifier: AGPL-3.0-only

// Package resources serves dockwell.v1.ResourceService: the resources
// Dockwell manages, by name and place, without their specs.
package resources

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/store"
)

// Page sizes for ListResources.
const (
	DefaultPageSize = 200
	MaxPageSize     = 1000
)

// Handler implements ResourceService.
type Handler struct {
	kinds  *resource.Registry
	db     *store.DB
	logger *slog.Logger
}

var _ dockwellv1connect.ResourceServiceHandler = (*Handler)(nil)

// New returns a handler that reads db, and reads specs with kinds.
func New(kinds *resource.Registry, db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{kinds: kinds, db: db, logger: logger}
}

// ListResources returns a page of the resources that exist now.
func (h *Handler) ListResources(ctx context.Context, req *connect.Request[dockwellv1.ListResourcesRequest]) (*connect.Response[dockwellv1.ListResourcesResponse], error) {
	m := req.Msg
	size, err := paging.Size(m.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	after, _, err := paging.Read(m.GetPageToken(), "ListResources")
	if err != nil {
		return nil, err
	}
	var list []store.LiveResource
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		// One more than asked for says whether there is a next page.
		list, err = tx.PageResources(ctx, m.GetKind(), m.GetScope(), after.ID, size+1)
		return err
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "can't list resources", slog.String("error", err.Error()))
		return nil, connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its resources; try again, and check its logs if it keeps failing"))
	}
	res := &dockwellv1.ListResourcesResponse{}
	if len(list) > size {
		list = list[:size]
		res.NextPageToken = paging.Token(paging.Cursor{ID: list[size-1].ID})
	}
	for i := range list {
		r := &list[i]
		res.Resources = append(res.Resources, &dockwellv1.Resource{
			Id:          r.ID,
			Resource:    resourcepb.Ref(resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}),
			Revision:    int64(r.Revision),
			UpdateTime:  timestamppb.New(r.UpdatedAt),
			DisplayName: h.displayName(resource.Kind(r.Kind), r.Spec),
		})
	}
	return connect.NewResponse(res), nil
}

// displayName is a spec's display_name, for kinds whose spec has one. It
// never holds a secret, so it is the one part of a spec that is shown.
func (h *Handler) displayName(kind resource.Kind, spec []byte) string {
	m, err := h.kinds.Decode(kind, spec)
	if err != nil || m == nil {
		return ""
	}
	r := m.ProtoReflect()
	fd := r.Descriptor().Fields().ByName("display_name")
	if fd == nil || fd.Kind() != protoreflect.StringKind || fd.IsList() {
		return ""
	}
	return r.Get(fd).String()
}
