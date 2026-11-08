// SPDX-License-Identifier: AGPL-3.0-only

// Package builds keeps what servers say of the builds of services' images, and
// serves BuildService: a build's state, its log as kept, a follow of it, and
// cancelling one. A build is a step of a change, so this records an event, as
// backups do, and never reaches the revision store. See
// docs/adr/0046-builds-from-git.md.
package builds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// Bounds on what a server's word may put on record.
const (
	maxLine      = 4 << 10
	maxShort     = 500
	maxLines     = 2000 // in one update
	pageSize     = 20
	maxPageSize  = 100
	lostRecheck  = 5 * time.Second
	eventCancel  = "build.cancel"
	sendLinesMax = 2000 // lines in one message of a follow
)

// Finder finds a service by ID or by name, as ServiceService does, and reads its
// build settings and where it runs.
type Finder interface {
	Find(ctx context.Context, named, project, environment string) (store.Resource, error)
	BuildOf(ctx context.Context, named, project, environment string) (*resourcev1.ImageBuild, string, []string, error)
}

// Handler implements BuildService, and keeps what agents say of builds.
type Handler struct {
	db       *store.DB
	agents   *agentapi.Agents
	services Finder
	logger   *slog.Logger
	now      func() time.Time
	sources  revision.Sources

	mu       sync.Mutex
	watchers map[string]map[chan []store.BuildLine]struct{}
	detected map[string]detected
}

var _ dockwellv1connect.BuildServiceHandler = (*Handler)(nil)

// New returns a handler that hears of builds from agents, and finds services with
// services.
func New(db *store.DB, agents *agentapi.Agents, services Finder, logger *slog.Logger) *Handler {
	h := &Handler{
		db: db, agents: agents, services: services, logger: logger, now: time.Now,
		watchers: map[string]map[chan []store.BuildLine]struct{}{}, detected: map[string]detected{},
	}
	agents.OnBuild(h.record)
	return h
}

// record keeps what a server says of a build. An agent speaks for its own server
// and nothing more, so a build it tells of has to be a step the change planned on
// that server, and what it says is made fit to keep first.
func (h *Handler) record(server string, u *agentv1.BuildUpdate) {
	ctx := context.Background()
	ref := resourcepb.FromRef(u.GetResource())
	if !strings.HasPrefix(u.GetChangeId(), id.Change+"_") || ref.Kind != "service" {
		return
	}
	var b store.Build
	var added []store.BuildLine
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name)
		if err != nil {
			return err
		}
		b, err = tx.BuildOf(ctx, u.GetChangeId(), r.ID, server)
		if errors.Is(err, store.ErrNotFound) {
			if b, err = h.newBuild(ctx, tx, server, u.GetChangeId(), r, ref); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if b.Ended() {
			// What ended stays as it ended: a step resumed builds as a build of its
			// own only once its first has.
			return nil
		}
		merge(&b, u)
		if err := tx.UpdateBuild(ctx, b); err != nil {
			return err
		}
		added = lines(u.GetLines())
		if len(added) > 0 {
			if b, err = tx.AddBuildLines(ctx, b.ID, added); err != nil {
				return err
			}
		}
		if b.Ended() {
			return tx.ForgetOldBuildLogs(ctx, b.ResourceID, store.KeepLogs)
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, errNotPlanned) && !errors.Is(err, store.ErrNotFound) {
			h.logger.Warn("can't keep what a server says of a build", slog.String("server", server), slog.String("change", u.GetChangeId()), slog.String("error", err.Error()))
		}
		return
	}
	h.publish(b.ID, added)
}

var errNotPlanned = errors.New("the change has no such build")

