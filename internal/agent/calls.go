// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// errNoFiles answers a call for a server's files on an agent set up without them.
var errNoFiles = errors.New("this agent doesn't read files")

// drainLimit bounds how long a stream that is moving to a renewed
// certificate waits for its calls to finish.
const drainLimit = 15 * time.Minute

const (
	// DefaultCallGrace is how long a call gets to end once its time is up or its
	// stream is going. Past it, it's left behind.
	DefaultCallGrace = 30 * time.Second
	// maxLeftBehind is how many calls can be left behind at once, across streams.
	// Each holds a goroutine and a thread until whatever it's stuck on comes back,
	// so with no bound, a plan a minute on a dead mount would add up to the
	// agent's whole memory budget in days.
	maxLeftBehind = 16
)

// calls runs the control plane's calls on one stream, each in its own
// goroutine, and sends each result back. They end with the stream: a call
// still running when it drops is cancelled, and the control plane plans
// again from what the server has when the agent reconnects.
//
// The stream ends in order: close refuses the calls that arrive from then
// on, the stream's receiver stops, and stop cancels what still runs and
// waits for it. So no call starts while stop waits, and moving to a renewed
// certificate never cuts a step off that it took on.
//
// That wait has a bound. A call can be stuck where nothing can stop it, as a system
// call on a network mount whose far end is down is, and the agent's next stream,
// and so the server being online, can't depend on it. So the agent comes back, and
// what it has to keep is smaller: nothing else touches the resource of a step that
// hasn't ended, whatever its call has done, as that step may still be stopping,
// starting and renaming what the next one would. See unfinished, and finish for how
// a call is left behind.
type calls struct {
	ctx   context.Context // the calls', apart from the stream's
	end   context.CancelFunc
	cfg   Config
	local resource.Local
	// delivery opens the values of variables and secrets sent for a step,
	// sealed for server.
	delivery *delivery.Key
	server   string
	send     func(*agentv1.ConnectRequest) error
	logger   *slog.Logger
	// grace is how long a call gets to end once it should.
	grace time.Duration
	// unfinished is what outlives its call on this agent, on any stream.
	unfinished *unfinished

	mu      sync.Mutex
	running map[uint64]context.CancelFunc
	// reading are the logs calls under way. They don't hold up a move to a
	// renewed certificate, as a follow has no end: they stop, and the control
	// plane carries them on over the next stream.
	reading map[uint64]context.CancelFunc
	closing bool // refuse calls from now on
	wg      sync.WaitGroup
	idle    chan struct{} // signalled when the last running call ends
	// drained is closed once the control plane answers Draining: it sends
	// no more calls on this stream.
	drained     chan struct{}
	drainedOnce sync.Once
}

func newCalls(ctx context.Context, cfg Config, send func(*agentv1.ConnectRequest) error, key *delivery.Key) *calls {
	ctx, end := context.WithCancel(ctx)
	var server string
	if cfg.Identity != nil {
		server = cfg.Identity.ServerID
	}
	grace, held := cfg.CallGrace, cfg.unfinished
	if grace <= 0 {
		grace = DefaultCallGrace
	}
	if held == nil {
		held = newUnfinished()
	}
	return &calls{
		ctx: ctx, end: end, cfg: cfg, local: resource.Local{Drivers: cfg.Drivers}, delivery: key, server: server, send: send, logger: cfg.Logger,
		grace: grace, unfinished: held,
		running: map[uint64]context.CancelFunc{}, reading: map[uint64]context.CancelFunc{},
		idle: make(chan struct{}, 1), drained: make(chan struct{}),
	}
}

// acknowledged notes the control plane's answer to Draining.
func (c *calls) acknowledged() { c.drainedOnce.Do(func() { close(c.drained) }) }

