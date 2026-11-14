// SPDX-License-Identifier: AGPL-3.0-only

// Package events serves dockwell.v1.EventService: the timeline of changes
// to managed state, and of what servers and people did beside them, from
// the revision store and the audit log. See
// docs/adr/0020-the-event-timeline.md.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/alerts"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/backups"
	"github.com/dockwell/dockwell/internal/control/certificates"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
	controlfiles "github.com/dockwell/dockwell/internal/control/files"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/health"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/control/processes"
	"github.com/dockwell/dockwell/internal/control/reconciled"
	"github.com/dockwell/dockwell/internal/control/settings"
	"github.com/dockwell/dockwell/internal/control/tasks"
	"github.com/dockwell/dockwell/internal/control/terminals"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// Page sizes for ListEvents and a watch's snapshot.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// DefaultPoll is how often a watch reads the store again for events that
// another process recorded, such as dockwell recover beside the control
// plane. Events recorded here, and every change, arrive at once.
const DefaultPoll = 3 * time.Second

// lateness is how far back a watch looks again each time it reads the
// store: an event recorded at one time may commit a moment later, behind
// one that committed first.
const lateness = 30 * time.Second

// gather is how long a watch waits after something happens for whatever
// comes with it, such as the steps of one change, to send them together.
const gather = 100 * time.Millisecond

// What a person does with a server's files, and what stopped them. Not asking for
// a passkey rests on these being where the person can see them (ADR 0032), as with
// a secret's reveal. A save has no change of its own, as files aren't managed state.
const (
	fileRead          = "file.read"
	fileRefuse        = "file.refuse"
	fileSave          = "file.save"
	fileSaveWayBackIn = "file.save_way_back_in"
	fileSaveFailed    = "file.save_failed"
	fileSaveUnknown   = "file.save_unknown"
	// What asked for a passkey to reach what lets someone back in, with none following.
	fileSaveUnconfirmed    = "file.save_unconfirmed"
	changeApplyUnconfirmed = "change.apply_unconfirmed"
)

// fileWrites are what a person makes, moves, deletes and puts back on a server, and
// what stopped them (ADR 0045). Each is on the timeline, as a save is.
var fileWrites = []string{
	"file.make", "file.make_failed", "file.make_unconfirmed",
	"file.move", "file.move_failed", "file.move_unconfirmed",
	"file.delete", "file.delete_failed", "file.delete_unconfirmed",
	"file.restore", "file.restore_failed", "file.restore_unconfirmed",
	"file.upload", "file.upload_failed", "file.upload_unconfirmed",
	"file.change_unknown",
}

// fileDownload is a file or a folder downloaded, which is on Activity as a read is.
const fileDownload = "file.download"

// The audit log's actions that belong on the timeline, by kind. What the
// timeline leaves out stays in the audit log: refusals, such as wrong codes
// and refused tokens, a token's first use from an address, signing out,
// and the install's own setup.
var (
	serverActions = slices.Concat([]string{
		"server.join", "server.renew", "server.revoke", fileSave, fileSaveWayBackIn, fileSaveFailed, fileSaveUnknown, fileSaveUnconfirmed, fleet.ActionPublicAddress, fleet.ActionPublicAddressClear,
		processes.EventEnd, processes.EventEndFailed, processes.EventEndUnconfirmed, revision.ActionDesiredSent, revision.ActionDesiredUnknown,
		reconciled.ActionMade, reconciled.ActionStarted, reconciled.ActionWaiting, reconciled.ActionDropped,
	},
		// Who had a root shell on a server, and when (ADR 0037).
		terminals.Actions, fileWrites)
	healthActions = []string{health.Action}
	// A setting of the install isn't managed state, so it's an event, not a change (ADR 0042).
	settingsActions = settings.Actions
	// Taking a backup is an event, and a restore is a change (ADR 0033).
	backupActions = backups.Actions
	// A scheduled task's run is an event; making or changing the task is a change (ADR 0053).
	taskActions = tasks.Actions
	// An alert is the control plane's record, and a mute no change (ADR 0044).
	alertActions  = alerts.Actions
	accessActions = []string{
		"owner.claim", "auth.sign_in", "session.revoke",
		"passkey.add", "passkey.add_with_recovery_code", "passkey.add_with_authenticator_code", "passkey.remove",
		"totp.enable", "totp.disable", "totp.unlock", "totp.setup_discarded", "recovery_codes.regenerate",
		"token.create", "token.revoke",
		"sign_in_link.create", "claim_link.create", "join_token.create",
		"secret.reveal", "secrets.rotate_key",
		// A test of a destination's keys, before any plan: who tried which storage.
		backups.ActionTestDestination,
		// A saved DNS provider's token, used again to write in its zones (ADR 0088).
		dnsproviders.ActionCheck,
		// A certificate's key by DNS going to a server, or revoked as it went (ADR 0088).
		certificates.ActionDeliver, certificates.ActionRevoke,
		// An app opened through the panel, which is as much as a secret revealed (ADR 0061).
		"service.open",
		// One of an app's own values that reads hidden, revealed (ADR 0081).
		ActionRevealServiceValue,
		fileRead, fileRefuse, fileDownload, changeApplyUnconfirmed,
		"member.invite", "member.invite_revoke", "member.join",
	}
)