// newBuild records a build a server begins to tell of, once the change's plan says
// it builds there.
func (h *Handler) newBuild(ctx context.Context, tx *store.Tx, server, changeID string, r store.Resource, ref resource.Ref) (store.Build, error) {
	c, err := tx.GetChange(ctx, changeID)
	if err != nil {
		return store.Build{}, err
	}
	if c.State != store.ChangeApplying {
		return store.Build{}, errNotPlanned
	}
	var plan revision.Plan
	if err := json.Unmarshal(c.Plan, &plan); err != nil {
		return store.Build{}, fmt.Errorf("can't read change %s's plan: %w", changeID, err)
	}
	var built *resource.BuildPlan
	stepped := false
	for _, sp := range plan.Servers {
		if sp.Server != server {
			continue
		}
		for i := range sp.Steps {
			if sp.Steps[i].Ref == ref {
				stepped = true
				if sp.Steps[i].Build != nil {
					built = sp.Steps[i].Build
				}
			}
		}
	}
	// An agent speaks for its own server's steps, and a change has none there for a
	// service it doesn't change on it.
	if !stepped {
		return store.Build{}, errNotPlanned
	}
	if built == nil {
		// A rollback's plan, which runs before anyone reads it, or a resume's, can
		// build what its own plan didn't say, as the server lost the image: it is
		// the same spec, so take it from what the server says.
		built = &resource.BuildPlan{}
	}
	b := store.Build{
		ID: id.New(id.Build), ChangeID: changeID, ResourceID: r.ID, EnvironmentID: r.Scope, Service: r.Name, ServerID: server,
		State: store.BuildQueued, URL: built.URL, Ref: built.Ref, Commit: built.Commit, Image: built.Image, CreatedAt: h.now(),
	}
	if r.ChangeID == changeID {
		b.Revision = int64(r.Revision)
	}
	if err := tx.CreateBuild(ctx, b); err != nil {
		return store.Build{}, err
	}
	return b, nil
}

// merge takes what an update says of a build into what is kept of it.
func merge(b *store.Build, u *agentv1.BuildUpdate) {
	if s, ok := states[u.GetState()]; ok {
		b.State = s
	}
	if v := short(u.GetCommitSubject()); v != "" {
		b.Subject = v
	}
	if v := short(u.GetCommitAuthor()); v != "" {
		b.Author = v
	}
	if v := short(u.GetImage()); v != "" && b.Image == "" {
		b.Image = v
	}
	if u.GetSteps() > 0 {
		b.Steps, b.Cached = int64(u.GetSteps()), int64(min(u.GetCachedSteps(), u.GetSteps()))
	}
	if u.GetStep() > 0 {
		b.Step, b.StepName = int64(u.GetStep()), short(u.GetStepName())
	}
	if v := u.GetError(); v != "" {
		b.Error = fit(v, maxLine)
	}
	if v := u.GetFailedLine(); v != "" {
		b.FailedLine = fit(v, maxLine)
	}
	if d := fitDetection(resourcepb.DetectionFrom(u.GetDetection())); d != nil && b.Detection == "" {
		if j, err := json.Marshal(d); err == nil {
			b.Detection = string(j)
		}
	}
	b.Dropped += max(u.GetDropped(), 0)
	if t := u.GetStartTime(); t != nil && b.StartedAt == nil {
		at := t.AsTime()
		b.StartedAt = &at
	}
	if t := u.GetEndTime(); t != nil {
		at := t.AsTime()
		b.EndedAt = &at
	}
}

var states = map[dockwellv1.BuildState]string{
	dockwellv1.BuildState_BUILD_STATE_QUEUED:    store.BuildQueued,
	dockwellv1.BuildState_BUILD_STATE_BUILDING:  store.BuildBuilding,
	dockwellv1.BuildState_BUILD_STATE_DONE:      store.BuildDone,
	dockwellv1.BuildState_BUILD_STATE_FAILED:    store.BuildFailed,
	dockwellv1.BuildState_BUILD_STATE_CANCELLED: store.BuildCancelled,
}

var stateProtos = map[string]dockwellv1.BuildState{
	store.BuildQueued:    dockwellv1.BuildState_BUILD_STATE_QUEUED,
	store.BuildBuilding:  dockwellv1.BuildState_BUILD_STATE_BUILDING,
	store.BuildDone:      dockwellv1.BuildState_BUILD_STATE_DONE,
	store.BuildFailed:    dockwellv1.BuildState_BUILD_STATE_FAILED,
	store.BuildCancelled: dockwellv1.BuildState_BUILD_STATE_CANCELLED,
	store.BuildLost:      dockwellv1.BuildState_BUILD_STATE_LOST,
}

// lines are an update's lines, fit to keep, at most maxLines of them.
func lines(in []*dockwellv1.BuildLine) []store.BuildLine {
	if len(in) > maxLines {
		in = in[:maxLines]
	}
	out := make([]store.BuildLine, 0, len(in))
	for _, l := range in {
		at := time.Now().UTC()
		if l.GetTime() != nil {
			at = l.GetTime().AsTime()
		}
		out = append(out, store.BuildLine{Step: int64(max(l.GetStep(), 0)), At: at, Text: fit(l.GetText(), maxLine)})
	}
	return out
}

