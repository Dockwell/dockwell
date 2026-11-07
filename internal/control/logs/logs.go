// SPDX-License-Identifier: AGPL-3.0-only

// Package logs serves dockwell.v1.LogService: a service's logs, which each
// server's agent reads from Docker as they are asked for, and which pass
// through here on the way to a reader without being stored. See
// docs/adr/0025-logs-per-deploy.md.
package logs

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	agentlogs "github.com/dockwell/dockwell/internal/agent/logs"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/containers"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// Limits of a reader.
const (
	// DefaultPageSize and MaxPageSize bound a page of ReadLogs.
	DefaultPageSize = 1000
	MaxPageSize     = 5000
	// DefaultTail and MaxTail bound what a stream starts with, per container.
	DefaultTail = 200
	MaxTail     = 100_000
	// gather is how long a stream waits after a line for what comes with it,
	// so that a burst makes one message.
	gather = 50 * time.Millisecond
	// messageBytes is how much of a message goes out at once, past which it
	// doesn't wait.
	messageBytes = 256 << 10
	// MaxQuery is how long a search can be, in bytes. A longer one finds little
	// that part of it wouldn't, and it travels down the agent's main stream.
	MaxQuery = 1 << 10
	// pageBytes is the most of log lines a page of ReadLogs holds, newest kept,
	// as they come out in JSON, its largest form: under the 8 MiB the REST path
	// holds of a message, with room for the rest of the page.
	pageBytes = 7 << 20
	// pageLineBytes is about what a line costs in that form, past its text: its
	// time, stream, level and field names.
	pageLineBytes = 140
	// maxPagesPerReader and maxPages bound how many pages are being built or
	// sent at once, for one reader and in all: each holds up to pageBytes and
	// more, for as long as its reader takes to read it.
	maxPagesPerReader, maxPages = 4, 8
	// placementEvery is how often a stream looks for servers the service moved
	// to, while it follows.
	placementEvery = 5 * time.Second
	// retryEvery is how soon a stream tries a server again whose agent couldn't
	// read logs, as when Docker wasn't running.
	retryEvery = 5 * time.Second
	// MoveGrace is how long a read waits for a server whose agent is moving to a new
	// stream, as it does each time its certificate is renewed, before it says the
	// server is offline: as long as the server reads as connected everywhere else.
	MoveGrace = agentapi.MoveGrace
	// AuditEvery is how often one reader's reading of one service's logs goes
	// in the audit log, at most, and how often it does while a stream stays open.
	AuditEvery = time.Hour
	// auditEntries bounds what is remembered of who read what.
	auditEntries = 10_000
	// callWait bounds a server's answer to which sources it has.
	callWait = 15 * time.Second
)

// Handler implements LogService.
type Handler struct {
	agents     *agentapi.Agents
	changes    *changes.Handler
	containers *containers.Handler
	db         *store.DB
	logger     *slog.Logger
	now        func() time.Time
	options    Options

	auditMu sync.Mutex
	audited map[string]time.Time

	pagesMu sync.Mutex
	pages   map[string]int
}

// Options changes a Handler's limits, as tests do. Zero means the default.
type Options struct {
	// AuditEvery replaces AuditEvery.
	AuditEvery time.Duration
	// WriteStall replaces how long a write to a reader can make no progress.
	WriteStall time.Duration
	// Now replaces the time.
	Now func() time.Time
	// TellEvery replaces tellEvery.
	TellEvery time.Duration
}

var _ dockwellv1connect.LogServiceHandler = (*Handler)(nil)

// tellEvery is how often servers hear what deletions put out of reach, besides when
// they connect. What is out of reach already is, as the control plane reads nothing
// of it: this is for what leaves the disk, which goes by the day.
const tellEvery = time.Hour

// KeepTelling tells every connected server what deletions put out of reach, every
// tellEvery until ctx is done.
func (h *Handler) KeepTelling(ctx context.Context) {
	every := h.options.TellEvery
	if every <= 0 {
		every = tellEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			h.TellAll(ctx)
		}
	}
}

// TellAll tells every connected server what deletions put out of reach.
func (h *Handler) TellAll(ctx context.Context) {
	for _, server := range h.agents.Connected() {
		h.tell(ctx, server)
	}
}

// tell says to one server what deletions put out of reach: all of it, so a server
// that was away or joined since catches up, and of every service, as any server may
// hold what a service left. With nothing to say it says nothing: at worst a server goes
// on thinking something is out of reach that undoing brought back, which only sets the
// order things leave in if it runs out of room.
func (h *Handler) tell(ctx context.Context, server string) {
	session, ok := h.agents.Session(server)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	now := h.now()
	var services []*agentv1.ForgottenService
	err := h.db.View(ctx, func(tx *store.Tx) error {
		found, err := revision.ForgottenServices(ctx, tx, string(service.Kind), now.Add(-agentlogs.RetainFor))
		if err != nil {
			return err
		}
		services = make([]*agentv1.ForgottenService, len(found))
		for i := range found {
			f := &found[i]
			services[i] = &agentv1.ForgottenService{
				Resource:     &dockwellv1.ResourceRef{Kind: f.Resource.Kind, Scope: f.Resource.Scope, Name: f.Resource.Name},
				UpToRevision: int64(f.UpTo),
				Expired:      now.Sub(f.At) >= revision.UndoWindow,
			}
		}
		return nil
	})
	// An empty list says something too: an undone deletion no longer counts against
	// what a server keeps.
	if err == nil {
		err = session.Forgotten(ctx, services)
	}
	if err != nil && ctx.Err() == nil {
		h.logger.WarnContext(ctx, "can't tell a server what deletions put out of reach; it hears again within the hour", slog.String("server", server), slog.String("error", err.Error()))
	}
}

