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
	// lookEvery bounds how long the scheduler sleeps, so a clock that's set
	// differently is noticed.
	lookEvery = 30 * time.Second
)

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

	mu      sync.Mutex
	loaded  bool
	tasks   map[string]*kept
	running map[string]bool
	wake    chan struct{}
	log     *runLog
	retry   time.Duration
	ready   chan struct{}
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

// load reads the tasks and runs an earlier start left, once. The caller holds mu.
func (d *Driver) load() error {
	if d.loaded {
		return nil
	}
	d.tasks, d.running, d.wake = map[string]*kept{}, map[string]bool{}, make(chan struct{}, 1)
	if d.ready == nil {
		d.ready = make(chan struct{}, 1)
	}
	d.log = &runLog{dir: filepath.Join(d.dir(), "runs")}
	if err := d.log.load(); err != nil {
		return fmt.Errorf("can't read the tasks' runs in %s: %w", d.log.dir, err)
	}
	b, err := os.ReadFile(filepath.Join(d.dir(), "tasks.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("can't read the tasks this server keeps: %w", err)
	default:
		var list []*kept
		if err := json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("can't read %s: %w", filepath.Join(d.dir(), "tasks.json"), err)
		}
		for _, k := range list {
			k.spec = &resourcev1.Task{}
			if err := protojson.Unmarshal(k.Spec, k.spec); err != nil {
				return fmt.Errorf("can't read the task %s this server keeps: %w", k.Ref.Name, err)
			}
			d.tasks[k.Ref.String()] = k
		}
	}
	d.loaded = true
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
	tmp := filepath.Join(d.dir(), "tasks.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(d.dir(), "tasks.json"))
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
// from now, so it doesn't run for what was due before it was made.
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
		if !ok {
			k = &kept{Ref: step.Ref, LastDue: d.now()}
			d.tasks[key] = k
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
// was stopped runs once, first.
func (d *Driver) Keep(ctx context.Context) {
	d.mu.Lock()
	d.life = ctx
	if err := d.load(); err != nil {
		d.mu.Unlock()
		d.logger().ErrorContext(ctx, "can't run scheduled tasks", slog.String("error", err.Error()))
		return
	}
	wake := d.wake
	d.mu.Unlock()
	for {
		wait := d.tick(ctx)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-wake:
			timer.Stop()
		}
	}
}

// tick starts what's due, and says how long until the next is.
func (d *Driver) tick(ctx context.Context) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.load(); err != nil {
		d.logger().ErrorContext(ctx, "can't run scheduled tasks", slog.String("error", err.Error()))
		return lookEvery
	}
	now := d.now()
	wait := lookEvery
	changed := false
	for _, k := range d.tasks {
		if k.spec.GetPaused() {
			continue
		}
		due := Next(k.spec, k.LastDue)
		if due.IsZero() {
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
	case d.running[ref.String()]:
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

// start starts a run of k, or keeps one that's skipped as the last is still going.
// The caller holds mu.
func (d *Driver) start(ctx context.Context, k *kept, reason dockwellv1.TaskRunReason, due time.Time) *dockwellv1.TaskRun {
	now := d.now()
	run := &dockwellv1.TaskRun{
		Id: id.New(id.TaskRun), Reason: reason,
		DueTime: timestamppb.New(due), StartTime: timestamppb.New(now),
		State: dockwellv1.TaskRunState_TASK_RUN_STATE_RUNNING,
	}
	key := k.Ref.String()
	if d.running[key] {
		run.State, run.EndTime = dockwellv1.TaskRunState_TASK_RUN_STATE_SKIPPED, run.GetStartTime()
		run.Error = "the run before it was still going, and a task's runs never overlap"
		d.record(ctx, k.Ref, run)
		return run
	}
	d.running[key] = true
	d.record(ctx, k.Ref, run)
	spec := proto.Clone(k.spec).(*resourcev1.Task) //nolint:forcetypeassert // a clone has its original's type
	life := d.life
	if life == nil {
		life = context.WithoutCancel(ctx)
	}
	go d.run(context.WithoutCancel(ctx), life, k.Ref, spec, proto.Clone(run).(*dockwellv1.TaskRun)) //nolint:forcetypeassert,contextcheck // a run outlives the call that started it, and stops with the agent
	return run
}

// run runs each attempt of a run, and keeps how it went.
func (d *Driver) run(ctx, agent context.Context, ref resource.Ref, spec *resourcev1.Task, run *dockwellv1.TaskRun) {
	defer func() {
		d.mu.Lock()
		delete(d.running, ref.String())
		d.mu.Unlock()
	}()
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
			ExitCode: last.exitCode, HttpStatus: last.httpStatus, TimedOut: last.timedOut, ResponseBytes: last.bytes,
		})
		if last.err == nil || !last.started {
			break
		}
	}
	run.State, run.EndTime = stateOf(last), timestamppb.New(d.now())
	if last.err != nil {
		run.Error = last.err.Error()
	}
	run.Output, run.OutputCutBytes = out.done()
	d.record(ctx, ref, run)
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
