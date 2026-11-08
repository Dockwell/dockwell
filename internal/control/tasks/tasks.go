// SPDX-License-Identifier: AGPL-3.0-only

// Package tasks is TaskService: scheduled tasks as they're asked for and as they
// run (ADR 0053). Each server's agent keeps its own tasks' time and runs them; this
// reads their runs from it when it connects and whenever it says one started or
// ended, keeps each task's newest on each server, and puts a failure on Activity.
package tasks

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/task"
)

// What the audit log calls what becomes of a task's run, each on the timeline.
const (
	// ActionFailed is a run that failed, timed out, or couldn't start.
	ActionFailed = "task.failed"
	// ActionRecovered is a run that succeeded after one that didn't.
	ActionRecovered = "task.recovered"
	// ActionRun is someone running a task by hand.
	ActionRun = "task.run"
)

// Actions are all of them.
var Actions = []string{ActionFailed, ActionRecovered, ActionRun}

// Told is what an audit event says of a run, in its detail, which the timeline
// reads. It holds nothing a run printed.
type Told struct {
	Run         string `json:"run"`
	Task        string `json:"task"`
	DisplayName string `json:"display_name,omitempty"`
	State       int32  `json:"state"`
	Reason      int32  `json:"reason"`
	ExitCode    int32  `json:"exit_code,omitempty"`
	HTTPStatus  int32  `json:"http_status,omitempty"`
	Error       string `json:"error,omitempty"`
}

// ReadTold reads what an event says of a run.
func ReadTold(detail string) (Told, bool) {
	var t Told
	err := json.Unmarshal([]byte(detail), &t)
	return t, err == nil && t.Run != ""
}

const (
	// readWait bounds one read of a server's runs.
	readWait = 30 * time.Second
	// recentRuns is how many of a task's newest runs on a server its row shows.
	recentRuns = 14
	// maxUpcoming is how many of a task's runs in the next day a timeline gets.
	maxUpcoming = 288
	// defaultPage and maxPage bound a page of runs.
	defaultPage, maxPage = 20, 100
	// pruneEvery is how often the runs of deleted tasks are looked for, and
	// keepGone how long they stay after the delete, which can be undone until then.
	pruneEvery = time.Hour
	keepGone   = 7 * 24 * time.Hour
)

// Handler implements TaskService.
type Handler struct {
	engine *revision.Engine
	agents *agentapi.Agents
	db     *store.DB
	logger *slog.Logger
	now    func() time.Time

	base    context.Context //nolint:containedctx // what reads outlive the call that asked for them: the control plane's run
	mu      sync.Mutex
	cursors map[string]string
	busy    map[string]bool
	again   map[string]bool
}

var _ dockwellv1connect.TaskServiceHandler = (*Handler)(nil)

// New returns a handler.
func New(engine *revision.Engine, agents *agentapi.Agents, db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{
		engine: engine, agents: agents, db: db, logger: logger, now: time.Now, base: context.Background(),
		cursors: map[string]string{}, busy: map[string]bool{}, again: map[string]bool{},
	}
}

// Start reads each server's runs when it connects and whenever its agent says it
// has some, until ctx ends.
func (h *Handler) Start(ctx context.Context) {
	h.mu.Lock()
	h.base = ctx
	h.mu.Unlock()
	h.agents.OnConnect(func(server string) {
		// From what the agent has kept, whatever was read before: an agent whose data
		// started again counts from the start, and a run read twice is kept once.
		h.mu.Lock()
		h.cursors[server] = ""
		h.mu.Unlock()
		h.collect(server)
	})
	h.agents.OnTaskRunsReady(h.collect)
	go h.pruneGone(ctx)
}

// pruneGone lets go of the runs of tasks deleted a while ago, now and every
// pruneEvery, until ctx ends.
func (h *Handler) pruneGone(ctx context.Context) {
	for {
		err := h.db.Update(ctx, func(tx *store.Tx) error {
			return tx.PruneGoneTaskRuns(ctx, string(task.Kind), h.now().Add(-keepGone))
		})
		if err != nil && ctx.Err() == nil {
			h.logger.ErrorContext(ctx, "can't let go of the runs of deleted tasks", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pruneEvery):
		}
	}
}

// collect reads a server's runs, once at a time for each server: what's asked while
// a read runs, the read after it takes in.
func (h *Handler) collect(server string) {
	h.mu.Lock()
	if h.busy[server] {
		h.again[server] = true
		h.mu.Unlock()
		return
	}
	h.busy[server] = true
	ctx := h.base
	h.mu.Unlock()
	go func() {
		for {
			h.read(ctx, server)
			h.mu.Lock()
			if !h.again[server] || ctx.Err() != nil {
				h.busy[server], h.again[server] = false, false
				h.mu.Unlock()
				return
			}
			h.again[server] = false
			h.mu.Unlock()
		}
	}()
}