// start runs a call, or refuses it once the stream is closing: nothing
// runs, and the control plane sends it again on the next stream.
func (c *calls) start(call *agentv1.Call) {
	timeout := call.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = time.Minute
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		c.reply(call.GetId(), &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{
			Message:  "this agent is moving to a new stream, so it didn't run the call; send it again once the agent reconnects",
			Draining: true,
		}}})
		return
	}
	if n := c.unfinished.left.Load(); n >= maxLeftBehind {
		c.mu.Unlock()
		c.reply(call.GetId(), &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: fmt.Sprintf(
			"%d calls on this agent never came back, so it takes no more until one does. Something they touch isn't answering, "+
				"such as a network mount. Check the server's mounts, then restart the agent if they don't come back", n)}}})
		return
	}
	if logs := call.GetLogs(); logs != nil {
		ctx, cancel := context.WithCancel(c.ctx)
		c.reading[call.GetId()] = cancel
		c.wg.Add(1)
		c.mu.Unlock()
		go func() {
			defer c.wg.Done()
			defer c.doneReading(call.GetId(), cancel)
			c.reply(call.GetId(), c.readLogs(ctx, logs))
		}()
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	c.running[call.GetId()] = cancel
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer c.done(call.GetId())
		// A file call keeps its place until its result has gone, so what waits
		// for a slow stream is bounded, and a call whose caller left drops out.
		if c.cfg.Files != nil && (call.GetListFiles() != nil || call.GetReadFile() != nil || call.GetSaveFile() != nil) {
			release, err := c.cfg.Files.Take(ctx)
			if err != nil {
				c.reply(call.GetId(), &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: err.Error()}}})
				return
			}
			defer release()
		}
		result := c.finish(ctx, call)
		// A step cut short because this stream is ending can only say it was
		// cancelled. Sending nothing lets the control plane say what's true:
		// the agent disconnected, and the step may or may not have run.
		if result != nil && (c.ctx.Err() == nil || result.GetError() == nil) {
			c.reply(call.GetId(), result)
		}
	}()
}

// A call's state, for what comes back to know whether anyone still waits.
const (
	callRunning int32 = iota
	callReturned
	callLeftBehind
)

// finish carries out a call on a goroutine of its own, and waits for it until its
// time is up or its stream goes, and grace more, and for as long as the step says
// it has left to finish (resource.Finishing). Past that it's left behind, as nothing
// can stop a call stuck in the kernel, and from then on it touches nothing. Its
// result is then nil: the control plane stopped waiting at the call's own deadline,
// which came first, and said so itself.
func (c *calls) finish(ctx context.Context, call *agentv1.Call) *agentv1.CallResult {
	step, refused := c.unfinished.begin(ctx, call)
	if refused != nil {
		return refused
	}
	done := make(chan *agentv1.CallResult, 1)
	var state atomic.Int32
	what := callKind(call)
	overtime := resource.NewOvertime()
	go func() {
		result := c.run(resource.WithOvertime(ctx, overtime), call)
		c.unfinished.end(step)
		if state.CompareAndSwap(callRunning, callReturned) {
			done <- result
			return
		}
		c.unfinished.left.Add(-1)
		c.unfinished.wake() // a call refused at the limit can go now
		c.logger.Info("a call that was left behind came back", slog.String("call", what), slog.Uint64("id", call.GetId()))
	}()
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
	}
	grace := time.NewTimer(c.grace)
	defer grace.Stop()
	for {
		select {
		case result := <-done:
			return result
		case <-grace.C:
		}
		wait := overtime.Leave(c.grace)
		if wait <= 0 {
			break
		}
		grace.Reset(wait)
	}
	if !state.CompareAndSwap(callRunning, callLeftBehind) {
		return <-done
	}
	n := c.unfinished.left.Add(1)
	c.logger.Warn("a call didn't end when it should, so the agent left it behind and went on; something it touches may not be answering, such as a network mount",
		slog.String("call", what), slog.Uint64("id", call.GetId()), slog.Int("left_behind", int(n)))
	return nil
}

// callKind names what a call asks, as the protocol does, for the log.
func callKind(call *agentv1.Call) string {
	m := call.ProtoReflect()
	if f := m.WhichOneof(m.Descriptor().Oneofs().ByName("call")); f != nil {
		return string(f.Name())
	}
	return "call"
}

func (c *calls) doneReading(id uint64, cancel context.CancelFunc) {
	cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reading, id)
}