// New returns a handler that reads logs through agents.
func New(agents *agentapi.Agents, ch *changes.Handler, db *store.DB, logger *slog.Logger, options Options) *Handler {
	if options.AuditEvery <= 0 {
		options.AuditEvery = AuditEvery
	}
	if options.WriteStall <= 0 {
		options.WriteStall = writeStall
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	h := &Handler{
		agents: agents, changes: ch, containers: containers.New(agents, ch, db, logger), db: db, logger: logger, now: options.Now, options: options,
		audited: map[string]time.Time{}, pages: map[string]int{},
	}
	agents.OnConnect(func(server string) { h.tell(context.Background(), server) })
	return h
}

// server is one server a service is on, or was on.
type server struct {
	id, name string
	// placed says the service runs there now, as opposed to having run there,
	// which leaves only what the server kept.
	placed bool
}

// target is a service asked for by a request, or a container Dockwell didn't make.
type target struct {
	id          string
	ref         resource.Ref
	environment string
	// container is the full ID of a container Dockwell didn't make, for a target that
	// is one. It runs on the one server in servers, which ref's scope names.
	container string
	// deleted says it has no spec now, which a deletion leaves, and so does a first
	// deploy that failed and was rolled back, whose logs can be read.
	deleted bool
	// forgotten is what a deletion put out of reach.
	forgotten revision.Forgotten
	servers   []server
}

func (t *target) proto() *dockwellv1.LoggedService {
	return &dockwellv1.LoggedService{Id: t.id, Name: t.ref.Name, Environment: t.environment, Deleted: t.deleted}
}

// subject is what the audit log says was read: a service, or the server of a
// container, as a container has no ID of Dockwell's. It goes by the server's own,
// which lasts longer than a container does.
func (t *target) subject() string {
	if t.container != "" {
		return t.ref.Scope
	}
	return t.id
}

// containerName is the name a container target goes by, which a follow reads whatever
// holds. A service has none.
func (t *target) containerName() string {
	if t.container == "" {
		return ""
	}
	return t.ref.Name
}

// said is how the audit log names what was read.
func (t *target) said() string {
	if t.container != "" {
		return "the container " + t.ref.Name + " on " + t.servers[0].name
	}
	return t.ref.Name
}

// resolveAny finds what a request names: a service, or with a server, a container
// Dockwell didn't make there.
func (h *Handler) resolveAny(ctx context.Context, name, project, environment, onServer string) (*target, error) {
	if onServer == "" {
		return h.resolve(ctx, name, project, environment)
	}
	if name == "" {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New("name a container, by its name or its full ID"), "invalid")
	}
	if project != "" || environment != "" {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New("a server names a container, which lives in no project or environment, so leave those out"), "invalid")
	}
	s, c, err := h.containers.Find(ctx, onServer, name)
	if err != nil {
		return nil, err
	}
	return &target{
		id: c.GetId(), container: c.GetId(), ref: resource.Ref{Kind: container.Kind, Scope: s.ID, Name: c.GetName()},
		servers: []server{{id: s.ID, name: s.Name, placed: true}},
	}, nil
}

// resolve finds the service a request names. One with no spec can be asked for by
// ID, as what its servers kept can still be read when its first deploy failed and was
// rolled back. One that was deleted can't, by any name, on any server, whether or not
// that server has heard, as nothing is asked of it.
func (h *Handler) resolve(ctx context.Context, name, project, environment string) (*target, error) {
	if name == "" {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New("name a service, by its name or its ID"), "invalid")
	}
	byID := strings.HasPrefix(name, id.Service+"_")
	var envID string
	if !byID {
		_, env, err := h.changes.Environment(ctx, project, environment)
		if err != nil {
			return nil, err
		}
		envID = env.ID
	}
	t := &target{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var res store.Resource
		var err error
		if byID {
			res, err = tx.GetResourceByID(ctx, name)
		} else {
			res, err = tx.GetResource(ctx, string(service.Kind), envID, name)
		}
		if err != nil {
			return err
		}
		if res.Kind != string(service.Kind) {
			return store.ErrNotFound
		}
		t.id, t.deleted = res.ID, res.Spec == nil
		t.ref = resource.Ref{Kind: service.Kind, Scope: res.Scope, Name: res.Name}
		if t.forgotten, err = revision.ForgottenOf(ctx, tx, res); err != nil {
			return err
		}
		if env, err := tx.GetResourceByID(ctx, res.Scope); err == nil {
			t.environment = env.Name
		}
		return h.servers(ctx, tx, t, res)
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no service %s. Check its name with dockwell resources list --kind service", name), "not_found")
	case err != nil:
		return nil, h.internal(ctx, err)
	case t.forgotten.Deleted:
		return nil, deletedError(t.ref.Name)
	}
	return t, nil
}

// deletedError refuses to read what a deleted service left.
func deletedError(name string) error {
	return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("%s was deleted, so what its servers kept can't be read. Undoing the deletion within 7 days brings it back", name), "not_found")
}

// servers finds where a service is and has been. A server that was removed from
// the fleet can't be asked.
func (h *Handler) servers(ctx context.Context, tx *store.Tx, t *target, res store.Resource) error {
	deps, err := tx.Deployments(ctx, res.ID, 0)
	if err != nil {
		return err
	}
	placed := map[string]bool{}
	if res.Spec != nil {
		now, err := tx.Placement(ctx, res.ID, res.Revision)
		if err != nil {
			return err
		}
		for _, s := range now {
			placed[s] = true
		}
	}
	seen := map[string]bool{}
	t.servers = nil
	for i := range deps {
		serverID := deps[i].Server
		if seen[serverID] {
			continue
		}
		seen[serverID] = true
		s, err := tx.GetServer(ctx, serverID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if s.RevokedAt == nil {
			t.servers = append(t.servers, server{id: s.ID, name: s.Name, placed: placed[s.ID]})
		}
	}
	slices.SortFunc(t.servers, func(a, b server) int { return cmp.Compare(a.name, b.name) })
	return nil
}

func (h *Handler) internal(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return connect.NewError(connect.CodeCanceled, ctx.Err())
	}
	h.logger.ErrorContext(ctx, "can't read a service's logs", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its state; try again, and check its logs if it keeps failing"))
}

// record puts reading a service's logs in the audit log: at most once an hour
// for one reader and one service, as a reader who reconnects, pages or searches
// is still doing the one thing, and a timeline of it would bury everything else.
// It says what was read and by whom, never what was searched for, which may
// itself be sensitive. A reading that can't be recorded isn't served. A restart
// forgets what was recorded, which costs one more event for each.
func (h *Handler) record(ctx context.Context, t *target, what string) error {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil
	}
	actor := p.Actor()
	key := actor + " " + t.id
	now := h.now()
	h.auditMu.Lock()
	last, seen := h.audited[key]
	due := !seen || now.Sub(last) >= h.options.AuditEvery
	if due {
		if len(h.audited) >= auditEntries {
			h.forgetOldest()
		}
		h.audited[key] = now
	}
	h.auditMu.Unlock()
	if !due {
		return nil
	}
	client, _ := authapi.ClientFrom(ctx)
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: actor, Action: "logs.read", Subject: t.subject(), Target: t.subject(),
			Detail: what + " " + t.said(), Surface: client.Surface(p),
		})
	})
	if err != nil {
		h.auditMu.Lock()
		delete(h.audited, key)
		h.auditMu.Unlock()
		return h.internal(ctx, err)
	}
	return nil
}