// read reads what a server's agent has past where the last read ended, a page at a
// time, and keeps it.
func (h *Handler) read(ctx context.Context, server string) {
	s, ok := h.agents.Session(server)
	if !ok {
		return
	}
	for range 50 {
		h.mu.Lock()
		cursor := h.cursors[server]
		h.mu.Unlock()
		callCtx, cancel := context.WithTimeout(ctx, readWait)
		res, err := s.TaskRuns(callCtx, cursor)
		cancel()
		if err != nil {
			if !errors.Is(err, resource.ErrNoDriver) && ctx.Err() == nil {
				h.logger.WarnContext(ctx, "can't read a server's task runs", slog.String("server", server), slog.String("error", err.Error()))
			}
			return
		}
		for _, r := range res.GetRuns() {
			if err := h.keep(ctx, server, r); err != nil {
				h.logger.ErrorContext(ctx, "can't keep a task's run", slog.String("server", server), slog.String("error", err.Error()))
				return
			}
		}
		if n := res.GetDropped(); n > 0 {
			h.logger.WarnContext(ctx, "a server let go of task runs before they were read", slog.String("server", server), slog.Int64("runs", n))
		}
		h.mu.Lock()
		h.cursors[server] = res.GetCursor()
		h.mu.Unlock()
		if !res.GetMore() {
			return
		}
	}
}

// keep keeps a run as its agent says it is now, and puts one that ended badly, or
// well after one that didn't, on Activity, once.
func (h *Handler) keep(ctx context.Context, server string, said *agentv1.AgentTaskRun) error {
	ref := resourcepb.FromRef(said.GetTask())
	run := proto.CloneOf(said.GetRun())
	if ref.Kind != task.Kind || run.GetId() == "" {
		return nil
	}
	return h.db.Update(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResource(ctx, string(task.Kind), ref.Scope, ref.Name)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			return nil // a task that's gone keeps no runs
		}
		if err != nil {
			return err
		}
		srv, err := tx.GetServer(ctx, server)
		if err != nil {
			return err
		}
		run.TaskId, run.ServerId, run.ServerName = r.ID, server, srv.Name
		before, err := tx.GetTaskRun(ctx, run.GetId())
		known := err == nil
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := put(ctx, tx, run); err != nil {
			return err
		}
		if err := tx.PruneTaskRuns(ctx, r.ID, server); err != nil {
			return err
		}
		if run.GetEndTime() == nil || known && !before.EndedAt.IsZero() {
			return nil
		}
		action, err := h.endedAction(ctx, tx, run)
		if err != nil || action == "" {
			return err
		}
		display := ""
		if spec, err := h.engine.Kinds().Decode(task.Kind, r.Spec); err == nil {
			display = spec.(*resourcev1.Task).GetDisplayName() //nolint:forcetypeassert // the kind's own message
		}
		return record(ctx, tx, action, revision.SystemActor, store.SurfaceAutomatic, run.GetEndTime().AsTime(), ref.Name, display, run)
	})
}

// put keeps a run, with what it printed.
func put(ctx context.Context, tx *store.Tx, run *dockwellv1.TaskRun) error {
	b, err := proto.Marshal(run)
	if err != nil {
		return err
	}
	kept := store.TaskRun{
		ID: run.GetId(), TaskID: run.GetTaskId(), ServerID: run.GetServerId(), State: int32(run.GetState()),
		StartedAt: run.GetStartTime().AsTime(), Run: b,
	}
	if run.GetEndTime() != nil {
		kept.EndedAt = run.GetEndTime().AsTime()
	}
	return tx.PutTaskRun(ctx, kept)
}

// failed says a run ended badly. One the agent stopped under says nothing of the
// task.
func failed(s dockwellv1.TaskRunState) bool {
	return s == dockwellv1.TaskRunState_TASK_RUN_STATE_FAILED || s == dockwellv1.TaskRunState_TASK_RUN_STATE_TIMED_OUT ||
		s == dockwellv1.TaskRunState_TASK_RUN_STATE_NOT_STARTED
}