// ActionRevealServiceValue is the audit log's word for revealing one of an app's own
// values that reads hidden. Its subject is the value's name, and its target the
// service (ADR 0081).
const ActionRevealServiceValue = "service.value_reveal"

// Handler implements EventService.
type Handler struct {
	engine *revision.Engine
	db     *store.DB
	logger *slog.Logger
	poll   time.Duration
}

var _ dockwellv1connect.EventServiceHandler = (*Handler)(nil)

// New returns a handler over engine and the store it reads. poll is how
// often a watch reads the store again; DefaultPoll when zero.
func New(engine *revision.Engine, db *store.DB, logger *slog.Logger, poll time.Duration) *Handler {
	if poll <= 0 {
		poll = DefaultPoll
	}
	return &Handler{engine: engine, db: db, logger: logger, poll: poll}
}

// ListEvents returns a page of events, newest first.
func (h *Handler) ListEvents(ctx context.Context, req *connect.Request[dockwellv1.ListEventsRequest]) (*connect.Response[dockwellv1.ListEventsResponse], error) {
	m := req.Msg
	f, err := filter(m.GetKinds(), m.GetServerId(), m.GetResourceId(), m.GetActorId(), m.GetActorKind())
	if err != nil {
		return nil, err
	}
	size, err := paging.Size(m.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	after, ok, err := paging.Read(m.GetPageToken(), "ListEvents")
	if err != nil {
		return nil, err
	}
	var before *store.TimelineCursor
	if ok {
		before = &store.TimelineCursor{At: after.At, ID: after.ID}
	}
	res := &dockwellv1.ListEventsResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		res.Events, res.NextPageToken, _, err = h.page(ctx, tx, actors.New(), f, before, size)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(res), nil
}

// CountEvents says how many events of each kind there are, under the filters
// ListEvents takes but the kinds, counting only what the caller's ListEvents
// would list, so a view's count is what its list holds (#639). It narrows its own
// answer, as counts hold nothing the interceptor could place.
func (h *Handler) CountEvents(ctx context.Context, req *connect.Request[dockwellv1.CountEventsRequest]) (*connect.Response[dockwellv1.CountEventsResponse], error) {
	m := req.Msg
	every, err := filter(nil, m.GetServerId(), m.GetResourceId(), m.GetActorId(), m.GetActorKind())
	if err != nil {
		return nil, err
	}
	grants, narrowed := authapi.Narrowed(ctx)
	counts := map[dockwellv1.EventKind]int{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		if !narrowed {
			// Everything listed is the caller's: one grouped query.
			tally, err := tx.TimelineCounts(ctx, every)
			if err != nil {
				return err
			}
			counts[dockwellv1.EventKind_EVENT_KIND_CHANGE] = tally.Changes
			for action, n := range tally.Actions {
				counts[kindOfAction(action)] += n
			}
			return nil
		}
		// Each event placed as the interceptor places ListEvents', a page at a time.
		visible := authapi.Visible(ctx, tx, grants)
		look := actors.New().In(ctx, tx)
		var before *store.TimelineCursor
		for {
			places, err := tx.TimelinePlaces(ctx, store.TimelineQuery{Filter: every, Before: before, Limit: countPage})
			if err != nil {
				return err
			}
			entries, err := tx.TimelineEntries(ctx, places)
			if err != nil {
				return err
			}
			for _, e := range entries {
				var ev *dockwellv1.Event
				kind := dockwellv1.EventKind_EVENT_KIND_CHANGE
				if e.Change != nil {
					ev = &dockwellv1.Event{Kind: kind, Change: &dockwellv1.Change{Id: e.Change.ID}}
				} else {
					ev = FromAudit(e.Event, look)
					kind = kindOfAction(e.Event.Action)
				}
				ok, err := visible(ev.ProtoReflect())
				if err != nil {
					return err
				}
				if ok {
					counts[kind]++
				}
			}
			if len(places) < countPage {
				return nil
			}
			last := places[len(places)-1]
			before = &store.TimelineCursor{At: last.At, ID: last.ID}
		}
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	res := &dockwellv1.CountEventsResponse{}
	total := 0
	for _, k := range eventKinds() {
		n := counts[k]
		total += n
		res.Counts = append(res.Counts, &dockwellv1.EventKindCount{Kind: k, Count: int32(n)}) //nolint:gosec // a timeline holds far fewer than 2^31 entries
	}
	res.Total = int32(total)
	return connect.NewResponse(res), nil
}

// countPage is how many of the timeline's places CountEvents reads at once for a
// caller whose list is narrowed.
const countPage = 500

// kindOfAction is the kind an audit event's action is listed under, as ListEvents'
// kinds filter by them.
func kindOfAction(action string) dockwellv1.EventKind {
	for _, k := range []struct {
		kind    dockwellv1.EventKind
		actions []string
	}{
		{dockwellv1.EventKind_EVENT_KIND_SERVER, serverActions},
		{dockwellv1.EventKind_EVENT_KIND_ACCESS, accessActions},
		{dockwellv1.EventKind_EVENT_KIND_HEALTH, healthActions},
		{dockwellv1.EventKind_EVENT_KIND_BACKUP, backupActions},
		{dockwellv1.EventKind_EVENT_KIND_SETTINGS, settingsActions},
		{dockwellv1.EventKind_EVENT_KIND_TASK, taskActions},
		{dockwellv1.EventKind_EVENT_KIND_ALERT, alertActions},
	} {
		if slices.Contains(k.actions, action) {
			return k.kind
		}
	}
	return dockwellv1.EventKind_EVENT_KIND_ACCESS
}

// eventKinds are the kinds of event there are, in the order EventKind has them.
func eventKinds() []dockwellv1.EventKind {
	var out []dockwellv1.EventKind
	for n := range dockwellv1.EventKind_name {
		if k := dockwellv1.EventKind(n); k != dockwellv1.EventKind_EVENT_KIND_UNSPECIFIED {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// page reads a page of events, older than before when it is set, with the
// token for the next and each event's time on the timeline.
func (h *Handler) page(ctx context.Context, tx *store.Tx, names *actors.Names, f store.TimelineFilter, before *store.TimelineCursor, size int) (
	[]*dockwellv1.Event, string, []time.Time, error,
) {
	// One more than asked for says whether there is a next page.
	entries, err := tx.Timeline(ctx, store.TimelineQuery{Filter: f, Before: before, Limit: size + 1})
	if err != nil {
		return nil, "", nil, err
	}
	next := ""
	if len(entries) > size {
		entries = entries[:size]
		last := entries[size-1]
		next = paging.Token(paging.Cursor{At: last.At, ID: last.ID()})
	}
	events, err := h.render(ctx, tx, names, entries)
	if err != nil {
		return nil, "", nil, err
	}
	at := make([]time.Time, len(entries))
	for i, e := range entries {
		at[i] = e.At
	}
	return events, next, at, nil
}

// WatchEvents sends the newest page of events, then what happens.
func (h *Handler) WatchEvents(ctx context.Context, req *connect.Request[dockwellv1.WatchEventsRequest], stream *connect.ServerStream[dockwellv1.WatchEventsResponse]) error {
	m := req.Msg
	f, err := filter(m.GetKinds(), m.GetServerId(), m.GetResourceId(), m.GetActorId(), m.GetActorKind())
	if err != nil {
		return err
	}
	size, err := paging.Size(m.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Listen before reading, so nothing that happens in between is missed.
	updates := h.engine.WatchAll(ctx)
	recorded := h.db.Recorded()
	started := time.Now()

	w := &watch{
		h: h, f: f, names: actors.New(), sent: map[string]sent{}, removed: map[string]time.Time{},
		floor: started.Add(-lateness),
	}
	first := &dockwellv1.WatchEventsResponse{Snapshot: true}
	var times []time.Time
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		first.Events, first.NextPageToken, times, err = h.page(ctx, tx, w.names, f, nil, size)
		return err
	})
	if err != nil {
		return h.internal(ctx, err)
	}
	if first.NextPageToken != "" {
		// Older than the snapshot is ListEvents' to read, not the watch's.
		w.floor = maxTime(w.floor, times[len(times)-1])
	}
	for i, e := range first.Events {
		w.saw(times[i], e)
	}
	if err := stream.Send(first); err != nil {
		return err
	}

	ticker := time.NewTicker(h.poll)
	defer ticker.Stop()
	var flush <-chan time.Time
	again := false             // read the store again from the floor
	moved := map[string]bool{} // changes that moved on
	for {
		select {
		case <-ctx.Done():
			return nil
		case u, ok := <-updates:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				// This watch fell behind the engine, which dropped it: listen
				// again, and read every change it has sent again, as well as
				// the store.
				updates = h.engine.WatchAll(ctx)
				for id, e := range w.sent {
					if e.event.GetChange() != nil {
						moved[id] = true
					}
				}
				again = true
			} else if f.Changes {
				moved[u.Change] = true
			}
		case <-recorded:
			recorded = h.db.Recorded()
			again = true
		case <-ticker.C:
			again = true
		case <-flush:
			flush = nil
			var msg *dockwellv1.WatchEventsResponse
			err := h.db.View(ctx, func(tx *store.Tx) error {
				var err error
				msg, err = w.catchUp(ctx, tx, again, moved)
				return err
			})
			if err != nil {
				return h.internal(ctx, err)
			}
			again, moved = false, map[string]bool{}
			if len(msg.Events) > 0 || len(msg.Removed) > 0 {
				if err := stream.Send(msg); err != nil {
					return err
				}
			}
			continue
		}
		if flush == nil && (again || len(moved) > 0) {
			flush = time.After(gather)
		}
	}
}

// rereadPage is how many places a watch reads at a time when it reads the
// store again, paging forward from its floor until it has them all.
const rereadPage = 500

// watch is what one watch has sent, and the names it has read.
type watch struct {
	h     *Handler
	f     store.TimelineFilter
	names *actors.Names
	// sent holds each event sent from the floor on, and every change sent
	// that may still move on, as last sent.
	sent map[string]sent
	// removed holds the changes this watch has said were discarded, from
	// the floor on, by when, so it says so once.
	removed map[string]time.Time
	// floor is where reading the store again starts: lateness before the
	// newest event seen, and never before the snapshot's oldest.
	floor time.Time
}

// sent is an event as a watch last sent it, and its time on the timeline.
type sent struct {
	at    time.Time
	event *dockwellv1.Event
}

// saw notes an event sent, and moves the floor up behind the newest.
func (w *watch) saw(at time.Time, e *dockwellv1.Event) {
	w.sent[e.GetId()] = sent{at: at, event: e}
	w.floor = maxTime(w.floor, at.Add(-lateness))
}

// known reports whether the watch has sent the entry at p as it stands
// still: an audit event at the same place, as one that changes, such as a
// service's that keeps changing, moves on with it, or a change at the same
// place that can't move on again. Reading the store again skips those.
func (w *watch) known(p store.TimelinePlace) bool {
	s, ok := w.sent[p.ID]
	switch {
	case !ok:
		return false
	case !p.Change:
		return s.at.Equal(p.At)
	}
	return s.at.Equal(p.At) && !open(s.event)
}

// catchUp reads what happened since the watch last looked, with tx: the
// store from the floor when again is set, every page of it, and the
// changes that moved on, with what they revert, whose undo or rollback is
// theirs to show. It returns what to send: the events that are new or
// differ from what was sent, and the changes discarded since, which leave
// the timeline whether or not this watch sent them, as a client may have
// read them with ListEvents.
func (w *watch) catchUp(ctx context.Context, tx *store.Tx, again bool, moved map[string]bool) (*dockwellv1.WatchEventsResponse, error) {
	msg := &dockwellv1.WatchEventsResponse{}
	var read []store.TimelinePlace
	seen := map[string]bool{}
	take := func(places []store.TimelinePlace, skipKnown bool) {
		for _, p := range places {
			switch {
			case p.Discarded:
				if _, said := w.removed[p.ID]; !said {
					w.removed[p.ID] = p.At
					msg.Removed = append(msg.Removed, p.ID)
					delete(w.sent, p.ID)
				}
			case seen[p.ID], skipKnown && w.known(p):
			default:
				seen[p.ID] = true
				read = append(read, p)
			}
		}
	}
	if again {
		after := &store.TimelineCursor{At: w.floor}
		for {
			places, err := tx.TimelinePlaces(ctx, store.TimelineQuery{Filter: w.f, After: after, Discarded: true, Limit: rereadPage})
			if err != nil {
				return nil, err
			}
			take(places, true)
			if len(places) < rereadPage {
				break
			}
			last := places[len(places)-1]
			after = &store.TimelineCursor{At: last.At, ID: last.ID}
		}
	}
	if len(moved) > 0 {
		ids := make([]string, 0, len(moved))
		for id := range moved {
			ids = append(ids, id)
		}
		places, err := tx.TimelinePlaces(ctx, store.TimelineQuery{Filter: w.f, ChangeIDs: ids, Discarded: true})
		if err != nil {
			return nil, err
		}
		take(places, false)
	}
	entries, err := tx.TimelineEntries(ctx, read)
	if err != nil {
		return nil, err
	}
	// What the changes read revert is theirs to show too: an undo or a
	// rollback that moved on changes the change it reverts.
	var reverted []string
	for _, e := range entries {
		if e.Change == nil {
			continue
		}
		if r := e.Change.Reverts; r != "" && !seen[r] {
			seen[r] = true
			reverted = append(reverted, r)
		}
	}
	if len(reverted) > 0 {
		more, err := tx.Timeline(ctx, store.TimelineQuery{Filter: w.f, ChangeIDs: reverted})
		if err != nil {
			return nil, err
		}
		entries = append(entries, more...)
	}
	events, err := w.h.render(ctx, tx, w.names, entries)
	if err != nil {
		return nil, err
	}
	// Newest first, as everywhere else.
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := entries[order[i]], entries[order[j]]
		return a.At.After(b.At) || a.At.Equal(b.At) && a.ID() > b.ID()
	})
	for _, i := range order {
		e := events[i]
		if s, ok := w.sent[e.GetId()]; ok && proto.Equal(s.event, e) {
			continue
		}
		msg.Events = append(msg.Events, e)
		w.saw(entries[i].At, e)
	}
	// What is behind the floor and can't move on again is read no more.
	for id, s := range w.sent {
		if s.at.Before(w.floor) && !open(s.event) {
			delete(w.sent, id)
		}
	}
	for id, at := range w.removed {
		if at.Before(w.floor) {
			delete(w.removed, id)
		}
	}
	return msg, nil
}

// open reports whether an event may still change: a change that is planned
// or applying, or applied and still within its undo window.
func open(e *dockwellv1.Event) bool {
	c := e.GetChange()
	switch c.GetState() {
	case dockwellv1.ChangeState_CHANGE_STATE_PLANNED, dockwellv1.ChangeState_CHANGE_STATE_APPLYING:
		return true
	case dockwellv1.ChangeState_CHANGE_STATE_APPLIED:
		return c.GetUndoneBy() == "" && c.GetUndoDeadline().AsTime().After(time.Now())
	case dockwellv1.ChangeState_CHANGE_STATE_UNSPECIFIED, dockwellv1.ChangeState_CHANGE_STATE_FAILED,
		dockwellv1.ChangeState_CHANGE_STATE_DISCARDED:
	}
	return false
}

// render turns timeline entries into events, reading what they need with
// tx: each change's progress and what reverts it, and the names of actors
// and subjects that names doesn't have.
func (h *Handler) render(ctx context.Context, tx *store.Tx, names *actors.Names, entries []store.TimelineEntry) ([]*dockwellv1.Event, error) {
	var cs []store.Change
	for _, e := range entries {
		if e.Change != nil {
			cs = append(cs, *e.Change)
		}
	}
	rs, err := h.engine.Records(ctx, tx, cs)
	if err != nil {
		return nil, err
	}
	records := make(map[string]*revision.Record, len(rs))
	for _, r := range rs {
		records[r.ID] = r
	}
	look := names.In(ctx, tx)
	placer := place.NewPlacer(tx)
	out := make([]*dockwellv1.Event, len(entries))
	for i, e := range entries {
		if e.Change != nil {
			r := records[e.Change.ID]
			out[i] = changeEvent(e.At, r, look)
			// What reaches a project the reader holds no role in is narrowed to theirs.
			if err := changes.Narrow(ctx, tx, out[i].Change); err != nil {
				return nil, err
			}
			if out[i].GetChange().GetElsewhere().GetEditsElsewhere() {
				out[i].Actor = nil
			}
			if r.State == store.ChangePlanned {
				// A planned change in a protected environment waits for an admin (ADR 0049).
				places, err := placer.Of(ctx, r.ID)
				if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, place.ErrUnknown) {
					return nil, err
				}
				out[i].Change.Protected = slices.ContainsFunc(places, func(p place.Place) bool { return p.Protected })
				if err := changes.ForCaller(ctx, tx, h.engine, r.ID, out[i].Change); err != nil {
					return nil, err
				}
			}
		} else {
			out[i] = FromAudit(e.Event, look)
		}
	}
	return out, nil
}