// short is a short field a server says, fit to keep.
func short(s string) string { return fit(s, maxShort) }

// fit makes what a server says fit to keep and show: valid UTF-8, with no control
// characters but tabs, cut to n bytes.
func fit(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// publish tells whoever follows a build that it moved on, with the lines it gained.
func (h *Handler) publish(buildID string, added []store.BuildLine) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.watchers[buildID] {
		select {
		case ch <- added:
		default:
			// A follower that's behind reads what's kept when it next can.
			select {
			case ch <- nil:
			default:
			}
		}
	}
}

func (h *Handler) watch(buildID string) (<-chan []store.BuildLine, func()) {
	ch := make(chan []store.BuildLine, 16)
	h.mu.Lock()
	if h.watchers[buildID] == nil {
		h.watchers[buildID] = map[chan []store.BuildLine]struct{}{}
	}
	h.watchers[buildID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.watchers[buildID], ch)
		if len(h.watchers[buildID]) == 0 {
			delete(h.watchers, buildID)
		}
		h.mu.Unlock()
	}
}

// Of are the builds of a service for each of changes, by change: for
// ServiceRevision.build. A change with a build on more than one server takes the
// newest.
func (h *Handler) Of(ctx context.Context, serviceID string, changes []string) (map[string]*dockwellv1.Build, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	out := map[string]*dockwellv1.Build{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		builds, err := tx.ListBuilds(ctx, store.BuildFilter{ResourceID: serviceID, ChangeIDs: changes})
		if err != nil {
			return err
		}
		protos, err := h.protos(ctx, tx, builds)
		if err != nil {
			return err
		}
		for i := range builds {
			if out[builds[i].ChangeID] == nil {
				out[builds[i].ChangeID] = protos[i]
			}
		}
		return nil
	})
	return out, err
}

// protos are builds as the API says them. A build that waits or runs under a change
// that has ended was lost with its server.
func (h *Handler) protos(ctx context.Context, tx *store.Tx, builds []store.Build) ([]*dockwellv1.Build, error) {
	var ids []string
	for i := range builds {
		if !builds[i].Ended() {
			ids = append(ids, builds[i].ChangeID)
		}
	}
	changes, err := tx.GetChanges(ctx, ids)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	out := make([]*dockwellv1.Build, len(builds))
	for i := range builds {
		b := builds[i]
		if !b.Ended() {
			if c, ok := changes[b.ChangeID]; ok && c.State != store.ChangeApplying {
				b.State = store.BuildLost
			}
		}
		name, ok := names[b.ServerID]
		if !ok {
			if srv, err := tx.GetServer(ctx, b.ServerID); err == nil {
				name = srv.Name
			}
			names[b.ServerID] = name
		}
		out[i] = toProto(b, name)
	}
	// What a failing step printed last is log content, for whoever may read logs
	// (ADR 0025).
	if p, ok := authapi.PrincipalFrom(ctx); ok && !p.ReadsLogs() {
		for _, b := range out {
			b.FailedLine = ""
		}
	}
	return out, nil
}

func toProto(b store.Build, server string) *dockwellv1.Build {
	p := &dockwellv1.Build{
		Id: b.ID, ServiceId: b.ResourceID, Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: b.EnvironmentID, Name: b.Service},
		ChangeId: b.ChangeID, Revision: b.Revision, ServerId: b.ServerID, ServerName: server, State: stateProtos[b.State],
		Url: b.URL, Ref: b.Ref, Commit: b.Commit, CommitSubject: b.Subject, CommitAuthor: b.Author, Image: b.Image,
		CreateTime: timestamppb.New(b.CreatedAt), Steps: int32(b.Steps), CachedSteps: int32(b.Cached), Step: int32(b.Step), StepName: b.StepName, //nolint:gosec // a build's steps are few
		Error: b.Error, FailedLine: b.FailedLine, CancelledBy: b.CancelledBy, LogLines: b.Lines, LogLinesLeftOut: b.LeftOut, LogLinesDropped: b.Dropped,
	}
	if b.Detection != "" {
		var d resource.BuildDetection
		if json.Unmarshal([]byte(b.Detection), &d) == nil {
			p.Detection = resourcepb.Detection(&d)
		}
	}
	if b.StartedAt != nil {
		p.StartTime = timestamppb.New(*b.StartedAt)
	}
	if b.EndedAt != nil {
		p.EndTime = timestamppb.New(*b.EndedAt)
	}
	return p
}

