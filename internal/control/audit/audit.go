// SPDX-License-Identifier: AGPL-3.0-only

// Package audit serves dockwell.v1.AuditService: the audit log as it was
// recorded, for a person signed in. See docs/adr/0047-the-audit-log-for-a-person.md.
package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/events"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// Page sizes.
const (
	defaultPage = 100
	mostPage    = 500
)

// How the store names someone who wasn't signed in: "anonymous:" and the address.
const stranger = "anonymous:"

// The store's names for each kind of actor, by the prefix or the whole name.
var actorPrefixes = map[dockwellv1.AuditActor][]string{
	dockwellv1.AuditActor_AUDIT_ACTOR_PERSON:   {"user:"},
	dockwellv1.AuditActor_AUDIT_ACTOR_TOKEN:    {"token:"},
	dockwellv1.AuditActor_AUDIT_ACTOR_DOCKWELL: {"dockwell", "system"},
	dockwellv1.AuditActor_AUDIT_ACTOR_SERVER:   {"server:"},
	dockwellv1.AuditActor_AUDIT_ACTOR_SHELL:    {"cli:"},
	dockwellv1.AuditActor_AUDIT_ACTOR_STRANGER: {stranger},
}

// Handler implements AuditService.
type Handler struct {
	db     *store.DB
	engine *revision.Engine
	logger *slog.Logger
}

var _ dockwellv1connect.AuditServiceHandler = (*Handler)(nil)

// New returns a handler that reads the audit log from db, and the changes
// its change events are about from engine. Without an engine, those events
// come without their change.
func New(db *store.DB, engine *revision.Engine, logger *slog.Logger) *Handler {
	return &Handler{db: db, engine: engine, logger: logger}
}

// ListAuditEvents returns a page of the audit log, newest first.
func (h *Handler) ListAuditEvents(ctx context.Context, req *connect.Request[dockwellv1.ListAuditEventsRequest]) (*connect.Response[dockwellv1.ListAuditEventsResponse], error) {
	m := req.Msg
	size, err := paging.Size(m.GetPageSize(), defaultPage, mostPage)
	if err != nil {
		return nil, err
	}
	cursor, _, err := paging.Read(m.GetPageToken(), "ListAuditEvents")
	if err != nil {
		return nil, err
	}
	if n := utf8.RuneCountInString(m.GetQuery()); n > store.MaxAuditQuery {
		return nil, authapi.Invalid(fmt.Errorf("query is %d characters, and holds %d at most. Shorten it", n, store.MaxAuditQuery))
	}
	f := store.AuditFilter{ActorPrefixes: actorPrefixes[m.GetActor()], Query: m.GetQuery()}
	if _, ok := actorPrefixes[m.GetActor()]; !ok && m.GetActor() != dockwellv1.AuditActor_AUDIT_ACTOR_UNSPECIFIED {
		return nil, authapi.Invalid(errors.New("actor isn't a kind Dockwell knows. Leave it out for every kind"))
	}
	if m.GetSince() != nil {
		f.Since = m.GetSince().AsTime()
	}
	if s := m.GetSurface(); s != dockwellv1.Surface_SURFACE_UNSPECIFIED {
		for name, surface := range changes.Surfaces {
			if surface == s {
				f.Surface = name
			}
		}
		if f.Surface == "" {
			return nil, authapi.Invalid(errors.New("surface isn't one Dockwell knows. Leave it out for every one"))
		}
	}
	out := &dockwellv1.ListAuditEventsResponse{}
	names := actors.New()
	err = h.db.View(ctx, func(tx *store.Tx) error {
		// One more than the page, to know whether another comes after it.
		found, err := tx.AuditEvents(ctx, f, store.TimelineCursor{At: cursor.At, ID: cursor.ID}, size+1)
		if err != nil {
			return err
		}
		if len(found) > size {
			last := found[size-1]
			found = found[:size]
			out.NextPageToken = paging.Token(paging.Cursor{At: last.At, ID: last.ID})
		}
		lookup := names.In(ctx, tx)
		about, err := h.changes(ctx, tx, found, lookup)
		if err != nil {
			return err
		}
		for i := range found {
			out.Events = append(out.Events, event(lookup, &found[i], about))
		}
		return nil
	})
	if err != nil {
		h.logger.Error("can't read the audit log", slog.String("error", err.Error()))
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("can't read the audit log from the control plane's store. Try again in a moment, and if it keeps failing, check the control plane's log"))
	}
	return connect.NewResponse(out), nil
}

// changes reads the changes a page's change events are about, as they are
// now, each with its plan in brief, by ID.
func (h *Handler) changes(ctx context.Context, tx *store.Tx, page []store.Event, look actors.Lookup) (map[string]*dockwellv1.Change, error) {
	if h.engine == nil {
		return nil, nil
	}
	var ids []string
	for i := range page {
		if strings.HasPrefix(page[i].Action, "change.") && strings.HasPrefix(page[i].Subject, id.Change+"_") {
			ids = append(ids, page[i].Subject)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	found, err := tx.GetChanges(ctx, slices.Compact(slices.Sorted(slices.Values(ids))))
	if err != nil {
		return nil, err
	}
	cs := make([]store.Change, 0, len(found))
	for changeID := range found {
		cs = append(cs, found[changeID])
	}
	records, err := h.engine.Records(ctx, tx, cs)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*dockwellv1.Change, len(records))
	for _, r := range records {
		out[r.ID] = changes.Proto(r, look.Actor)
	}
	return out, nil
}

// event is one audit event for the API, with its actor and target named,
// and the event as the timeline has it, with the change it's about, if any.
func event(lookup actors.Lookup, e *store.Event, about map[string]*dockwellv1.Change) *dockwellv1.AuditEvent {
	out := &dockwellv1.AuditEvent{
		Id: e.ID, Time: timestamppb.New(e.At), Action: e.Action, Surface: changes.Surfaces[e.Surface],
		Subject: e.Subject, Detail: e.Detail, Event: events.FromAudit(e, lookup),
	}
	if c, ok := about[e.Subject]; ok && strings.HasPrefix(e.Action, "change.") {
		out.Event.Change = c
	}
	if address, ok := strings.CutPrefix(e.Actor, stranger); ok {
		out.Address = address
	} else {
		out.Actor = lookup.Actor(e.Actor)
	}
	if e.Target != "" {
		out.Target = &dockwellv1.EventSubject{Id: e.Target, Name: lookup.Name(e.Target)}
	}
	return out
}