// endedAction is what a run that just ended puts on Activity: the first failure
// after a run that succeeded, or before any, and the first success after a failure.
// A task that goes on failing, or on succeeding, adds nothing more.
func (h *Handler) endedAction(ctx context.Context, tx *store.Tx, run *dockwellv1.TaskRun) (string, error) {
	state := run.GetState()
	if !failed(state) && state != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED {
		return "", nil
	}
	at := run.GetStartTime().AsTime()
	earlier, err := tx.ListTaskRuns(ctx, store.TaskRunFilter{TaskID: run.GetTaskId(), ServerID: run.GetServerId(), AfterAt: &at, AfterID: run.GetId(), Limit: 10})
	if err != nil {
		return "", err
	}
	failedBefore := false
	for i := range earlier {
		e := &earlier[i]
		if e.EndedAt.IsZero() || dockwellv1.TaskRunState(e.State) == dockwellv1.TaskRunState_TASK_RUN_STATE_INTERRUPTED {
			continue
		}
		failedBefore = failed(dockwellv1.TaskRunState(e.State))
		break
	}
	switch {
	case failed(state) && !failedBefore:
		return ActionFailed, nil
	case !failed(state) && failedBefore:
		return ActionRecovered, nil
	}
	return "", nil
}

// record puts a run on Activity.
func record(ctx context.Context, tx *store.Tx, action, actor, surface string, at time.Time, name, display string, run *dockwellv1.TaskRun) error {
	told := Told{Run: run.GetId(), Task: name, DisplayName: display, State: int32(run.GetState()), Reason: int32(run.GetReason()), Error: run.GetError()}
	if n := len(run.GetAttempts()); n > 0 {
		last := run.GetAttempts()[n-1]
		told.ExitCode, told.HTTPStatus = last.GetExitCode(), last.GetHttpStatus()
	}
	detail, err := json.Marshal(told)
	if err != nil {
		return err
	}
	return tx.RecordEvent(ctx, store.Event{
		ID: id.New(id.Event), At: at, Actor: actor, Action: action, Subject: run.GetTaskId(), Target: run.GetServerId(), Detail: string(detail), Surface: surface,
	})
}

// stored is a task as the store has it.
type stored struct {
	res     store.Resource
	spec    *resourcev1.Task
	servers []store.Server
}