func lineProtos(in []store.BuildLine) []*dockwellv1.BuildLine {
	out := make([]*dockwellv1.BuildLine, len(in))
	for i, l := range in {
		out[i] = &dockwellv1.BuildLine{Number: l.Number, Time: timestamppb.New(l.At), Step: int32(l.Step), Text: l.Text} //nolint:gosec // as above
	}
	return out
}

// ListBuilds returns a service's builds, newest first.
func (h *Handler) ListBuilds(ctx context.Context, req *connect.Request[dockwellv1.ListBuildsRequest]) (*connect.Response[dockwellv1.ListBuildsResponse], error) {
	m := req.Msg
	size, err := paging.Size(m.GetPageSize(), pageSize, maxPageSize)
	if err != nil {
		return nil, err
	}
	after, _, err := paging.Read(m.GetPageToken(), "ListBuilds")
	if err != nil {
		return nil, err
	}
	r, err := h.services.Find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	res := &dockwellv1.ListBuildsResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		f := store.BuildFilter{ResourceID: r.ID, Limit: size + 1}
		if after.ID != "" {
			f.BeforeID, f.BeforeAt = after.ID, after.At
		}
		builds, err := tx.ListBuilds(ctx, f)
		if err != nil {
			return err
		}
		if len(builds) > size {
			builds = builds[:size]
			last := builds[size-1]
			res.NextPageToken = paging.Token(paging.Cursor{ID: last.ID, At: last.CreatedAt})
		}
		res.Builds, err = h.protos(ctx, tx, builds)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "listing builds", err)
	}
	return connect.NewResponse(res), nil
}

// get reads one build, as the API says it.
func (h *Handler) get(ctx context.Context, buildID string) (store.Build, *dockwellv1.Build, error) {
	if !strings.HasPrefix(buildID, id.Build+"_") {
		return store.Build{}, nil, authapi.Invalid(fmt.Errorf("%q isn't a build's ID, such as bld_01JD6Z3R8M9QF2K7T4XW5BNC0V. List a service's builds with ListBuilds", buildID))
	}
	var b store.Build
	var p *dockwellv1.Build
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if b, err = tx.GetBuild(ctx, buildID); err != nil {
			return err
		}
		ps, err := h.protos(ctx, tx, []store.Build{b})
		if err != nil {
			return err
		}
		p = ps[0]
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return b, nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no build %s. List a service's builds with ListBuilds", buildID), "not_found")
	}
	if err != nil {
		return b, nil, h.internal(ctx, "reading a build", err)
	}
	return b, p, nil
}

// GetBuild returns one build.
func (h *Handler) GetBuild(ctx context.Context, req *connect.Request[dockwellv1.GetBuildRequest]) (*connect.Response[dockwellv1.GetBuildResponse], error) {
	_, p, err := h.get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.GetBuildResponse{Build: p}), nil
}

// ReadBuildLog returns what a build's log keeps.
func (h *Handler) ReadBuildLog(ctx context.Context, req *connect.Request[dockwellv1.ReadBuildLogRequest]) (*connect.Response[dockwellv1.ReadBuildLogResponse], error) {
	b, _, err := h.get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	var kept []store.BuildLine
	err = h.db.View(ctx, func(tx *store.Tx) error {
		kept, err = tx.BuildLines(ctx, b.ID, 0, store.KeepFirst+store.KeepLast)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "reading a build's log", err)
	}
	return connect.NewResponse(&dockwellv1.ReadBuildLogResponse{Lines: lineProtos(kept), LeftOut: b.LeftOut, Dropped: b.Dropped}), nil
}