func changeEvent(at time.Time, r *revision.Record, look actors.Lookup) *dockwellv1.Event {
	c := changes.Proto(r, look.Actor)
	// Where it sits on the timeline is when it began, as what is under way has to
	// stay put. What it says is when it failed, if it has.
	if ended := r.EndedAt(); ended.After(at) {
		at = ended
	}
	e := &dockwellv1.Event{
		Id: r.ID, Kind: dockwellv1.EventKind_EVENT_KIND_CHANGE, Action: "change." + r.State, Time: timestamppb.New(at),
		Actor: c.GetProposedBy(), Surface: c.GetProposedVia(), Change: c,
	}
	if r.AppliedBy != "" {
		e.Actor, e.Surface = c.GetAppliedBy(), c.GetAppliedVia()
	}
	return e
}

// FromAudit is an event from the audit log, as its writer recorded it:
// where it came from, and who or what it acted on, with the facts its kind
// carries. The timeline and the audit log both say events this way.
func FromAudit(a *store.Event, look actors.Lookup) *dockwellv1.Event {
	e := &dockwellv1.Event{
		Id: a.ID, Kind: dockwellv1.EventKind_EVENT_KIND_ACCESS, Action: a.Action, Time: timestamppb.New(a.At),
		Actor: look.Actor(a.Actor), Surface: changes.Surfaces[a.Surface],
	}
	if strings.HasPrefix(a.Action, "server.") || strings.HasPrefix(a.Action, "process.") || a.Action == fileSave || a.Action == fileSaveWayBackIn || a.Action == fileSaveFailed || a.Action == fileSaveUnknown || a.Action == fileSaveUnconfirmed ||
		slices.Contains(terminals.Actions, a.Action) || slices.Contains(fileWrites, a.Action) {
		e.Kind = dockwellv1.EventKind_EVENT_KIND_SERVER
	}
	if slices.Contains(terminals.Actions, a.Action) {
		// As for backups: only internal/control/terminals writes these, and what it
		// writes holds nothing typed or printed.
		if told, ok := terminals.ReadTold(a.Detail); ok {
			e.Terminal = told.Proto()
		}
	}
	if a.Action == health.Action {
		// The one kind of event whose detail leaves the store: it holds what
		// internal/control/health wrote, and nothing else writes this action.
		e.Kind = dockwellv1.EventKind_EVENT_KIND_HEALTH
		e.Subject = &dockwellv1.EventSubject{Id: a.Subject, Name: look.Name(a.Subject)}
		e.Health = &dockwellv1.EventHealth{ServerId: a.Target, ServerName: look.Name(a.Target)}
		if told, ok := health.ReadTold(a.Detail); ok {
			e.Health.State, e.Health.Previous, e.Health.Output, e.Health.Flips = told.State, told.Previous, told.Output, told.Flips
			if told.Since != nil {
				e.Health.Since = timestamppb.New(*told.Since)
			}
		}
		return e
	}
	if slices.Contains(taskActions, a.Action) {
		// Only internal/control/tasks writes these, and what it writes holds nothing
		// a run printed.
		e.Kind = dockwellv1.EventKind_EVENT_KIND_TASK
		e.Subject = &dockwellv1.EventSubject{Id: a.Subject, Name: look.Name(a.Subject), Scope: look.Scope(a.Subject)}
		e.TaskRun = &dockwellv1.EventTaskRun{ServerName: look.Name(a.Target)}
		if told, ok := tasks.ReadTold(a.Detail); ok {
			e.TaskRun.Id, e.TaskRun.Task, e.TaskRun.DisplayName = told.Run, told.Task, told.DisplayName
			e.TaskRun.State, e.TaskRun.Reason = dockwellv1.TaskRunState(told.State), dockwellv1.TaskRunReason(told.Reason)
			e.TaskRun.ExitCode, e.TaskRun.HttpStatus, e.TaskRun.Error = told.ExitCode, told.HTTPStatus, told.Error
		}
		return e
	}
	if slices.Contains(backupActions, a.Action) {
		// As for health: only internal/control/backups writes these, and what it
		// writes holds no path and nothing of what's in a backup.
		e.Kind = dockwellv1.EventKind_EVENT_KIND_BACKUP
		e.Subject = &dockwellv1.EventSubject{Id: a.Subject, Name: look.Name(a.Subject)}
		e.Backup = &dockwellv1.EventBackup{ServerId: a.Target, ServerName: look.Name(a.Target)}
		if told, ok := backups.ReadTold(a.Detail); ok {
			e.Backup.Id, e.Backup.Volume, e.Backup.SizeBytes, e.Backup.Error, e.Backup.Destination = told.ID, told.Volume, told.Size, told.Error, told.Destination
			e.Backup.Method = resourcepb.BackupMethod(told.Method)
			e.Backup.Drill = backups.DrillResult(told.Drill)
		}
		return e
	}
	if slices.Contains(alertActions, a.Action) {
		// As for health: only internal/control/alerts writes these, and what it
		// writes holds nothing that signs in, nor any address.
		e.Kind = dockwellv1.EventKind_EVENT_KIND_ALERT
		if a.Subject != "" {
			e.Subject = &dockwellv1.EventSubject{Id: a.Subject, Name: look.Name(a.Subject), Scope: look.Scope(a.Subject)}
		}
		e.Alert = &dockwellv1.EventAlert{ServerId: a.Target, ServerName: look.Name(a.Target)}
		if told, ok := alerts.ReadTold(a.Detail); ok {
			e.Alert.Id, e.Alert.Rule, e.Alert.Title, e.Alert.Path = told.ID, told.Rule, told.Title, told.Path
			e.Alert.Reason, e.Alert.Detail = told.Reason, told.Detail
			e.Alert.Condition, e.Alert.SubjectName = resourcev1.AlertRule_Condition(told.Condition), told.SubjectName
			e.Alert.Variant, e.Alert.Where, e.Alert.Value = told.Facts.Variant, told.Facts.Where, told.Facts.Value
			if told.Facts.Server != "" && e.Alert.GetServerName() == "" {
				e.Alert.ServerName = told.Facts.Server
			}
			e.Alert.Severity = resourcev1.AlertRule_Severity(told.Severity)
			if told.OpenedAt != nil {
				e.Alert.OpenTime = timestamppb.New(*told.OpenedAt)
			}
			if told.Until != nil {
				e.Alert.Until = timestamppb.New(*told.Until)
			}
			if told.TakenFrom != "" {
				e.Alert.TakenFrom = look.Actor(told.TakenFrom)
			}
		}
		return e
	}
	if a.Target != "" {
		e.Subject = &dockwellv1.EventSubject{Id: a.Target, Name: look.Name(a.Target), Scope: look.Scope(a.Target)}
	}
	if strings.HasPrefix(a.Action, "member.") {
		// Who, with what role, where (ADR 0049): only internal/auth writes these.
		if m, ok := auth.ReadMemberDetail(a.Detail); ok {
			e.Member = &dockwellv1.EventMember{Email: m.Email, Name: m.Name, Role: authapi.RoleProto(m.Role), ProjectId: m.Project, Address: m.From}
			if m.Project != store.Install {
				e.Member.ProjectName = look.Name(m.Project)
			}
		}
	}
	if setting, ok := settings.ReadEvent(a.Action, a.Detail); ok {
		e.Kind, e.Setting = dockwellv1.EventKind_EVENT_KIND_SETTINGS, setting
	}
	if a.Action == fleet.ActionPublicAddress || a.Action == fleet.ActionPublicAddressClear {
		// Its detail leaves the store too: it's what the address was, to put it back.
		var change fleet.AddressChange
		if err := json.Unmarshal([]byte(a.Detail), &change); err == nil {
			e.PublicAddress = &dockwellv1.EventPublicAddress{Address: change.Address, Previous: change.Previous}
		}
	}
	if strings.HasPrefix(a.Action, "process.") {
		if d, ok := processes.ReadDetail(a.Detail); ok {
			e.Process = d
		}
	}
	if a.Action == revision.ActionDesiredSent || a.Action == revision.ActionDesiredUnknown {
		// Its detail leaves the store as counts: only the engine writes it, and it
		// holds no value, no spec, and the cache's ID stays behind.
		if sent, ok := revision.ReadDesiredSent(a.Detail); ok {
			e.DesiredSent = &dockwellv1.EventDesiredSent{
				Resources: uint32(max(sent.Resources, 0)), WithValues: uint32(max(sent.WithValues, 0)), Unknown: uint32(max(sent.Unknown, 0)), //nolint:gosec // counts of resources
				NewCache: sent.NewCache, FirstCache: sent.FirstCache, ServerName: look.Name(a.Target),
			}
		}
	}
	if slices.Contains(reconciled.Actions, a.Action) {
		// What a server's agent did on its own, which internal/control/reconciled
		// made fit to show before it kept it.
		if told, ok := reconciled.ReadTold(a.Detail); ok {
			e.Reconciled = &dockwellv1.EventReconciled{
				ServerName: look.Name(a.Target), Kind: told.Kind, Name: told.Name, Reason: told.Reason,
				Dropped: uint32(max(told.Dropped, 0)), //nolint:gosec // at most 1<<20
			}
		}
	}
	if a.Action == ActionRevealServiceValue && e.Subject != nil {
		e.Subject.ValueName = a.Subject // only ever a variable's name, as internal/control/variables writes it
	}
	if strings.HasPrefix(a.Action, "file.") && e.Subject != nil {
		e.Subject.Path = a.Subject // only ever a path, cleaned by internal/control/files
		// A write's detail leaves the store too: where it went, and what it took.
		if told, ok := controlfiles.ReadTold(a.Detail); ok && (slices.Contains(fileWrites, a.Action) || a.Action == fileDownload) {
			e.Subject.ToPath, e.Subject.WayBackIn, e.Subject.ForGood, e.Subject.SizeBytes = told.To, told.WayBackIn, told.ForGood, told.Size
		}
	}
	return e
}

