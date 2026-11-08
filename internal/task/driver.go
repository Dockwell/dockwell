// SPDX-License-Identifier: AGPL-3.0-only

package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

const (
	// retryAfter is how long a run waits before it tries again.
	retryAfter = time.Minute
	// lateBy is how late a due run can start and still be on time: past it, the
	// agent was stopped or the server was off, and it's a missed run.
	lateBy = time.Minute
	// clockSlack is how far ahead of the clock a task's last due time can be before
	// the clock is taken to have gone back.
	clockSlack = 5 * time.Minute
	// DrainWait is how long the agent waits, as it stops, for the runs it stopped
	// to end and be kept.
	DrainWait = stopGrace + 5*time.Second
)

// lookEvery bounds how long the scheduler sleeps, so a clock that's set
// differently is noticed, and tasks it couldn't read are read again. It's a
// variable for tests.
var lookEvery = 30 * time.Second

// Driver is the agent's: it keeps each task's spec in its data directory, runs
// each on its schedule from the server's own clock, and keeps their runs until the
// control plane reads them (ADR 0053).
type Driver struct {
	Type
	// Dir is where the agent keeps its tasks and their runs.
	Dir string
	// Client runs an app's task, as a one-off container. Nil on a server without
	// Docker, where only shell commands and requests run.
	Client *docker.Client
	Logger *slog.Logger
	// Now is the clock, for tests.
	Now func() time.Time

	mu     sync.Mutex
	loaded bool
	tasks  map[string]*kept
	// running is each task's run that's going, as it was last kept.
	running map[string]*dockwellv1.TaskRun
	wake    chan struct{}
	log     *runLog
	retry   time.Duration
	ready   chan struct{}
	// loadErr is why the tasks couldn't be read, as last logged.
	loadErr string
	// runs counts the runs going, for Drain.
	runs sync.WaitGroup
	// life is what Keep runs under: a run outlives the call that started it, as Run
	// now's does, and stops with the agent.
	life context.Context //nolint:containedctx // the agent's, for runs that outlive their call
}

var (
	_ resource.Driver = (*Driver)(nil)
	_ resource.Type   = (*Driver)(nil)
)

// kept is a task as the agent keeps it: its spec, and the due time of the last run
// it took on, so a run missed while it was stopped can be found.
type kept struct {
	Ref     resource.Ref    `json:"ref"`
	Spec    json.RawMessage `json:"spec"`
	LastDue time.Time       `json:"last_due"`
	spec    *resourcev1.Task
}

func (d *Driver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Driver) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (d *Driver) dir() string { return filepath.Join(d.Dir, "tasks") }

// goingDir holds a record of each run that's going, until it ends, so a run the
// agent stopped under is known when it starts again.
func (d *Driver) goingDir() string { return filepath.Join(d.dir(), "going") }

// load reads the tasks and runs an earlier start left, once. The caller holds mu.
func (d *Driver) load() error {
	if d.loaded {
		return nil
	}
	d.tasks, d.running, d.wake = map[string]*kept{}, map[string]*dockwellv1.TaskRun{}, make(chan struct{}, 1)
	if d.ready == nil {
		d.ready = make(chan struct{}, 1)
	}
	d.log = &runLog{dir: filepath.Join(d.dir(), "runs")}
	if err := d.log.load(); err != nil {
		return fmt.Errorf("can't read the tasks' runs in %s: %w", d.log.dir, err)
	}
	file := filepath.Join(d.dir(), "tasks.json")
	b, err := os.ReadFile(file) //nolint:gosec // the agent's own file
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("can't read the tasks this server keeps in %s, so none of them runs: %w. Check the agent can read it", file, err)
	default:
		var list []*kept
		if err := json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("can't read the tasks this server keeps in %s, so none of them runs: %w. "+
				"Mend the file, or move it aside and change each task in the control plane, which puts it back here", file, err)
		}
		for _, k := range list {
			k.spec = &resourcev1.Task{}
			if err := protojson.Unmarshal(k.Spec, k.spec); err != nil {
				return fmt.Errorf("can't read the task %s this server keeps in %s: %w. Change it in the control plane, which writes it again", k.Ref.Name, file, err)
			}
			d.tasks[k.Ref.String()] = k
		}
	}
	if err := d.endInterrupted(); err != nil {
		return err
	}
	d.loaded = true
	return nil
}

