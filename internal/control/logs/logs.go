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
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
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
	// pageBytes is the most of log lines a page of ReadLogs holds, newest kept.
	pageBytes = 8 << 20
	// placementEvery is how often a stream looks for servers the service moved
	// to, while it follows.
	placementEvery = 5 * time.Second
	// retryEvery is how soon a stream tries a server again whose agent couldn't
	// read logs, as when Docker wasn't running.
	retryEvery = 5 * time.Second
	// auditEvery is how often one reader's reading of one service's logs goes
	// in the audit log, at most.
	auditEvery = time.Hour
	// auditEntries bounds what is remembered of who read what.
	auditEntries = 10_000
	// callWait bounds a server's answer to which sources it has.
	callWait = 15 * time.Second
)

// Handler implements LogService.
type Handler struct {
	agents  *agentapi.Agents
	changes *changes.Handler
	db      *store.DB
	logger  *slog.Logger
	now     func() time.Time

	auditMu sync.Mutex
	audited map[string]time.Time
}

var _ dockwellv1connect.LogServiceHandler = (*Handler)(nil)

// New returns a handler that reads logs through agents.
func New(agents *agentapi.Agents, ch *changes.Handler, db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{agents: agents, changes: ch, db: db, logger: logger, now: time.Now, audited: map[string]time.Time{}}
}

// server is one server a service is on, or was on.
type server struct {
	id, name string
	// placed says the service runs there now, as opposed to having run there,
	// which leaves only what the server kept.
	placed bool
}

// target is a service asked for by a request.
type target struct {
	id          string
	ref         resource.Ref
	environment string
	deleted     bool
	servers     []server
}

func (t *target) proto() *dockwellv1.LoggedService {
	return &dockwellv1.LoggedService{Id: t.id, Name: t.ref.Name, Environment: t.environment, Deleted: t.deleted}
}

// resolve finds the service a request names. A deleted service can be asked for
// by ID, as what its servers kept can still be read, such as when its first
// deploy failed and was rolled back.
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
	}
	return t, nil
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
	for _, d := range deps {
		if seen[d.Server] {
			continue
		}
		seen[d.Server] = true
		s, err := tx.GetServer(ctx, d.Server)
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
	due := !seen || now.Sub(last) >= auditEvery
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
			ID: id.New(id.Event), At: now, Actor: actor, Action: "logs.read", Subject: t.id, Target: t.id,
			Detail: what + " " + t.ref.Name, Surface: client.Surface(p),
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
	t, err := h.resolve(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
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
	session, ok := h.agents.Session(s.id)
	if !ok {
		return nil, offline(s)
	}
	ctx, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	sources, err := session.LogSources(ctx, resourceRef(t.ref))
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

// sinceOf is the start of a request's time range.
func (h *Handler) sinceOf(since *timestamppb.Timestamp, last *durationpb.Duration) (*timestamppb.Timestamp, error) {
	if last == nil {
		return since, nil
	}
	if since != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New("say since or last, not both"), "invalid")
	}
	if err := last.CheckValid(); err != nil || last.AsDuration() <= 0 {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New("last needs a length of time longer than zero, such as 15m"), "invalid")
	}
	return timestamppb.New(h.now().Add(-last.AsDuration())), nil
}