// WatchBuild sends a build and its log as kept, then what it gains, until it ends.
// A message with lines carries the build as it stood when they were read; one that
// says the build ended carries no lines, and is the last.
func (h *Handler) WatchBuild(ctx context.Context, req *connect.Request[dockwellv1.WatchBuildRequest], stream *connect.ServerStream[dockwellv1.WatchBuildResponse]) error {
	b, _, err := h.get(ctx, req.Msg.GetId())
	if err != nil {
		return err
	}
	updates, stop := h.watch(b.ID)
	defer stop()
	var sent int64
	var last *dockwellv1.Build
	recheck := time.NewTicker(lostRecheck)
	defer recheck.Stop()
	for {
		// What's kept past what was sent, read from the store, so a follower that
		// fell behind, or came late, misses nothing that's kept.
		var p *dockwellv1.Build
		var kept []store.BuildLine
		err := h.db.View(ctx, func(tx *store.Tx) error {
			var err error
			if b, err = tx.GetBuild(ctx, b.ID); err != nil {
				return err
			}
			ps, err := h.protos(ctx, tx, []store.Build{b})
			if err != nil {
				return err
			}
			p = ps[0]
			kept, err = tx.BuildLines(ctx, b.ID, sent, store.KeepFirst+store.KeepLast)
			return err
		})
		if err != nil {
			return h.internal(ctx, "following a build", err)
		}
		ended := p.GetState() != dockwellv1.BuildState_BUILD_STATE_QUEUED && p.GetState() != dockwellv1.BuildState_BUILD_STATE_BUILDING
		shown := p
		if ended {
			// The lines go out as the build stood before it ended, and the end once.
			shown = proto.CloneOf(p)
			shown.State = dockwellv1.BuildState_BUILD_STATE_BUILDING
		}
		moved := last == nil || !proto.Equal(last, shown)
		for len(kept) > 0 || moved && !ended {
			n := min(len(kept), sendLinesMax)
			msg := &dockwellv1.WatchBuildResponse{Build: shown, Lines: lineProtos(kept[:n])}
			if last == nil {
				msg.LeftOut = b.LeftOut
			}
			if n > 0 {
				sent = kept[n-1].Number
			}
			kept, moved, last = kept[n:], false, shown
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
		if ended {
			return stream.Send(&dockwellv1.WatchBuildResponse{Build: p})
		}
		select {
		case <-ctx.Done():
			return nil
		case <-updates:
		case <-recheck.C:
		}
	}
}

// CancelBuild asks a build's server to stop it.
func (h *Handler) CancelBuild(ctx context.Context, req *connect.Request[dockwellv1.CancelBuildRequest]) (*connect.Response[dockwellv1.CancelBuildResponse], error) {
	b, p, err := h.get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if p.GetState() != dockwellv1.BuildState_BUILD_STATE_QUEUED && p.GetState() != dockwellv1.BuildState_BUILD_STATE_BUILDING {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("the build of %s has ended, so there's nothing to cancel", b.Service), "ended")
	}
	who, actor := h.caller(ctx)
	session, ok := h.agents.SessionPastMove(ctx, b.ServerID, agentapi.MoveGrace)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't stop its build now. Try again once it reconnects", p.GetServerName()), "server_offline")
	}
	stopping, err := session.CancelBuild(ctx, &agentv1.CancelBuildCall{ChangeId: b.ChangeID, Resource: p.GetResource(), By: who})
	if err != nil {
		return nil, authapi.ReasonError(connect.CodeUnavailable, fmt.Errorf("%s didn't stop the build: %w. Try again", p.GetServerName(), err), "server_offline")
	}
	if !stopping {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s has no build of %s under way, so it may just have ended. Read it again", p.GetServerName(), b.Service), "ended")
	}
	client, _ := authapi.ClientFrom(ctx)
	err = h.db.Update(context.WithoutCancel(ctx), func(tx *store.Tx) error {
		cur, err := tx.GetBuild(ctx, b.ID)
		if err != nil {
			return err
		}
		cur.CancelledBy = who
		if err := tx.UpdateBuild(ctx, cur); err != nil {
			return err
		}
		p.CancelledBy = who
		principal, _ := authapi.PrincipalFrom(ctx)
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: h.now(), Actor: actor, Action: eventCancel, Subject: b.ID, Target: b.ServerID,
			Surface: client.Surface(principal), Detail: fmt.Sprintf("cancelled the build of %s for %s", b.Service, b.ChangeID),
		})
	})
	if err != nil {
		h.logger.WarnContext(ctx, "can't record who cancelled a build", slog.String("build", b.ID), slog.String("error", err.Error()))
	}
	return connect.NewResponse(&dockwellv1.CancelBuildResponse{Build: p}), nil
}

// caller is who calls, by name for people, and as the audit log names them.
func (h *Handler) caller(ctx context.Context) (name, actor string) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return "someone", ""
	}
	name = p.User.Name
	if p.Token != nil {
		name = "the API token " + p.Token.Name
	}
	if name == "" {
		name = p.User.Email
	}
	return name, p.Actor()
}

func (h *Handler) internal(ctx context.Context, what string, err error) error {
	if connect.CodeOf(err) != connect.CodeUnknown {
		return err
	}
	h.logger.ErrorContext(ctx, "can't serve builds", slog.String("doing", what), slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("something went wrong reading builds. Try again, and check the control server's log if it keeps happening"))
}