// readLogs runs a logs call, sending its lines over a connection of its own:
// when the control plane stops reading them, because whoever reads them is
// slow, flow control stops this connection alone, and the agent's stream, with
// its stats, plans and applies, goes on. The connection goes with the call.
func (c *calls) readLogs(ctx context.Context, call *agentv1.LogsCall) *agentv1.CallResult {
	fail := func(err error) *agentv1.CallResult {
		return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: err.Error(), Draining: c.isClosing() && c.ctx.Err() == nil}}}
	}
	if c.cfg.Logs == nil {
		return fail(errors.New("this agent can't read logs, as it has no Docker driver"))
	}
	client, release, err := c.cfg.Identity.Client()
	if err != nil {
		return fail(err)
	}
	defer release()
	// A reader that stopped taking lines leaves a send blocked, which only
	// ending the upload's context ends. Closing it would wait on that send, so a
	// call that failed is cancelled instead.
	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	upload := client.SendLogs(uploadCtx)
	err = c.cfg.Logs.Stream(ctx, call, upload.Send)
	if err != nil {
		cancel()
	} else if _, cerr := upload.CloseAndReceive(); cerr != nil && ctx.Err() == nil && !errors.Is(cerr, io.EOF) {
		err = cerr
	}
	switch {
	case err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil:
		return fail(err)
	case ctx.Err() != nil && c.isClosing():
		return fail(errors.New("this agent is moving to a new stream, so it stopped reading logs; read them again once the agent reconnects"))
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_Logs{Logs: &agentv1.LogsResult{}}}
}

// reply sends a call's result.
func (c *calls) reply(id uint64, result *agentv1.CallResult) {
	result.Id = id
	if err := c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Result{Result: result}}); err != nil && c.ctx.Err() == nil && !c.isClosing() {
		c.logger.Warn("can't send a call's result; the control plane checks again when the stream is back",
			slog.Uint64("call", id), slog.String("error", errText(err)))
	}
}

// close refuses the calls that arrive from now on. Those already running
// carry on.
func (c *calls) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closing = true
}

func (c *calls) isClosing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closing
}

func (c *calls) done(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cancel, ok := c.running[id]; ok {
		cancel()
		delete(c.running, id)
	}
	if len(c.running) == 0 {
		select {
		case c.idle <- struct{}{}:
		default:
		}
	}
}

// cancel stops a call the control plane no longer waits for.
func (c *calls) cancel(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cancel, ok := c.running[id]; ok {
		cancel()
	}
	if cancel, ok := c.reading[id]; ok {
		cancel()
	}
}

// stopReading ends the logs calls under way, for the control plane to carry on
// over the next stream.
func (c *calls) stopReading() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.reading {
		cancel()
	}
}

func (c *calls) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.running) > 0
}

// stop cancels every call still running and waits for them, within a bound: the
// agent's next stream never depends on a call returning. What's left past it is a
// step still finishing, which keeps its resource to itself (unfinished), or a send
// or a logs call that can't end. It closes first, so no call starts while it waits;
// stop the stream's receiver between the two.
func (c *calls) stop() {
	c.close()
	c.end()
	ended := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(ended)
	}()
	limit := time.NewTimer(2 * c.grace)
	defer limit.Stop()
	select {
	case <-ended:
	case <-limit.C:
		c.logger.Warn("a call on this stream hasn't ended, so the stream ended without it and the agent goes on to its next")
	}
}

// drain readies a stream that moves to a renewed certificate to end, while
// it keeps reporting stats. It refuses calls from now on, and tells the
// control plane, which sends no more and says so; the calls that arrive
// before its answer are refused, and the control plane sends them again on
// the next stream. drain then waits for the calls under way to finish, so
// moving doesn't cut a step off halfway. It gives up after drainLimit.
func (c *calls) drain(ctx context.Context, recvErr <-chan error, tick <-chan time.Time, sendStats func() error) error {
	c.close()
	c.stopReading()
	if err := c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Draining{Draining: &agentv1.Draining{}}}); err != nil {
		return err
	}
	limit := time.NewTimer(drainLimit)
	defer limit.Stop()
	drained, answered := c.drained, false
	for c.busy() || !answered {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-recvErr:
			return err
		case <-tick:
			if err := sendStats(); err != nil {
				return err
			}
		case <-c.idle:
		case <-drained:
			drained, answered = nil, true
		case <-limit.C:
			return nil
		}
	}
	return nil
}