// recordEnd puts the end of a long stream in the audit log, so the log says when
// a reader stopped seeing what was printed. A stream shorter than the time between
// events counts towards the once an hour, so a client that reconnects all day
// doesn't flood it. Its request has ended by now, so it has a time of its own.
func (h *Handler) recordEnd(ctx context.Context, t *target, began time.Time) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return
	}
	actor := p.Actor()
	key := actor + " ended " + t.id
	now := h.now()
	lasted := now.Sub(began)
	h.auditMu.Lock()
	last, seen := h.audited[key]
	due := lasted >= h.options.AuditEvery || !seen || now.Sub(last) >= h.options.AuditEvery
	if due {
		if len(h.audited) >= auditEntries {
			h.forgetOldest()
		}
		h.audited[key] = now
	}
	h.auditMu.Unlock()
	if !due {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callWait)
	defer cancel()
	client, _ := authapi.ClientFrom(ctx)
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: actor, Action: "logs.read", Subject: t.subject(), Target: t.subject(),
			Detail: "stopped following " + t.said() + " after " + human.Duration(lasted.Round(time.Second)), Surface: client.Surface(p),
		})
	})
	if err != nil {
		h.logger.WarnContext(ctx, "can't put the end of reading a service's logs in the audit log", slog.String("service", t.ref.Name), slog.String("error", err.Error()))
	}
}

// forgetOldest makes room in what is remembered of who read what, which costs
// at most one more event each.
func (h *Handler) forgetOldest() {
	oldest, at := "", time.Time{}
	for k, t := range h.audited {
		if oldest == "" || t.Before(at) {
			oldest, at = k, t
		}
	}
	delete(h.audited, oldest)
}

// ListLogSources returns the containers a service has logs from.
func (h *Handler) ListLogSources(ctx context.Context, req *connect.Request[dockwellv1.ListLogSourcesRequest]) (*connect.Response[dockwellv1.ListLogSourcesResponse], error) {
	m := req.Msg
	t, err := h.resolveAny(ctx, m.GetId(), m.GetProject(), m.GetEnvironment(), m.GetServer())
	if err != nil {
		return nil, err
	}
	res := &dockwellv1.ListLogSourcesResponse{Service: t.proto()}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range t.servers {
		wg.Go(func() {
			sources, status := h.sourcesOn(ctx, t, s)
			mu.Lock()
			defer mu.Unlock()
			res.Sources = append(res.Sources, sources...)
			if status != nil {
				res.Servers = append(res.Servers, status)
			}
		})
	}
	wg.Wait()
	slices.SortStableFunc(res.Sources, func(a, b *dockwellv1.LogSource) int {
		return b.GetCreateTime().AsTime().Compare(a.GetCreateTime().AsTime())
	})
	slices.SortFunc(res.Servers, func(a, b *dockwellv1.ServerLogStatus) int { return cmp.Compare(a.GetServerName(), b.GetServerName()) })
	return connect.NewResponse(res), nil
}

func (h *Handler) sourcesOn(ctx context.Context, t *target, s server) ([]*dockwellv1.LogSource, *dockwellv1.ServerLogStatus) {
	session, ok := h.agents.SessionPastMove(ctx, s.id, MoveGrace)
	if !ok {
		return nil, offline(s)
	}
	ctx, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	sources, err := session.LogSources(ctx, &agentv1.LogSourcesCall{Resource: resourceRef(t.ref), ForgottenUpTo: int64(t.forgotten.UpTo), Container: t.container})
	if err != nil {
		return nil, unavailable(s, err)
	}
	for _, src := range sources {
		src.ServerId, src.ServerName = s.id, s.name
	}
	return sources, nil
}

func resourceRef(r resource.Ref) *dockwellv1.ResourceRef {
	return &dockwellv1.ResourceRef{Kind: string(r.Kind), Scope: r.Scope, Name: r.Name}
}

func offline(s server) *dockwellv1.ServerLogStatus {
	return &dockwellv1.ServerLogStatus{
		ServerId: s.id, ServerName: s.name, State: dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE,
		Message: "the agent on " + s.name + " isn't connected, so its logs can't be read now. Its containers keep running and logging. Check the server; its logs carry on when its agent reconnects",
	}
}

func unavailable(s server, err error) *dockwellv1.ServerLogStatus {
	var se *resource.ServerError
	msg := err.Error()
	if errors.As(err, &se) {
		msg = se.Err.Error()
	}
	return &dockwellv1.ServerLogStatus{
		ServerId: s.id, ServerName: s.name, State: dockwellv1.ServerLogState_SERVER_LOG_STATE_UNAVAILABLE,
		Message: fmt.Sprintf("%s can't read logs now: %s", s.name, msg),
	}
}

// checkRange says whether a request's time range makes sense. A length of time
// goes to each server as it is, as a line's time is by its server's clock.
func checkRange(since *timestamppb.Timestamp, last *durationpb.Duration) error {
	if last == nil {
		return nil
	}
	if since != nil {
		return authapi.ReasonError(connect.CodeInvalidArgument, errors.New("say since or last, not both"), "invalid")
	}
	if err := last.CheckValid(); err != nil || last.AsDuration() <= 0 {
		return authapi.ReasonError(connect.CodeInvalidArgument, errors.New("last needs a length of time longer than zero, such as 15m"), "invalid")
	}
	return nil
}

// checkQuery refuses a search too long to be one.
func checkQuery(query string) error {
	if len(query) > MaxQuery {
		return authapi.ReasonError(connect.CodeInvalidArgument, fmt.Errorf("a search takes up to %d bytes of text, and this one has %d. Search for part of it", MaxQuery, len(query)), "invalid")
	}
	return nil
}

// reader says who reads, for what a server keeps of its places for one reader.
func reader(ctx context.Context) string {
	if p, ok := authapi.PrincipalFrom(ctx); ok {
		return p.Actor()
	}
	return ""
}

// takePage holds one of the places for a page being built or sent. It is let go
// of once the page has been written, or the writing has given up, by Guard.
func (h *Handler) takePage(ctx context.Context) error { return h.takePageFor(ctx, reader(ctx)) }