// ListTasks returns every task, by name.
func (h *Handler) ListTasks(ctx context.Context, _ *connect.Request[dockwellv1.ListTasksRequest]) (*connect.Response[dockwellv1.ListTasksResponse], error) {
	out := &dockwellv1.ListTasksResponse{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListKind(ctx, string(task.Kind))
		if err != nil {
			return err
		}
		for i := range list {
			if list[i].Spec == nil {
				continue
			}
			s, err := h.load(ctx, tx, list[i])
			if err != nil {
				return err
			}
			info, err := h.info(ctx, tx, s)
			if err != nil {
				return err
			}
			out.Tasks = append(out.Tasks, info)
		}
		return nil
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	slices.SortFunc(out.Tasks, func(a, b *dockwellv1.TaskInfo) int {
		return cmp.Or(cmp.Compare(a.GetResource().GetName(), b.GetResource().GetName()), cmp.Compare(a.GetId(), b.GetId()))
	})
	return connect.NewResponse(out), nil
}

// GetTask returns one task.
func (h *Handler) GetTask(ctx context.Context, req *connect.Request[dockwellv1.GetTaskRequest]) (*connect.Response[dockwellv1.GetTaskResponse], error) {
	m := req.Msg
	var out *dockwellv1.TaskInfo
	err := h.db.View(ctx, func(tx *store.Tx) error {
		s, err := h.find(ctx, tx, m.GetId(), m.GetProject(), m.GetEnvironment())
		if err != nil {
			return err
		}
		out, err = h.info(ctx, tx, s)
		return err
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.GetTaskResponse{Task: out}), nil
}

// ListTaskRuns returns a task's runs, newest first, with what each printed.
func (h *Handler) ListTaskRuns(ctx context.Context, req *connect.Request[dockwellv1.ListTaskRunsRequest]) (*connect.Response[dockwellv1.ListTaskRunsResponse], error) {
	m := req.Msg
	size := int(m.GetPageSize())
	switch {
	case size < 0 || size > maxPage:
		return nil, authapi.Invalid(fmt.Errorf("a page holds 1 to %d runs, not %d", maxPage, size))
	case size == 0:
		size = defaultPage
	}
	afterAt, afterID, err := readToken(m.GetPageToken())
	if err != nil {
		return nil, authapi.Invalid(err)
	}
	out := &dockwellv1.ListTaskRunsResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		s, err := h.find(ctx, tx, m.GetId(), m.GetProject(), m.GetEnvironment())
		if err != nil {
			return err
		}
		f := store.TaskRunFilter{TaskID: s.res.ID, Limit: size + 1, AfterID: afterID}
		if !afterAt.IsZero() {
			f.AfterAt = &afterAt
		}
		if m.GetServer() != "" {
			srv, err := oneOf(s, m.GetServer())
			if err != nil {
				return err
			}
			f.ServerID = srv.ID
		}
		runs, err := tx.ListTaskRuns(ctx, f)
		if err != nil {
			return err
		}
		for i := range runs {
			r := &runs[i]
			if i == size {
				out.NextPageToken = token(&runs[i-1])
				break
			}
			run := &dockwellv1.TaskRun{}
			if err := proto.Unmarshal(r.Run, run); err != nil {
				return fmt.Errorf("can't read the run %s: %w", r.ID, err)
			}
			out.Runs = append(out.Runs, run)
		}
		return nil
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(out), nil
}

// RunTask runs a task now on one of its servers.
func (h *Handler) RunTask(ctx context.Context, req *connect.Request[dockwellv1.RunTaskRequest]) (*connect.Response[dockwellv1.RunTaskResponse], error) {
	m := req.Msg
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	client, _ := authapi.ClientFrom(ctx)
	var s stored
	var srv store.Server
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if s, err = h.find(ctx, tx, m.GetId(), m.GetProject(), m.GetEnvironment()); err != nil {
			return err
		}
		switch {
		case m.GetServer() != "":
			srv, err = oneOf(s, m.GetServer())
		case len(s.servers) == 1:
			srv = s.servers[0]
		case len(s.servers) == 0:
			err = authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s is placed on no server yet, so nothing runs it. Place it on one", s.res.Name), "not_placed")
		default:
			names := make([]string, len(s.servers))
			for i := range s.servers {
				names[i] = s.servers[i].Name
			}
			err = authapi.Invalid(fmt.Errorf("%s runs on %s. Pass server to say which runs it now", s.res.Name, human.List(names)))
		}
		return err
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	session, ok := h.agents.Session(srv.ID)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s isn't connected, so it can't run %s now. It runs it on its schedule all the same", srv.Name, s.res.Name), "offline")
	}
	ref := resource.Ref{Kind: task.Kind, Scope: s.res.Scope, Name: s.res.Name}
	run, err := session.RunTask(ctx, ref)
	if err != nil {
		plain, _ := resource.Explain(err)
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s can't run %s now: %s", srv.Name, s.res.Name, strings.TrimSuffix(plain, ".")), "not_run")
	}
	run.TaskId, run.ServerId, run.ServerName = s.res.ID, srv.ID, srv.Name
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		if err := put(ctx, tx, run); err != nil {
			return err
		}
		return record(ctx, tx, ActionRun, p.Actor(), client.Surface(p), h.now(), s.res.Name, s.spec.GetDisplayName(), run)
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.RunTaskResponse{Run: run}), nil
}

// load reads a stored task's spec and the servers its latest revision is on.
func (h *Handler) load(ctx context.Context, tx *store.Tx, r store.Resource) (stored, error) {
	spec, err := h.engine.Kinds().Decode(task.Kind, r.Spec)
	if err != nil {
		return stored{}, fmt.Errorf("can't read the task %s: %w", r.Name, err)
	}
	ids, err := tx.Placement(ctx, r.ID, r.Revision)
	if err != nil {
		return stored{}, err
	}
	s := stored{res: r, spec: spec.(*resourcev1.Task)} //nolint:forcetypeassert // the kind's own message
	for _, sid := range ids {
		srv, err := tx.GetServer(ctx, sid)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return stored{}, err
		}
		s.servers = append(s.servers, srv)
	}
	slices.SortFunc(s.servers, func(a, b store.Server) int { return cmp.Compare(a.Name, b.Name) })
	return s, nil
}