// run carries out one call with the agent's drivers.
func (c *calls) run(ctx context.Context, call *agentv1.Call) *agentv1.CallResult {
	fail := func(err error) *agentv1.CallResult {
		plain, detail := resource.Explain(err)
		if detail == "" {
			plain = ""
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{
			Message: err.Error(), Plain: plain, OutOfTime: errors.Is(err, context.DeadlineExceeded), Reason: resource.ReasonOf(err),
		}}}
	}
	switch {
	case call.GetPlan() != nil:
		desired := make([]resource.Desired, 0, len(call.GetPlan().GetDesired()))
		names := map[string]string{}
		for _, d := range call.GetPlan().GetDesired() {
			ref, spec, err := c.decode(d)
			if err != nil {
				return fail(err)
			}
			want := resource.Desired{Ref: ref, Spec: spec, Pull: d.GetPull(), Run: d.GetRun(), Restart: d.GetRestart(), Rollback: d.GetRollback(), Adopt: d.GetAdopt()}
			if id := d.GetBackupId(); id != "" {
				want.Backup = &resource.BackupAsked{ID: id}
			}
			if r := d.GetRestore(); r != nil {
				want.Restore = &resource.RestoreSource{Backup: r.GetBackupId(), Undoes: r.GetUndoes(), Change: r.GetChangeId()}
			}
			desired = append(desired, want)
			if n := d.GetScopeName(); n != "" {
				names[ref.Scope] = n
			}
		}
		steps, err := c.local.Plan(resource.WithScopeNames(ctx, names), desired)
		if err != nil {
			return fail(err)
		}
		out := &agentv1.PlanResult{}
		for i := range steps {
			out.Steps = append(out.Steps, resourcepb.Step(steps[i], ""))
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Plan{Plan: out}}
	case call.GetApply() != nil:
		step, err := resourcepb.FromStep(call.GetApply().GetStep())
		if err != nil {
			return fail(fmt.Errorf("this agent can't read the step it was sent: %w. "+
				"The control plane is newer than the agent, so update the agent to the control plane's version", err))
		}
		ref, spec, err := c.decode(call.GetApply().GetDesired())
		if err != nil {
			return fail(err)
		}
		step.Ref, step.Pulled, step.Finishing = ref, call.GetApply().GetPulled(), call.GetApply().GetFinishing()
		step.PlannedLeads = call.GetApply().GetPlannedLeads()
		if n := call.GetApply().GetDesired().GetScopeName(); n != "" {
			ctx = resource.WithScopeNames(ctx, map[string]string{ref.Scope: n})
		}
		progress := c.progress(call.GetId())
		defer progress.stop()
		ctx = resource.WithProgress(ctx, progress.say)
		ctx = resource.WithOutcome(ctx, func(o resource.Outcome) {
			_ = c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Progress{Progress: &agentv1.CallProgress{
				Id: call.GetId(), Outcome: &agentv1.StepOutcome{Digest: o.Digest, Previous: o.Previous, Replacing: o.Replacing, Note: o.Note, Untouched: o.Untouched, Did: resourcepb.Did(o.Did), Backup: backupTaken(o.Backup)},
			}}})
		})
		if d := call.GetApply().GetDeploy(); d != nil {
			ctx = resource.WithDeploy(ctx, resource.Deploy{
				Revision: int(d.GetRevision()), Change: d.GetChangeId(), Rollback: d.GetRollback(),
			})
		}
		// The values of what the spec uses, opened in memory for this
		// step alone; they never reach a log, the disk or Desired, which
		// hears of the step with the call's own context.
		applyCtx := ctx
		if sealed := call.GetApply().GetValues(); len(sealed) > 0 {
			if c.delivery == nil {
				return fail(errors.New("this agent has no delivery key to open the values sent for this step"))
			}
			values, err := c.delivery.Open(c.server, ref, sealed)
			if err != nil {
				return fail(err)
			}
			defer clear(values)
			applyCtx = resource.WithValues(ctx, values)
		}
		if err := c.local.Apply(applyCtx, step, spec); err != nil {
			return fail(err)
		}
		if c.cfg.Desired != nil {
			if err := c.cfg.Desired.Applied(ctx, ref, spec); err != nil {
				c.logger.Warn("applied a step, but can't keep its desired state", slog.String("resource", ref.String()), slog.String("error", err.Error()))
			}
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Apply{Apply: &agentv1.ApplyResult{}}}
	case call.GetObserve() != nil:
		refs := make([]resource.Ref, 0, len(call.GetObserve().GetResources()))
		for _, r := range call.GetObserve().GetResources() {
			refs = append(refs, resourcepb.FromRef(r))
		}
		observed, err := c.local.Observe(ctx, refs)
		if err != nil {
			return fail(err)
		}
		out := &agentv1.ObserveResult{}
		for _, o := range observed {
			res := &agentv1.ObservedResource{}
			if o != nil {
				b, err := resource.Encode(o)
				if err != nil {
					return fail(err)
				}
				res.Spec = b
			}
			out.Resources = append(out.Resources, res)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Observe{Observe: out}}
	case call.GetLogSources() != nil:
		if c.cfg.Logs == nil {
			return fail(errors.New("this agent can't read logs, as it has no Docker driver"))
		}
		sources, err := c.cfg.Logs.Sources(ctx, call.GetLogSources())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_LogSources{LogSources: &agentv1.LogSourcesResult{Sources: sources}}}
	case call.GetContainers() != nil:
		found, err := containers(ctx, c.cfg.Docker, call.GetContainers())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Containers{Containers: found}}
	case call.GetAdoption() != nil:
		found, err := c.adoption(ctx, call.GetAdoption())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Adoption{Adoption: found}}
	case call.GetBackupPreview() != nil, call.GetBackup() != nil, call.GetDeleteBackup() != nil, call.GetFetchBackup() != nil, call.GetSendBackup() != nil:
		if c.cfg.Backups == nil {
			return fail(errors.New("this agent can't back up volumes, as it has no Docker driver"))
		}
		result, err := c.runBackup(ctx, call)
		if err != nil {
			return fail(err)
		}
		return result
	}
	if f := call.GetForgotten(); f != nil {
		if c.cfg.Logs != nil {
			c.cfg.Logs.Forgotten(f.GetServices())
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Forgotten{Forgotten: &agentv1.ForgottenResult{}}}
	}
	if list := call.GetListFiles(); list != nil {
		if c.cfg.Files == nil {
			return fail(errNoFiles)
		}
		listing, err := c.cfg.Files.List(ctx, list.GetPath())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ListFiles{ListFiles: listing}}
	}
	if read := call.GetReadFile(); read != nil {
		if c.cfg.Files == nil {
			return fail(errNoFiles)
		}
		file, err := c.cfg.Files.Read(ctx, read.GetPath())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ReadFile{ReadFile: file}}
	}
	if save := call.GetSaveFile(); save != nil {
		if c.cfg.Files == nil {
			return fail(errNoFiles)
		}
		saved, err := c.cfg.Files.Save(ctx, save.GetPath(), save.GetContent(), save.GetVersion(), save.GetRecord(), save.GetConfirmedPath())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_SaveFile{SaveFile: saved}}
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{
		Message: "this agent doesn't know that call; update it to the control plane's version",
	}}}
}