// endInterrupted ends each run an earlier start left going: the agent stopped
// under it, so it was stopped too, or how it went isn't known. The caller holds mu.
func (d *Driver) endInterrupted() error {
	names, err := os.ReadDir(d.goingDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("can't read which tasks' runs were going in %s: %w", d.goingDir(), err)
	}
	now := d.now()
	for _, n := range names {
		file := filepath.Join(d.goingDir(), n.Name())
		b, err := os.ReadFile(file) //nolint:gosec // the agent's own file
		r := &agentv1.AgentTaskRun{}
		if err == nil && protojson.Unmarshal(b, r) == nil && r.GetRun().GetId() != "" {
			run := r.GetRun()
			run.State, run.EndTime = dockwellv1.TaskRunState_TASK_RUN_STATE_INTERRUPTED, timestamppb.New(now)
			run.Error = "the agent stopped while it ran, so how it ended isn't known"
			if err := d.log.put(resourcepb.FromRef(r.GetTask()), r); err != nil {
				return fmt.Errorf("can't keep a run the agent stopped under: %w", err)
			}
		}
		_ = os.Remove(file)
	}
	return nil
}

// save writes the tasks to disk, whole. The caller holds mu.
func (d *Driver) save() error {
	list := slices.SortedFunc(maps.Values(d.tasks), func(a, b *kept) int {
		return cmpString(a.Ref.String(), b.Ref.String())
	})
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.dir(), 0o700); err != nil {
		return err
	}
	return writeSynced(filepath.Join(d.dir(), "tasks.json"), b)
}

// writeSynced writes a file whole, on the disk before it takes the old one's place,
// so a crash leaves one or the other.
func writeSynced(file string, b []byte) error {
	tmp := file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the agent's own file
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Observe is a task as this server keeps it.
func (d *Driver) Observe(_ context.Context, ref resource.Ref) (proto.Message, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(); err != nil {
		return nil, err
	}
	k, ok := d.tasks[ref.String()]
	if !ok {
		return nil, nil
	}
	return proto.Clone(k.spec), nil
}

// Plan makes, changes or deletes a task, which stops nothing and destroys nothing
// but the runs this server hasn't sent yet.
func (*Driver) Plan(actual, desired proto.Message, _ []resource.Change) (resource.Decision, error) {
	switch {
	case actual == nil:
		return resource.Decision{Action: resource.ActionCreate}, nil
	case desired == nil:
		return resource.Decision{Action: resource.ActionDelete}, nil
	}
	return resource.Decision{Action: resource.ActionUpdate}, nil
}

// Apply keeps a task, with its new spec, or forgets it. A task that's new starts
// from now, so it doesn't run for what was due before it was made, and so does one
// with a new schedule or time zone, or one that's paused or resumed: changing when
// a task runs never runs it.
func (d *Driver) Apply(_ context.Context, step resource.Step, desired proto.Message) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(); err != nil {
		return err
	}
	key := step.Ref.String()
	if desired == nil {
		delete(d.tasks, key)
	} else {
		spec := proto.Clone(desired).(*resourcev1.Task) //nolint:forcetypeassert // a clone has its original's type
		raw, err := protojson.Marshal(spec)
		if err != nil {
			return err
		}
		k, ok := d.tasks[key]
		switch {
		case !ok:
			k = &kept{Ref: step.Ref, LastDue: d.now()}
			d.tasks[key] = k
		case k.spec.GetSchedule() != spec.GetSchedule() || k.spec.GetTimeZone() != spec.GetTimeZone() || k.spec.GetPaused() != spec.GetPaused():
			k.LastDue = d.now()
		}
		k.Spec, k.spec = raw, spec
	}
	if err := d.save(); err != nil {
		return fmt.Errorf("can't keep the task %s on this server: %w", step.Ref.Name, err)
	}
	d.nudge()
	return nil
}

// Verify has nothing to wait for: a task is kept once it's written.
func (*Driver) Verify(context.Context, resource.Ref, proto.Message) error { return nil }

// nudge wakes the scheduler. The caller holds mu.
func (d *Driver) nudge() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Keep runs each task when it's due, until ctx ends. A run missed while the agent
// was stopped runs once, first. Tasks it can't read, it tries again to read, so
// mending the file is enough.
func (d *Driver) Keep(ctx context.Context) {
	d.mu.Lock()
	d.life = ctx
	d.mu.Unlock()
	for {
		wait := d.tick(ctx)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-d.waker():
			timer.Stop()
		}
	}
}

// waker is what wakes the scheduler, once the tasks are read: never before.
func (d *Driver) waker() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.loaded {
		return nil
	}
	return d.wake
}