// info is a task as the API says it: its spec, and how it stands on each server.
func (h *Handler) info(ctx context.Context, tx *store.Tx, s stored) (*dockwellv1.TaskInfo, error) {
	out := &dockwellv1.TaskInfo{
		Id: s.res.ID, Resource: &dockwellv1.ResourceRef{Kind: string(task.Kind), Scope: s.res.Scope, Name: s.res.Name},
		Kind: task.KindOf(s.spec), Spec: s.spec,
	}
	now := h.now()
	upcoming := task.Upcoming(s.spec, now, now.Add(24*time.Hour), maxUpcoming)
	for i := range s.servers {
		srv := &s.servers[i]
		on := &dockwellv1.TaskOnServer{ServerId: srv.ID, ServerName: srv.Name, Connection: h.connection(*srv)}
		if !s.spec.GetPaused() {
			if next := task.Next(s.spec, now); !next.IsZero() {
				on.NextRun = timestamppb.New(next)
			}
			for _, at := range upcoming {
				on.Upcoming = append(on.Upcoming, timestamppb.New(at))
			}
		}
		runs, err := tx.ListTaskRuns(ctx, store.TaskRunFilter{TaskID: s.res.ID, ServerID: srv.ID, Limit: recentRuns})
		if err != nil {
			return nil, err
		}
		for i := range runs {
			r := &runs[i]
			on.Recent = append(on.Recent, dockwellv1.TaskRunState(r.State))
			if i > 0 {
				continue
			}
			last := &dockwellv1.TaskRun{}
			if err := proto.Unmarshal(r.Run, last); err != nil {
				return nil, fmt.Errorf("can't read the run %s: %w", r.ID, err)
			}
			last.Output = nil
			on.LastRun = last
		}
		out.Servers = append(out.Servers, on)
	}
	return out, nil
}

// connection is how a server's agent stands: connected, gone quiet, or never yet.
func (h *Handler) connection(srv store.Server) dockwellv1.Connection {
	if _, ok := h.agents.Session(srv.ID); ok {
		return dockwellv1.Connection_CONNECTION_CONNECTED
	}
	if srv.LastSeenAt == nil {
		return dockwellv1.Connection_CONNECTION_PENDING
	}
	return dockwellv1.Connection_CONNECTION_UNREACHABLE
}

// find reads the task a request names, by ID or by name. A name may need its
// project or environment to say which.
func (h *Handler) find(ctx context.Context, tx *store.Tx, named, project, environment string) (stored, error) {
	if named == "" {
		return stored{}, authapi.Invalid(errors.New("the request names no task. Pass its ID, or its name"))
	}
	if strings.HasPrefix(named, (task.Type{}).IDPrefix()+"_") {
		r, err := tx.GetResourceByID(ctx, named)
		if err != nil {
			return stored{}, err
		}
		if r.Kind != string(task.Kind) || r.Spec == nil {
			return stored{}, store.ErrNotFound
		}
		return h.load(ctx, tx, r)
	}
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return stored{}, err
	}
	var found []store.Resource
	var where []string
	for _, p := range tree.Projects {
		if project != "" && project != p.ID && project != p.Name {
			continue
		}
		for _, e := range p.Environments {
			if environment != "" && environment != e.ID && environment != e.Name {
				continue
			}
			r, err := tx.GetResource(ctx, string(task.Kind), e.ID, named)
			if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
				continue
			}
			if err != nil {
				return stored{}, err
			}
			found = append(found, r)
			where = append(where, p.Name+" "+e.Name)
		}
	}
	switch len(found) {
	case 0:
		return stored{}, store.ErrNotFound
	case 1:
		return h.load(ctx, tx, found[0])
	}
	return stored{}, authapi.Invalid(fmt.Errorf("more than one task is named %s: in %s. Pass project or environment to say which, or use its ID", named, human.List(where)))
}

// oneOf is the server a request names, by name or ID, among those a task is on.
func oneOf(s stored, named string) (store.Server, error) {
	for i := range s.servers {
		if srv := s.servers[i]; srv.ID == named || srv.Name == named {
			return srv, nil
		}
	}
	return store.Server{}, authapi.Invalid(fmt.Errorf("%s isn't placed on %s", s.res.Name, named))
}

// token and readToken say where a page of runs ended.
func token(r *store.TaskRun) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(r.StartedAt.UnixMilli(), 10) + ":" + r.ID))
}

func readToken(t string) (time.Time, string, error) {
	if t == "" {
		return time.Time{}, "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(t)
	at, runID, ok := strings.Cut(string(b), ":")
	ms, perr := strconv.ParseInt(at, 10, 64)
	if err != nil || !ok || perr != nil {
		return time.Time{}, "", errors.New("the page token isn't one a list of runs gave. Leave it out to start from the newest")
	}
	return time.UnixMilli(ms).UTC(), runID, nil
}

// fail says what went wrong in words a caller can act on.
func (h *Handler) fail(ctx context.Context, err error) error {
	var cerr *connect.Error
	switch {
	case errors.As(err, &cerr):
		return err
	case errors.Is(err, store.ErrNotFound):
		return authapi.ReasonError(connect.CodeNotFound, errors.New("there's no such task. List them to find its name"), "not_found")
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "a task call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read its tasks; try again, and check its logs if it keeps failing"))
}