// ReadLogs returns a page of a service's log lines.
func (h *Handler) ReadLogs(ctx context.Context, req *connect.Request[dockwellv1.ReadLogsRequest]) (*connect.Response[dockwellv1.ReadLogsResponse], error) {
	m := req.Msg
	t, err := h.resolve(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	size, err := paging.Size(m.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	since, err := h.sinceOf(m.GetSince(), m.GetLast())
	if err != nil {
		return nil, err
	}
	before, err := readPageToken(m.GetPageToken())
	if err != nil {
		return nil, err
	}
	if err := h.record(ctx, t, "read"); err != nil {
		return nil, err
	}
	call := func() *agentv1.LogsCall {
		return &agentv1.LogsCall{
			Resource: resourceRef(t.ref), Since: since, Until: m.GetUntil(), Tail: int32(size), //nolint:gosec // bounded above
			SourceIds: m.GetSourceIds(), Revision: m.GetRevision(), ChangeId: m.GetChangeId(),
			Levels: m.GetLevels(), Query: m.GetQuery(), IncludeKept: true, Before: before.cursors(),
		}
	}
	// A next page reads only the sources with more to read.
	if m.GetPageToken() != "" {
		only := before.sourceIDs()
		if len(m.GetSourceIds()) > 0 {
			only = slices.DeleteFunc(only, func(source string) bool { return !slices.Contains(m.GetSourceIds(), source) })
		}
		base := call
		call = func() *agentv1.LogsCall {
			c := base()
			c.SourceIds = only
			return c
		}
	}
	page := newPage(size)
	var mu sync.Mutex
	var wg sync.WaitGroup
	res := &dockwellv1.ReadLogsResponse{}
	for _, s := range t.servers {
		wg.Go(func() {
			session, ok := h.agents.Session(s.id)
			if !ok {
				mu.Lock()
				res.Servers = append(res.Servers, offline(s))
				mu.Unlock()
				return
			}
			err := session.Logs(ctx, call(), func(u *agentv1.SendLogsRequest) error {
				mu.Lock()
				defer mu.Unlock()
				for _, src := range u.GetSources() {
					src.ServerId, src.ServerName = s.id, s.name
				}
				page.add(u)
				return nil
			})
			if err != nil && ctx.Err() == nil {
				status := unavailable(s, err)
				if errors.Is(err, resource.ErrLost) || errors.Is(err, resource.ErrNotConnected) {
					status = offline(s)
				}
				mu.Lock()
				res.Servers = append(res.Servers, status)
				mu.Unlock()
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
	size    int
	lines   map[string][]*dockwellv1.LogLine
	sources map[string]*dockwellv1.LogSource
	scans   []*agentv1.LogScan
	bytes   int
	// cut says a source has older lines than those kept: some were dropped
	// here, or its agent didn't send them.
	cut map[string]bool
}

func newPage(size int) *page {
	return &page{size: size, lines: map[string][]*dockwellv1.LogLine{}, sources: map[string]*dockwellv1.LogSource{}, cut: map[string]bool{}}
}

func (p *page) add(u *agentv1.SendLogsRequest) {
	for _, s := range u.GetSources() {
		p.sources[s.GetId()] = s
	}
	p.scans = append(p.scans, u.GetScans()...)
	for _, s := range u.GetScans() {
		if s.GetOlder() {
			p.cut[s.GetSourceId()] = true
		}
	}
	for _, c := range u.GetChunks() {
		p.lines[c.GetSourceId()] = append(p.lines[c.GetSourceId()], c.GetLines()...)
		for _, l := range c.GetLines() {
			p.bytes += len(l.GetText())
		}
	}
	if p.bytes > 2*pageBytes {
		p.trim(pageBytes)
	}
}

// trim drops the oldest lines, of whichever source, until what is left fits.
func (p *page) trim(limit int) {
	type at struct {
		source string
		line   *dockwellv1.LogLine
	}
	var all []at
	for source, lines := range p.lines {
		for _, l := range lines {
			all = append(all, at{source, l})
		}
	}
	slices.SortStableFunc(all, func(a, b at) int { return b.line.GetTime().AsTime().Compare(a.line.GetTime().AsTime()) })
	kept := map[string][]*dockwellv1.LogLine{}
	bytes := 0
	n := 0
	for ; n < len(all) && n < p.size && bytes < limit; n++ {
		kept[all[n].source] = append(kept[all[n].source], all[n].line)
		bytes += len(all[n].line.GetText())
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
			next[source] = kept[0].GetTime().AsTime().UnixNano()
		case !oldest.IsZero():
			// All of it was older than what the page kept.
			next[source] = oldest.UnixNano()
		}
	}
	res.NextPageToken = next.encode()
	for _, source := range slices.Sorted(maps.Keys(p.lines)) {
		res.Chunks = append(res.Chunks, &dockwellv1.LogChunk{SourceId: source, Lines: p.lines[source]})
	}
	res.Sources = slices.Collect(maps.Values(p.sources))
	slices.SortFunc(res.Sources, func(a, b *dockwellv1.LogSource) int {
		return b.GetCreateTime().AsTime().Compare(a.GetCreateTime().AsTime())
	})
	if !searching {
		return
	}
	for _, s := range p.scans {
		res.SearchedLines += s.GetLines()
		res.SearchCutShort = res.SearchCutShort || s.GetCutShort()
		if t := s.GetOldestTime(); t != nil && (res.SearchedBackTo == nil || t.AsTime().Before(res.SearchedBackTo.AsTime())) {
			res.SearchedBackTo = t
		}
	}
}

// pageToken is where the next page starts: for each source, the time of the
// oldest line the page before held, in nanoseconds.
type pageToken map[string]int64

func (t pageToken) encode() string {
	if len(t) == 0 {
		return ""
	}
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

func readPageToken(s string) (pageToken, error) {
	if s == "" {
		return nil, nil
	}
	t := pageToken{}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &t)
	}
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New("this page_token isn't one a page of logs gave. Leave it out to start again"), "invalid")
	}
	return t, nil
}

func (t pageToken) cursors() []*agentv1.LogCursor {
	var out []*agentv1.LogCursor
	for _, source := range slices.Sorted(maps.Keys(t)) {
		out = append(out, &agentv1.LogCursor{SourceId: source, Time: timestamppb.New(time.Unix(0, t[source]))})
	}
	return out
}

func (t pageToken) sourceIDs() []string { return slices.Sorted(maps.Keys(t)) }

// resumeToken is where a stream carries on: for each server, where each source
// has got to, and the time of the last line, by that server's clock.
type resumeToken struct {
	Servers map[string]*serverPlace `json:"s"`
}

type serverPlace struct {
	// Last is the time of the last line from the server, in nanoseconds.
	Last int64 `json:"l"`
	// Sources are where each source has got to, as a time and how many lines
	// have just that time.
	Sources map[string][2]int64 `json:"c"`
}

func (t *resumeToken) encode() string {
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}

func readResumeToken(s string) (*resumeToken, error) {
	t := &resumeToken{Servers: map[string]*serverPlace{}}
	if s == "" {
		return t, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, t)
	}
	if err != nil || t.Servers == nil {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New("this resume_token isn't one a stream of logs gave. Leave it out to start again"), "invalid")
	}
	return t, nil
}