func (h *Handler) takePageFor(ctx context.Context, who string) error {
	h.pagesMu.Lock()
	defer h.pagesMu.Unlock()
	total := 0
	for _, n := range h.pages {
		total += n
	}
	switch {
	case total >= maxPages:
		return authapi.ReasonError(connect.CodeResourceExhausted, fmt.Errorf("the control plane is building %d pages of logs, the most it does at once. Try again in a moment", maxPages), "busy")
	case h.pages[who] >= maxPagesPerReader:
		return authapi.ReasonError(connect.CodeResourceExhausted, fmt.Errorf("you are reading %d pages of logs at once, the most one reader can. Read those first, then ask for more", maxPagesPerReader), "busy")
	}
	h.pages[who]++
	release := func() {
		h.pagesMu.Lock()
		defer h.pagesMu.Unlock()
		h.pages[who]--
		if h.pages[who] <= 0 {
			delete(h.pages, who)
		}
	}
	if !holdUntilWritten(ctx, release) {
		// Nothing outside says when it has been written, as in a test that calls
		// the handler itself.
		context.AfterFunc(ctx, release)
	}
	return nil
}

// ReadLogs returns a page of a service's log lines.
func (h *Handler) ReadLogs(ctx context.Context, req *connect.Request[dockwellv1.ReadLogsRequest]) (*connect.Response[dockwellv1.ReadLogsResponse], error) {
	m := req.Msg
	t, err := h.resolveAny(ctx, m.GetId(), m.GetProject(), m.GetEnvironment(), m.GetServer())
	if err != nil {
		return nil, err
	}
	size, err := paging.Size(m.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	if err := checkRange(m.GetSince(), m.GetLast()); err != nil {
		return nil, err
	}
	if err := checkQuery(m.GetQuery()); err != nil {
		return nil, err
	}
	if err := checkLists(m.GetSourceIds(), m.GetLevels(), m.GetChangeId()); err != nil {
		return nil, err
	}
	before, err := readPageToken(m.GetPageToken(), t.servers)
	if err != nil {
		return nil, err
	}
	if err := h.takePage(ctx); err != nil {
		return nil, err
	}
	if err := h.record(ctx, t, "read"); err != nil {
		return nil, err
	}
	who := reader(ctx)
	// Each server is asked for what is its own, so what an agent is told stays within
	// what it takes, however many servers a service is on. A next page reads only the
	// sources with more to read, and not a server with none.
	call := func(server string) *agentv1.LogsCall {
		sources := m.GetSourceIds()
		if m.GetPageToken() != "" {
			sources = before.sourceIDs(server)
			if len(m.GetSourceIds()) > 0 {
				sources = slices.DeleteFunc(sources, func(source string) bool { return !slices.Contains(m.GetSourceIds(), source) })
			}
			if len(sources) == 0 {
				return nil
			}
		}
		return &agentv1.LogsCall{
			Resource: resourceRef(t.ref), Since: m.GetSince(), Last: m.GetLast(), Until: m.GetUntil(), Tail: int32(size), //nolint:gosec // bounded by MaxPageSize
			SourceIds: sources, Revision: m.GetRevision(), ChangeId: m.GetChangeId(), ForgottenUpTo: int64(t.forgotten.UpTo),
			Levels: m.GetLevels(), Query: m.GetQuery(), IncludeKept: true, Before: before.cursors(server), Reader: who, Container: t.container,
		}
	}
	page := newPage(size, before)
	var mu sync.Mutex
	var wg sync.WaitGroup
	res := &dockwellv1.ReadLogsResponse{}
	for _, s := range t.servers {
		wg.Go(func() {
			// A move cuts a read short, so it starts again over the next stream,
			// all within the one grace.
			until := time.Now().Add(MoveGrace)
			for {
				session, ok := h.agents.SessionPastMove(ctx, s.id, time.Until(until))
				if !ok {
					mu.Lock()
					res.Servers = append(res.Servers, offline(s))
					mu.Unlock()
					return
				}
				c := call(s.id)
				if c == nil {
					return
				}
				err := session.Logs(ctx, c, func(u *agentv1.SendLogsRequest) error {
					mu.Lock()
					defer mu.Unlock()
					for _, src := range u.GetSources() {
						src.ServerId, src.ServerName = s.id, s.name
					}
					page.add(s.id, u)
					return nil
				})
				if errors.Is(err, agentapi.ErrMoving) && time.Now().Before(until) {
					mu.Lock()
					page.forget(s.id)
					mu.Unlock()
					continue
				}
				if err != nil && ctx.Err() == nil {
					status := unavailable(s, err)
					if errors.Is(err, resource.ErrLost) || errors.Is(err, resource.ErrNotConnected) {
						status = offline(s)
					}
					mu.Lock()
					res.Servers = append(res.Servers, status)
					mu.Unlock()
				}
				return
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, connect.NewError(connect.CodeCanceled, err)
	}
	page.finish(res, m.GetQuery() != "" || len(m.GetLevels()) > 0)
	slices.SortFunc(res.Servers, func(a, b *dockwellv1.ServerLogStatus) int { return cmp.Compare(a.GetServerName(), b.GetServerName()) })
	return connect.NewResponse(res), nil
}

// page gathers what several servers send for one page, within pageBytes.
type page struct {
	size int
	// before is where each source's last page ended, and on which server each
	// source is.
	before  map[string][2]int64
	on      map[string]string
	lines   map[string][]*dockwellv1.LogLine
	sources map[string]*dockwellv1.LogSource
	// scans is what each server says it searched.
	scans map[string][]*agentv1.LogScan
	bytes int
	// gone is what a source's log is missing, which a marker says.
	gone map[string]*dockwellv1.LogGap
	// cut says a source has older lines than those kept: some were dropped
	// here, or its agent didn't send them.
	cut map[string]bool
}

func newPage(size int, before pageToken) *page {
	return &page{
		size: size, before: before.flat(), on: map[string]string{}, lines: map[string][]*dockwellv1.LogLine{}, sources: map[string]*dockwellv1.LogSource{}, scans: map[string][]*agentv1.LogScan{},
		gone: map[string]*dockwellv1.LogGap{}, cut: map[string]bool{},
	}
}

// lineCost is what a line comes to in JSON, as that is what a page is limited by:
// a quote takes two bytes, and a control character such as a colour code's six.
func lineCost(l *dockwellv1.LogLine) int {
	text := l.GetText()
	n := pageLineBytes + len(text)
	for i := range len(text) {
		switch c := text[i]; {
		case c == '"' || c == '\\':
			n++
		case c < 0x20 || c == 0x7f:
			n += 5
		}
	}
	return n
}

func (p *page) add(server string, u *agentv1.SendLogsRequest) {
	for _, s := range u.GetSources() {
		p.sources[s.GetId()] = s
		p.on[s.GetId()] = server
	}
	p.scans[server] = append(p.scans[server], u.GetScans()...)
	for _, s := range u.GetScans() {
		p.on[s.GetSourceId()] = server
		if s.GetOlder() {
			p.cut[s.GetSourceId()] = true
		}
	}
	for _, c := range u.GetChunks() {
		p.on[c.GetSourceId()] = server
		if g := c.GetGap(); g.GetReason() == dockwellv1.LogGapReason_LOG_GAP_REASON_GONE {
			p.gone[c.GetSourceId()] = g
		}
		p.lines[c.GetSourceId()] = append(p.lines[c.GetSourceId()], c.GetLines()...)
		for _, l := range c.GetLines() {
			p.bytes += lineCost(l)
		}
	}
	if p.bytes > 2*pageBytes {
		p.trim(pageBytes)
	}
}

// forget drops what a server has sent, as a read that starts again sends it again.
func (p *page) forget(server string) {
	delete(p.scans, server)
	for source, on := range p.on {
		if on != server {
			continue
		}
		delete(p.on, source)
		delete(p.sources, source)
		delete(p.gone, source)
		delete(p.cut, source)
		for _, l := range p.lines[source] {
			p.bytes -= lineCost(l)
		}
		delete(p.lines, source)
	}
}

// trim drops the oldest lines, of whichever source, until what is left fits. Of
// lines with one time it keeps a source's newest, as they come later in its log.
func (p *page) trim(limit int) {
	type at struct {
		source string
		line   *dockwellv1.LogLine
	}
	var all []at
	for source, lines := range p.lines {
		for _, l := range slices.Backward(lines) {
			all = append(all, at{source, l})
		}
	}
	slices.SortStableFunc(all, func(a, b at) int { return b.line.GetTime().AsTime().Compare(a.line.GetTime().AsTime()) })
	kept := map[string][]*dockwellv1.LogLine{}
	bytes := 0
	n := 0
	for ; n < len(all) && n < p.size && bytes < limit; n++ {
		kept[all[n].source] = append(kept[all[n].source], all[n].line)
		bytes += lineCost(all[n].line)
	}
	for _, a := range all[n:] {
		p.cut[a.source] = true
	}
	for source := range kept {
		slices.Reverse(kept[source])
	}
	p.lines, p.bytes = kept, bytes
}

// finish writes the page into res, with where the next one starts: for each
// source that may have older lines, what the page has of it. A source read in
// full, with fewer lines than a page, has no more.
func (p *page) finish(res *dockwellv1.ReadLogsResponse, searching bool) {
	counts := map[string]int{}
	for source, lines := range p.lines {
		counts[source] = len(lines)
	}
	p.trim(pageBytes)
	next := pageToken{}
	oldest := time.Time{}
	for _, lines := range p.lines {
		if len(lines) > 0 {
			if t := lines[0].GetTime().AsTime(); oldest.IsZero() || t.Before(oldest) {
				oldest = t
			}
		}
	}
	for source, n := range counts {
		kept := p.lines[source]
		if n < p.size && !p.cut[source] {
			continue
		}
		switch {
		case len(kept) > 0:
			next.set(p.on[source], source, p.placeOf(source, kept))
		case !oldest.IsZero():
			// All of it was older than what the page kept.
			next.set(p.on[source], source, [2]int64{oldest.UnixNano()})
		}
	}
	res.NextPageToken = next.encode()
	for _, source := range slices.Sorted(maps.Keys(p.lines)) {
		chunk := &dockwellv1.LogChunk{SourceId: source, Lines: p.lines[source]}
		// A place a log is missing lines goes with the page that reaches it.
		if g := p.gone[source]; g != nil && len(chunk.GetLines()) > 0 && !g.GetEndTime().AsTime().Before(chunk.GetLines()[0].GetTime().AsTime()) {
			chunk.Gap = g
		}
		res.Chunks = append(res.Chunks, chunk)
	}
	res.Sources = slices.Collect(maps.Values(p.sources))
	slices.SortFunc(res.Sources, func(a, b *dockwellv1.LogSource) int {
		return b.GetCreateTime().AsTime().Compare(a.GetCreateTime().AsTime())
	})
	if !searching {
		return
	}
	for _, scans := range p.scans {
		for _, s := range scans {
			res.SearchedLines += s.GetLines()
			res.SearchCutShort = res.SearchCutShort || s.GetCutShort()
			res.SearchTimedOut = res.SearchTimedOut || s.GetTimedOut()
			if t := s.GetOldestTime(); t != nil && (res.SearchedBackTo == nil || t.AsTime().Before(res.SearchedBackTo.AsTime())) {
				res.SearchedBackTo = t
			}
		}
	}
}

// placeOf is where the next page starts in a source: the time of the oldest line
// kept, and how many lines this page and those before hold of that time, as lines
// can share one, and those left of it are for the next page.
func (p *page) placeOf(source string, kept []*dockwellv1.LogLine) [2]int64 {
	t := kept[0].GetTime().AsTime()
	held := int64(0)
	for _, l := range kept {
		if !l.GetTime().AsTime().Equal(t) {
			break
		}
		held++
	}
	if was := p.before[source]; was[0] == t.UnixNano() {
		held += was[1]
	}
	return [2]int64{t.UnixNano(), held}
}

// pageToken is where the next page starts: for each server, and each of its sources,
// the time of the oldest line the page before held, in nanoseconds, and how many
// lines the pages so far hold of that time.
type pageToken map[string]map[string][2]int64

func (t pageToken) set(server, source string, place [2]int64) {
	if t[server] == nil {
		t[server] = map[string][2]int64{}
	}
	t[server][source] = place
}

func (t pageToken) encode() string {
	if len(t) == 0 {
		return ""
	}
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

func readPageToken(s string, servers []server) (pageToken, error) {
	if s == "" {
		return nil, nil
	}
	if err := checkTokenSize("page_token", s); err != nil {
		return nil, err
	}
	t := pageToken{}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &t)
	}
	if err != nil {
		return nil, badToken("page_token", "isn't one a page of logs gave")
	}
	for server, places := range t {
		if !isServerOf(servers, server) {
			delete(t, server)
			continue
		}
		if err := checkPlaces("page_token", places); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// flat is every source's place, whichever server it is on.
func (t pageToken) flat() map[string][2]int64 {
	out := map[string][2]int64{}
	for _, places := range t {
		maps.Copy(out, places)
	}
	return out
}

func (t pageToken) cursors(server string) []*agentv1.LogCursor {
	var out []*agentv1.LogCursor
	for _, source := range slices.Sorted(maps.Keys(t[server])) {
		place := t[server][source]
		out = append(out, &agentv1.LogCursor{SourceId: source, Time: timestamppb.New(time.Unix(0, place[0])), Count: uint32(place[1])}) //nolint:gosec // checked as it was read
	}
	return out
}

func (t pageToken) sourceIDs(server string) []string { return slices.Sorted(maps.Keys(t[server])) }

// resumeToken is where a stream carries on: for each server, where each source
// has got to, and the time of the last line, by that server's clock.
type resumeToken struct {
	Servers map[string]*serverPlace `json:"s"`
}

type serverPlace struct {
	// Last is the time of the last line from the server, in nanoseconds.
	Last int64 `json:"l"`
	// From is since when a source with no place here is read, by the server's clock:
	// when the stream began, as a source that printed nothing in what the reader has
	// may have printed before another source's last line. It moves on only past a
	// source the token forgot, which reading from further back would repeat. So only
	// on a server with over maxPlaces sources can one with no place lose lines.
	From int64 `json:"f,omitempty"`
	// Sources are where each source has got to, as a time and how many lines
	// have just that time.
	Sources map[string][2]int64 `json:"c"`
}

func (t *resumeToken) encode() string {
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

func readResumeToken(s string, servers []server) (*resumeToken, error) {
	t := &resumeToken{Servers: map[string]*serverPlace{}}
	if s == "" {
		return t, nil
	}
	if err := checkTokenSize("resume_token", s); err != nil {
		return nil, err
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, t)
	}
	if err != nil || t.Servers == nil {
		return nil, badToken("resume_token", "isn't one a stream of logs gave")
	}
	for server, place := range t.Servers {
		if !isServerOf(servers, server) {
			delete(t.Servers, server)
			continue
		}
		if place == nil {
			return nil, badToken("resume_token", "gives no place on a server")
		}
		if place.Last < 0 || place.From < 0 {
			return nil, badToken("resume_token", "gives a place in a log that can't be one")
		}
		if err := checkPlaces("resume_token", place.Sources); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// isServerOf says whether a server is one of those a service is or has been on.
func isServerOf(servers []server, serverID string) bool {
	return slices.ContainsFunc(servers, func(s server) bool { return s.id == serverID })
}

const (
	// maxPlaces bounds how many sources a token names on one server, the newest
	// kept. A source it forgets, which has long gone quiet, is read again by time.
	// It is what an agent takes of a call, which it checks for itself.
	maxPlaces = agentlogs.MaxNamedSources
	// maxTokenBytes bounds a token before anything reads it. It holds every source
	// of about 170 servers, each at maxPlaces, and many more of what really runs.
	maxTokenBytes = 1 << 20
	// maxLevels is more than the levels there are, for a list that repeats one.
	maxLevels = agentlogs.MaxLevels
	// maxChangeID bounds a change to filter by, far longer than any is.
	maxChangeID = 64
)

// A token comes back from whoever holds it, so nothing in it is taken on trust: its
// size, how many sources it names, what they are called and the times it gives are
// all checked, and what it says of a server that isn't this service's goes unread.

// badToken refuses a token, saying what is wrong with it and what to do.
func badToken(field, why string) error {
	return authapi.ReasonError(connect.CodeInvalidArgument,
		fmt.Errorf("this %s %s, so the logs can't carry on from it. Leave it out to start again, or use the one you were last given", field, why), "invalid")
}

// checkTokenSize refuses a token too long to be one, before reading it.
func checkTokenSize(field, token string) error {
	if len(token) > maxTokenBytes {
		return badToken(field, fmt.Sprintf("is %d bytes, and one is at most %d", len(token), maxTokenBytes))
	}
	return nil
}

// checkPlaces checks what a token says of the sources on one server.
func checkPlaces(field string, places map[string][2]int64) error {
	if len(places) > maxPlaces {
		return badToken(field, fmt.Sprintf("names %d sources on a server, and one names at most %d", len(places), maxPlaces))
	}
	for source, c := range places {
		switch {
		case !agentlogs.ValidSourceID(source):
			return badToken(field, "names a source that isn't one")
		case c[0] < 0 || c[1] < 0 || c[1] > math.MaxUint32:
			return badToken(field, "gives a place in a log that can't be one")
		}
	}
	return nil
}

// checkLists refuses what a request names more of than there can be, as each goes
// to every server the service is on.
func checkLists(sources []string, levels []dockwellv1.LogLevel, change string) error {
	invalid := func(format string, args ...any) error {
		return authapi.ReasonError(connect.CodeInvalidArgument, fmt.Errorf(format, args...), "invalid")
	}
	if len(sources) > agentlogs.MaxNamedSources {
		return invalid("this names %d sources, and a read takes at most %d. Leave source_ids out to read them all", len(sources), agentlogs.MaxNamedSources)
	}
	for _, source := range sources {
		if !agentlogs.ValidSourceID(source) {
			return invalid("source_ids names a source that isn't one. Find what there is with dockwell logs sources")
		}
	}
	if len(levels) > maxLevels {
		return invalid("this names %d levels, more than there are. Name each one once", len(levels))
	}
	if len(change) > maxChangeID {
		return invalid("change_id is %d bytes, which no change's is. Find it with dockwell changes list", len(change))
	}
	return nil
}

// note moves a server's place past a line.
func (p *serverPlace) note(source string, t time.Time) {
	nanos := t.UnixNano()
	c := p.Sources[source]
	if c[0] == nanos {
		c[1]++
	} else {
		c = [2]int64{nanos, 1}
	}
	if p.Sources == nil {
		p.Sources = map[string][2]int64{}
	}
	p.Sources[source] = c
	if nanos > p.Last {
		p.Last = nanos
	}
	if len(p.Sources) > maxPlaces {
		oldest, at := "", int64(0)
		for source, c := range p.Sources {
			if oldest == "" || c[0] < at {
				oldest, at = source, c[0]
			}
		}
		delete(p.Sources, oldest)
		p.From = max(p.From, at)
	}
}

func (p *serverPlace) clone() *serverPlace {
	out := &serverPlace{Last: p.Last, From: p.From, Sources: maps.Clone(p.Sources)}
	return out
}

// apply makes a call carry on from a server's place: after its last line, or from
// when the stream began where none came, so lines written meanwhile aren't left to
// the tail's count.
func (p *serverPlace) apply(call *agentv1.LogsCall) {
	if p == nil || p.Last == 0 && p.From == 0 {
		return
	}
	call.After = call.After[:0]
	for _, source := range slices.Sorted(maps.Keys(p.Sources)) {
		c := p.Sources[source]
		call.After = append(call.After, &agentv1.LogCursor{SourceId: source, Time: timestamppb.New(time.Unix(0, c[0])), Count: uint32(c[1])}) //nolint:gosec // a small count
	}
	from := p.From
	if from == 0 {
		from = p.Last
	}
	call.ResumeFrom = timestamppb.New(time.Unix(0, from))
	call.Tail = 0
}

// event is what one server's relay passes to its stream.
type event struct {
	server  server
	message *agentv1.SendLogsRequest
	status  *dockwellv1.ServerLogStatus
	place   *serverPlace
	done    bool
}

// WatchLogs sends a service's newest lines, then follows them.
func (h *Handler) WatchLogs(ctx context.Context, req *connect.Request[dockwellv1.WatchLogsRequest], stream *connect.ServerStream[dockwellv1.WatchLogsResponse]) error {
	m := req.Msg
	t, err := h.resolveAny(ctx, m.GetId(), m.GetProject(), m.GetEnvironment(), m.GetServer())
	if err != nil {
		return err
	}
	if err := checkRange(m.GetSince(), m.GetLast()); err != nil {
		return err
	}
	if err := checkQuery(m.GetQuery()); err != nil {
		return err
	}
	if err := checkLists(m.GetSourceIds(), m.GetLevels(), m.GetChangeId()); err != nil {
		return err
	}
	tail := m.GetTail()
	switch {
	case tail == 0:
		tail = DefaultTail
	case tail < -1 || tail > MaxTail:
		return authapi.ReasonError(connect.CodeInvalidArgument, fmt.Errorf("tail takes -1 for no lines, up to %d", MaxTail), "invalid")
	}
	places, err := readResumeToken(m.GetResumeToken(), t.servers)
	if err != nil {
		return err
	}
	if err := h.record(ctx, t, "follow"); err != nil {
		return err
	}
	began := h.now()
	if !m.GetOnce() {
		defer func() { h.recordEnd(ctx, t, began) }()
	}
	base := &agentv1.LogsCall{
		Resource: resourceRef(t.ref), Since: m.GetSince(), Last: m.GetLast(), Until: m.GetUntil(), Tail: tail, Follow: !m.GetOnce(),
		SourceIds: m.GetSourceIds(), Revision: m.GetRevision(), ChangeId: m.GetChangeId(), ForgottenUpTo: int64(t.forgotten.UpTo),
		Levels: m.GetLevels(), Query: m.GetQuery(), Reader: reader(ctx), Container: t.container, ContainerName: t.containerName(),
		// A range and a read to the end take in what the servers kept, as lines
		// of that time may be there. Following shows what runs now.
		IncludeKept: m.GetOnce() || m.GetSince() != nil || m.GetLast() != nil || m.GetUntil() != nil,
	}
	// A filter that names a deploy reads wherever it ran, and so does what names
	// a time; otherwise a stream reads only where the service runs now.
	everywhere := base.GetIncludeKept() || base.GetRevision() != 0 || base.GetChangeId() != "" || len(base.GetSourceIds()) > 0 || t.deleted

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &watch{h: h, t: t, base: base, ctx: ctx, events: make(chan event), places: places, started: map[string]bool{}, waiting: map[string]bool{}, everywhere: everywhere}
	w.startRelays()
	if w.running == 0 {
		// Nowhere to read from, yet: what a stream that follows waits for, and
		// the end of one that doesn't.
		w.said = true
		if err := stream.Send(&dockwellv1.WatchLogsResponse{CaughtUp: true}); err != nil {
			return err
		}
		if m.GetOnce() {
			return nil
		}
	}

	placement := time.NewTicker(placementEvery)
	defer placement.Stop()
	// So the audit log says who could see what was printed, for as long as a
	// stream stays open.
	audit := time.NewTicker(h.options.AuditEvery)
	defer audit.Stop()
	timer := time.NewTimer(gather)
	timer.Stop()
	timing := false
	pending := &dockwellv1.WatchLogsResponse{}
	size := 0
	flush := func() error {
		if len(pending.GetChunks()) == 0 && len(pending.GetSources()) == 0 && len(pending.GetServers()) == 0 && !pending.GetCaughtUp() {
			return nil
		}
		// A log that has printed nothing yet still has a place, from when the stream
		// began, which caught up says: carrying on from it misses nothing.
		if len(pending.GetChunks()) > 0 || pending.GetCaughtUp() {
			pending.ResumeToken = places.encode()
		}
		err := stream.Send(pending)
		pending, size = &dockwellv1.WatchLogsResponse{}, 0
		return err
	}
	for {
		select {
		case ev := <-w.events:
			if ev.done {
				w.running--
				if w.running == 0 && m.GetOnce() {
					w.caughtUp(pending)
					return flush()
				}
				continue
			}
			if ev.status != nil {
				pending.Servers = append(pending.Servers, ev.status)
				w.statusSeen(ev.status)
			}
			if u := ev.message; u != nil {
				for _, s := range u.GetSources() {
					s.ServerId, s.ServerName = ev.server.id, ev.server.name
				}
				pending.Sources = append(pending.Sources, u.GetSources()...)
				pending.Chunks = append(pending.Chunks, u.GetChunks()...)
				for _, c := range u.GetChunks() {
					for _, l := range c.GetLines() {
						size += len(l.GetText())
					}
				}
				if u.GetCaughtUp() {
					w.serverCaughtUp(ev.server.id)
				}
				places.Servers[ev.server.id] = ev.place
			}
			w.caughtUp(pending)
			if size >= messageBytes {
				if err := flush(); err != nil {
					return err
				}
				continue
			}
			if !timing {
				timer.Reset(gather)
				timing = true
			}
		case <-timer.C:
			timing = false
			if err := flush(); err != nil {
				return err
			}
		case <-placement.C:
			if !m.GetOnce() {
				if err := w.lookAgain(); err != nil {
					return err
				}
			}
		case <-audit.C:
			if err := h.record(ctx, t, "follow"); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// watch is one stream of logs.
type watch struct {
	h          *Handler
	t          *target
	base       *agentv1.LogsCall
	ctx        context.Context
	events     chan event
	places     *resumeToken
	everywhere bool

	started map[string]bool
	running int
	// waiting are the servers yet to send what they had; said is whether the
	// stream has said it is caught up since one last came back.
	waiting map[string]bool
	said    bool
}

// startRelays starts reading from each server not yet read.
func (w *watch) startRelays() {
	for _, s := range w.t.servers {
		if w.started[s.id] || (!s.placed && !w.everywhere) {
			continue
		}
		w.started[s.id] = true
		w.running++
		w.waiting[s.id] = true
		w.said = false
		place := w.places.Servers[s.id]
		if place == nil {
			place = &serverPlace{}
		}
		go w.relay(s, place.clone())
	}
}

func (w *watch) serverCaughtUp(server string) {
	delete(w.waiting, server)
}

// statusSeen notes a server's state, as one that can't be read isn't waited for,
// and one that comes back is.
func (w *watch) statusSeen(s *dockwellv1.ServerLogStatus) {
	if s.GetState() == dockwellv1.ServerLogState_SERVER_LOG_STATE_READING {
		w.waiting[s.GetServerId()] = true
		w.said = false
		return
	}
	delete(w.waiting, s.GetServerId())
}

// caughtUp says so on a message, once nothing is left to wait for.
func (w *watch) caughtUp(m *dockwellv1.WatchLogsResponse) {
	if !w.said && len(w.waiting) == 0 {
		w.said = true
		m.CaughtUp = true
	}
}

// lookAgain finds servers the service moved to since the stream started, and ends
// the stream if a deletion has put what it reads out of reach since then. A service
// made again since is read afresh, for what the new one wrote alone. A container
// goes nowhere: its agent says if it goes.
func (w *watch) lookAgain() error {
	if w.t.container != "" {
		return nil
	}
	t := *w.t
	err := w.h.db.View(w.ctx, func(tx *store.Tx) error {
		res, err := tx.GetResourceByID(w.ctx, w.t.id)
		if err != nil {
			return err
		}
		if t.forgotten, err = revision.ForgottenOf(w.ctx, tx, res); err != nil {
			return err
		}
		return w.h.servers(w.ctx, tx, &t, res)
	})
	if err != nil {
		// It looks again in a moment, and nothing has changed for a reader until then.
		if w.ctx.Err() == nil {
			w.h.logger.WarnContext(w.ctx, "can't look where a service's logs are; it looks again shortly", slog.String("error", err.Error()))
		}
		return nil
	}
	if t.forgotten.Deleted {
		return deletedError(w.t.ref.Name)
	}
	if t.forgotten.UpTo != w.t.forgotten.UpTo {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s was deleted and made again, so this stream ended. Read it again for what the new one wrote", w.t.ref.Name))
	}
	w.t.servers = t.servers
	w.startRelays()
	return nil
}

// relay reads one server's logs until there are no more to read, carrying on
// from where it was whenever its agent comes back: a stream that drops ends the
// call on the agent, and the next one asks for what comes after the last line
// it passed on.
func (w *watch) relay(s server, place *serverPlace) {
	send := func(ev event) bool {
		ev.server = s
		select {
		case w.events <- ev:
			return true
		case <-w.ctx.Done():
			return false
		}
	}
	defer send(event{done: true})
	reported := dockwellv1.ServerLogState_SERVER_LOG_STATE_UNSPECIFIED
	report := func(status *dockwellv1.ServerLogStatus) bool {
		if status.GetState() == reported {
			return true
		}
		reported = status.GetState()
		return send(event{status: status})
	}
	for {
		session, ok := w.h.agents.SessionPastMove(w.ctx, s.id, MoveGrace)
		if !ok {
			if !w.base.GetFollow() {
				report(offline(s))
				return
			}
			if !report(offline(s)) {
				return
			}
			var err error
			if session, err = w.h.agents.WaitSession(w.ctx, s.id); err != nil {
				return
			}
		}
		if !report(&dockwellv1.ServerLogStatus{ServerId: s.id, ServerName: s.name, State: dockwellv1.ServerLogState_SERVER_LOG_STATE_READING}) {
			return
		}
		call := cloneCall(w.base)
		place.apply(call)
		err := session.Logs(w.ctx, call, func(u *agentv1.SendLogsRequest) error {
			if t := u.GetStarted(); t != nil && place.From == 0 {
				place.From = t.AsTime().UnixNano()
			}
			for _, c := range u.GetChunks() {
				for _, l := range c.GetLines() {
					place.note(c.GetSourceId(), l.GetTime().AsTime())
				}
			}
			if !send(event{message: u, place: place.clone()}) {
				return w.ctx.Err()
			}
			return nil
		})
		switch {
		case w.ctx.Err() != nil:
			return
		case err == nil:
			// A read to the end is done; a follow ends only with its reader, or
			// as its agent moves to a new stream.
			if !w.base.GetFollow() {
				report(&dockwellv1.ServerLogStatus{ServerId: s.id, ServerName: s.name, State: dockwellv1.ServerLogState_SERVER_LOG_STATE_DONE})
				return
			}
		case errors.Is(err, agentapi.ErrMoving):
			// It carries on from its place over the next stream, a read to the end
			// as a follow does, and SessionPastMove says if that doesn't come.
		case errors.Is(err, resource.ErrLost), errors.Is(err, resource.ErrNotConnected):
			if !w.base.GetFollow() {
				report(offline(s))
				return
			}
			if _, connected := w.h.agents.Session(s.id); !connected {
				report(offline(s))
			}
		default:
			if !report(unavailable(s, err)) || !w.base.GetFollow() {
				return
			}
		}
		// A moment before trying again, whatever the reason, so a server that
		// keeps refusing isn't asked in a tight loop.
		wait := time.NewTimer(retryDelay(err))
		select {
		case <-wait.C:
		case <-w.ctx.Done():
			wait.Stop()
			return
		}
	}
}

// retryDelay is how soon to read a server's logs again after err. An agent that
// moved to a new stream is there at once; one that can't read logs needs longer.
func retryDelay(err error) time.Duration {
	var se *resource.ServerError
	if errors.As(err, &se) {
		return retryEvery
	}
	return 100 * time.Millisecond
}

func cloneCall(c *agentv1.LogsCall) *agentv1.LogsCall {
	return &agentv1.LogsCall{
		Resource: c.GetResource(), Since: c.GetSince(), Until: c.GetUntil(), Tail: c.GetTail(), Follow: c.GetFollow(),
		SourceIds: c.GetSourceIds(), Revision: c.GetRevision(), ChangeId: c.GetChangeId(), Levels: c.GetLevels(),
		Query: c.GetQuery(), IncludeKept: c.GetIncludeKept(), Reader: c.GetReader(), Last: c.GetLast(),
		ForgottenUpTo: c.GetForgottenUpTo(), Container: c.GetContainer(), ContainerName: c.GetContainerName(),
	}
}