// runBackup carries out a call about backups.
func (c *calls) runBackup(ctx context.Context, call *agentv1.Call) (*agentv1.CallResult, error) {
	switch {
	case call.GetBackupPreview() != nil:
		out, err := c.cfg.Backups.Preview(ctx, call.GetBackupPreview())
		if err != nil {
			return nil, err
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_BackupPreview{BackupPreview: out}}, nil
	case call.GetBackup() != nil:
		progress := c.progress(call.GetId())
		defer progress.stop()
		out, err := c.cfg.Backups.Take(resource.WithProgress(ctx, progress.say), call.GetBackup())
		if err != nil {
			return nil, err
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Backup{Backup: out}}, nil
	case call.GetSendBackup() != nil:
		send := call.GetSendBackup()
		destination, err := c.openDestination(send.GetVolume(), send.GetDestination())
		if err != nil {
			return nil, err
		}
		defer clear(destination)
		progress := c.progress(call.GetId())
		defer progress.stop()
		out, err := c.cfg.Backups.Send(resource.WithProgress(ctx, progress.say), send, destination)
		if err != nil {
			return nil, err
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_SendBackup{SendBackup: out}}, nil
	case call.GetFetchBackup() != nil:
		f := call.GetFetchBackup()
		destination, err := c.openDestination(f.GetVolume(), f.GetDestination())
		if err != nil {
			return nil, err
		}
		defer clear(destination)
		progress := c.progress(call.GetId())
		defer progress.stop()
		out, err := c.cfg.Backups.Fetch(resource.WithProgress(ctx, progress.say), f, destination)
		if err != nil {
			return nil, err
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_FetchBackup{FetchBackup: out}}, nil
	}
	d := call.GetDeleteBackup()
	destination, err := c.openDestination(d.GetVolume(), d.GetDestination())
	if err != nil {
		return nil, err
	}
	defer clear(destination)
	out, err := c.cfg.Backups.Delete(ctx, d, destination)
	if err != nil {
		return nil, err
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_DeleteBackup{DeleteBackup: out}}, nil
}