// maxPlaces bounds how many sources a token names on one server, the newest
// kept. A source it forgets, which has long gone quiet, is read again by time.
const maxPlaces = 64

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
	}
}

func (p *serverPlace) clone() *serverPlace {
	out := &serverPlace{Last: p.Last, Sources: maps.Clone(p.Sources)}
	return out
}

// apply makes a call carry on from a server's place.
func (p *serverPlace) apply(call *agentv1.LogsCall) {
	if p == nil || p.Last == 0 {
		return
	}
	call.After = call.After[:0]
	for _, source := range slices.Sorted(maps.Keys(p.Sources)) {
		c := p.Sources[source]
		call.After = append(call.After, &agentv1.LogCursor{SourceId: source, Time: timestamppb.New(time.Unix(0, c[0])), Count: uint32(c[1])}) //nolint:gosec // a small count
	}
	call.ResumeFrom = timestamppb.New(time.Unix(0, p.Last))
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
	t, err := h.resolve(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return err
	}
	since, err := h.sinceOf(m.GetSince(), m.GetLast())
	if err != nil {
		return err
	}
	tail := m.GetTail()
	switch {
	case tail == 0:
		tail = DefaultTail
	case tail < -1 || tail > MaxTail:
		return authapi.ReasonError(connect.CodeInvalidArgument, fmt.Errorf("tail takes -1 for no lines, up to %d", MaxTail), "invalid")
	}
	places, err := readResumeToken(m.GetResumeToken())
	if err != nil {
		return err
	}
	if err := h.record(ctx, t, "follow"); err != nil {
		return err
	}
	base := &agentv1.LogsCall{
		Resource: resourceRef(t.ref), Since: since, Until: m.GetUntil(), Tail: tail, Follow: !m.GetOnce(),
		SourceIds: m.GetSourceIds(), Revision: m.GetRevision(), ChangeId: m.GetChangeId(),
		Levels: m.GetLevels(), Query: m.GetQuery(),
		// A range and a read to the end take in what the servers kept, as lines
		// of that time may be there. Following shows what runs now.
		IncludeKept: m.GetOnce() || since != nil || m.GetUntil() != nil,
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
	timer := time.NewTimer(gather)
	timer.Stop()
	timing := false
	pending := &dockwellv1.WatchLogsResponse{}
	size := 0
	flush := func() error {
		if len(pending.GetChunks()) == 0 && len(pending.GetSources()) == 0 && len(pending.GetServers()) == 0 && !pending.GetCaughtUp() {
			return nil
		}
		if len(pending.GetChunks()) > 0 {
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
				w.lookAgain()
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

// lookAgain finds servers the service moved to since the stream started.
func (w *watch) lookAgain() {
	t := *w.t
	err := w.h.db.View(w.ctx, func(tx *store.Tx) error {
		res, err := tx.GetResourceByID(w.ctx, w.t.id)
		if err != nil {
			return err
		}
		return w.h.servers(w.ctx, tx, &t, res)
	})
	if err != nil {
		return
	}
	w.t.servers = t.servers
	w.startRelays()
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
		session, ok := w.h.agents.Session(s.id)
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
		Query: c.GetQuery(), IncludeKept: c.GetIncludeKept(),
	}
}