// Drain waits up to wait for the runs going to end and be kept, once the agent is
// stopping, which stops them.
func (d *Driver) Drain(wait time.Duration) {
	done := make(chan struct{})
	go func() {
		d.runs.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

// tick starts what's due, and says how long until the next is.
func (d *Driver) tick(ctx context.Context) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(); err != nil {
		if msg := err.Error(); msg != d.loadErr {
			d.loadErr = msg
			d.logger().ErrorContext(ctx, "can't run scheduled tasks", slog.String("error", msg))
		}
		return lookEvery
	}
	if d.loadErr != "" {
		d.loadErr = ""
		d.logger().InfoContext(ctx, "reads its scheduled tasks again, and runs them")
	}
	now := d.now()
	wait := lookEvery
	changed := false
	for _, k := range d.tasks {
		if k.LastDue.After(now.Add(clockSlack)) {
			// The clock ran ahead and was set back: what ran by the wrong time
			// doesn't hold the task back until the clock catches up.
			d.logger().WarnContext(ctx, "the server's clock went back, so a scheduled task counts its runs from now",
				slog.String("task", k.Ref.Name), slog.Time("last_due", k.LastDue), slog.Time("now", now))
			k.LastDue, changed = now, true
		}
		due := Next(k.spec, k.LastDue)
		if due.IsZero() {
			continue
		}
		if k.spec.GetPaused() {
			// Its slots pass while it's paused, so resuming it misses none.
			if !due.After(now) {
				k.LastDue, changed = now, true
			}
			continue
		}
		if due.After(now) {
			wait = min(wait, due.Sub(now))
			continue
		}
		changed = true
		if now.Sub(due) > lateBy {
			// Due while the agent was stopped, or the server was off: once, however
			// many were missed, unless the task says to skip them.
			k.LastDue = now
			if !k.spec.GetSkipMissed() {
				d.start(ctx, k, dockwellv1.TaskRunReason_TASK_RUN_REASON_MISSED, due)
			}
			continue
		}
		k.LastDue = due
		d.start(ctx, k, dockwellv1.TaskRunReason_TASK_RUN_REASON_SCHEDULE, due)
		if next := Next(k.spec, due); !next.IsZero() {
			wait = min(wait, max(0, next.Sub(now)))
		}
	}
	if changed {
		if err := d.save(); err != nil {
			d.logger().ErrorContext(ctx, "can't keep when the tasks last ran", slog.String("error", err.Error()))
		}
	}
	return max(wait, 0)
}

// RunNow runs a task now, as its schedule would, paused or not.
func (d *Driver) RunNow(ctx context.Context, ref resource.Ref) (*dockwellv1.TaskRun, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(); err != nil {
		return nil, err
	}
	k, ok := d.tasks[ref.String()]
	switch {
	case !ok:
		return nil, fmt.Errorf("this server has no task %s. Check the task is placed on it", ref.Name)
	case d.running[ref.String()] != nil:
		return nil, fmt.Errorf("%s is running now, and a task's runs never overlap. Run it again once this one ends", ref.Name)
	}
	return d.start(ctx, k, dockwellv1.TaskRunReason_TASK_RUN_REASON_BY_HAND, d.now()), nil
}

// Runs returns the runs past after, and lets go of the ones before it.
func (d *Driver) Runs(after string) (*agentv1.TaskRunsResult, error) {
	d.mu.Lock()
	if err := d.load(); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	log := d.log
	d.mu.Unlock()
	return log.read(after)
}

// start starts a run of k. While the last is still going, it starts none, as a
// task's runs never overlap, and counts the slot on the run that's going. The
// caller holds mu.
func (d *Driver) start(ctx context.Context, k *kept, reason dockwellv1.TaskRunReason, due time.Time) *dockwellv1.TaskRun {
	key := k.Ref.String()
	if going := d.running[key]; going != nil {
		going.SkippedSlots++
		d.record(ctx, k.Ref, proto.CloneOf(going))
		return proto.CloneOf(going)
	}
	now := d.now()
	run := &dockwellv1.TaskRun{
		Id: id.New(id.TaskRun), Reason: reason,
		DueTime: timestamppb.New(due), StartTime: timestamppb.New(now),
		State: dockwellv1.TaskRunState_TASK_RUN_STATE_RUNNING,
	}
	d.running[key] = run
	d.record(ctx, k.Ref, run)
	d.markGoing(ctx, k.Ref, run)
	d.runs.Add(1)
	spec := proto.Clone(k.spec).(*resourcev1.Task) //nolint:forcetypeassert // a clone has its original's type
	life := d.life
	if life == nil {
		life = context.WithoutCancel(ctx)
	}
	go d.run(context.WithoutCancel(ctx), life, k.Ref, spec, proto.Clone(run).(*dockwellv1.TaskRun)) //nolint:forcetypeassert,contextcheck // a run outlives the call that started it, and stops with the agent
	return run
}

// markGoing keeps a record of a run that's going until it ends. The caller holds
// mu.
func (d *Driver) markGoing(ctx context.Context, ref resource.Ref, run *dockwellv1.TaskRun) {
	b, err := protojson.Marshal(&agentv1.AgentTaskRun{Task: resourcepb.Ref(ref), Run: run})
	if err == nil {
		if err = os.MkdirAll(d.goingDir(), 0o700); err == nil {
			err = os.WriteFile(filepath.Join(d.goingDir(), run.GetId()+".json"), b, 0o600)
		}
	}
	if err != nil {
		d.logger().ErrorContext(ctx, "can't keep that a task's run is going", slog.String("task", ref.Name), slog.String("error", err.Error()))
	}
}

// run runs each attempt of a run, and keeps how it went.
func (d *Driver) run(ctx, agent context.Context, ref resource.Ref, spec *resourcev1.Task, run *dockwellv1.TaskRun) {
	defer d.runs.Done()
	out := newOutput(d.now)
	timeout := Timeout(spec)
	retry := d.retry
	if retry == 0 {
		retry = retryAfter
	}
	var last attempt
	for try := uint32(0); try <= spec.GetRetries(); try++ {
		if try > 0 {
			out.Line(fmt.Sprintf("Dockwell: tries again in %s, %d of %d.", retry, try, spec.GetRetries()))
			select {
			case <-agent.Done():
				try = spec.GetRetries() + 1
				continue
			case <-time.After(retry):
			}
		}
		start := d.now()
		last = d.attempt(agent, ref, spec, timeout, out)
		run.Attempts = append(run.Attempts, &dockwellv1.TaskAttempt{
			StartTime: timestamppb.New(start), EndTime: timestamppb.New(d.now()),
			ExitCode: last.exitCode, HttpStatus: last.httpStatus, TimedOut: last.timedOut, ResponseBytes: last.bytes, OutputHeld: last.held,
		})
		if last.err == nil || !last.started {
			break
		}
	}
	run.State, run.EndTime = stateOf(last), timestamppb.New(d.now())
	if last.err != nil {
		run.Error = last.err.Error()
	}
	if agent.Err() != nil && last.err != nil {
		run.State = dockwellv1.TaskRunState_TASK_RUN_STATE_INTERRUPTED
	}
	run.Output, run.OutputCutBytes = out.done()
	d.mu.Lock()
	defer d.mu.Unlock()
	key := ref.String()
	run.SkippedSlots = d.running[key].GetSkippedSlots()
	d.record(ctx, ref, run)
	delete(d.running, key)
	_ = os.Remove(filepath.Join(d.goingDir(), run.GetId()+".json"))
}

// attempt tries a run once.
func (d *Driver) attempt(ctx context.Context, ref resource.Ref, spec *resourcev1.Task, timeout time.Duration, out *output) attempt {
	switch r := spec.GetRuns().(type) {
	case *resourcev1.Task_Shell:
		return runShell(ctx, r.Shell, timeout, out)
	case *resourcev1.Task_Request:
		return runRequest(ctx, r.Request, timeout, out)
	case *resourcev1.Task_App:
		return d.runApp(ctx, ref, r.App, timeout, out)
	}
	return attempt{err: errors.New("this agent doesn't know what the task runs. Update it to the control plane's version")}
}

// record keeps a run as it is now, and tells the control plane.
func (d *Driver) record(ctx context.Context, ref resource.Ref, run *dockwellv1.TaskRun) {
	if err := d.log.put(ref, &agentv1.AgentTaskRun{Task: resourcepb.Ref(ref), Run: run}); err != nil {
		d.logger().ErrorContext(ctx, "can't keep a task's run", slog.String("task", ref.Name), slog.String("error", err.Error()))
		return
	}
	// Set by load, before any run starts.
	select {
	case d.ready <- struct{}{}:
	default:
	}
}

// RunsReady says a run started or ended, so the control plane reads the runs.
func (d *Driver) RunsReady() <-chan struct{} { return d.readyCh() }

func (d *Driver) readyCh() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ready == nil {
		d.ready = make(chan struct{}, 1)
	}
	return d.ready
}