// backupTaken is what a step took, as the stream carries it.
func backupTaken(b *resource.BackupTaken) *agentv1.BackupResult {
	if b == nil {
		return nil
	}
	return &agentv1.BackupResult{Id: b.ID, Method: resourcepb.BackupMethod(b.Method), Database: b.Database, Sha256: b.SHA256, SizeBytes: b.Size, FilesChanged: b.Changed}
}

// openDestination opens where a backup goes, sealed to this stream for this volume,
// in memory for one call (ADR 0022).
func (c *calls) openDestination(volume *dockwellv1.ResourceRef, sealed []byte) (map[string]string, error) {
	if len(sealed) == 0 {
		return nil, nil
	}
	if c.delivery == nil {
		return nil, errors.New("this agent has no delivery key to open where a backup goes")
	}
	return c.delivery.Open(c.server, resourcepb.FromRef(volume), sealed)
}

// progressEvery is how often a call says how it is going, at most.
const progressEvery = time.Second

// progress returns what a call's drivers tell how it is going, which goes
// to the control plane at most once every progressEvery. Stop it once
// the call ends.
func (c *calls) progress(id uint64) *throttle {
	return newThrottle(progressEvery, func(message string) {
		_ = c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Progress{Progress: &agentv1.CallProgress{Id: id, Message: message}}})
	})
}

// throttle passes messages on at most once every interval. One that comes
// sooner isn't lost: the latest waits, and goes when the interval ends, so
// a change of phase, such as from pulling to starting, always shows.
type throttle struct {
	every time.Duration
	send  func(string)

	mu      sync.Mutex
	last    time.Time   // when the last message went
	waiting string      // the latest message to go next
	timer   *time.Timer // set while one waits
	stopped bool
}

func newThrottle(every time.Duration, send func(string)) *throttle {
	return &throttle{every: every, send: send}
}

// say passes a message on now, or once the interval ends.
func (t *throttle) say(message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch wait := t.every - time.Since(t.last); {
	case t.stopped:
	case t.timer != nil:
		t.waiting = message
	case wait > 0:
		t.waiting = message
		t.timer = time.AfterFunc(wait, t.flush)
	default:
		t.last = time.Now()
		t.send(message)
	}
}

func (t *throttle) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timer = nil
	if t.stopped {
		return
	}
	t.last = time.Now()
	t.send(t.waiting)
}

// stop drops what waits: the call's result says the last word, and must
// not be followed by its progress.
func (t *throttle) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

// decode reads a desired resource with the agent's drivers.
func (c *calls) decode(d *agentv1.DesiredResource) (resource.Ref, proto.Message, error) {
	ref := resourcepb.FromRef(d.GetResource())
	if d.Spec == nil {
		return ref, nil, nil
	}
	spec, err := c.cfg.Drivers.Decode(ref.Kind, d.GetSpec())
	return ref, spec, err
}