// filter reads a request's filters.
func filter(kinds []dockwellv1.EventKind, serverID, resourceID, actorID string, actorKind dockwellv1.ActorKind) (store.TimelineFilter, error) {
	f := store.TimelineFilter{ServerID: serverID, ResourceID: resourceID}
	want := map[dockwellv1.EventKind]bool{}
	for _, k := range kinds {
		if _, ok := dockwellv1.EventKind_name[int32(k)]; !ok || k == dockwellv1.EventKind_EVENT_KIND_UNSPECIFIED {
			return f, authapi.Invalid(fmt.Errorf("kinds can't hold %v. Leave kinds out for every kind, or name some, such as EVENT_KIND_CHANGE", k))
		}
		want[k] = true
	}
	every := len(want) == 0
	f.Changes = every || want[dockwellv1.EventKind_EVENT_KIND_CHANGE]
	if every || want[dockwellv1.EventKind_EVENT_KIND_SERVER] {
		f.Actions = append(f.Actions, serverActions...)
	}
	if every || want[dockwellv1.EventKind_EVENT_KIND_ACCESS] {
		f.Actions = append(f.Actions, accessActions...)
	}
	if every || want[dockwellv1.EventKind_EVENT_KIND_HEALTH] {
		f.Actions = append(f.Actions, healthActions...)
	}
	if every || want[dockwellv1.EventKind_EVENT_KIND_BACKUP] {
		f.Actions = append(f.Actions, backupActions...)
	}
	if every || want[dockwellv1.EventKind_EVENT_KIND_SETTINGS] {
		f.Actions = append(f.Actions, settingsActions...)
	}
	if every || want[dockwellv1.EventKind_EVENT_KIND_TASK] {
		f.Actions = append(f.Actions, taskActions...)
	}
	if every || want[dockwellv1.EventKind_EVENT_KIND_ALERT] {
		f.Actions = append(f.Actions, alertActions...)
	}
	f.ResourceActions = slices.Concat(healthActions, backupActions, alertActions, taskActions)
	if actorID != "" {
		a, ok := actors.Store(actorID)
		if !ok {
			return f, authapi.Invalid(fmt.Errorf("actor_id %q isn't an actor's ID. Use the id an event's actor has, such as usr_…, tok_…, srv_… or mira@hz-1", actorID))
		}
		f.Actor = a
	}
	if actorKind != dockwellv1.ActorKind_ACTOR_KIND_UNSPECIFIED {
		if f.ActorPrefixes = actors.Prefixes(actorKind); f.ActorPrefixes == nil {
			return f, authapi.Invalid(fmt.Errorf("actor_kind %v isn't a kind of actor this control plane knows. Leave it out for every kind", actorKind))
		}
	}
	return f, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (h *Handler) internal(ctx context.Context, err error) error {
	var cerr *connect.Error
	switch {
	case errors.As(err, &cerr):
		return err
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "event call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read the timeline; try again, and check its logs if it keeps failing"))
}
