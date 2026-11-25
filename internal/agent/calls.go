// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/gen/dockwell/agent/v1/agentv1connect"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/access"
	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/agent/system"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	containerkind "github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/fail2ban"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/sshconfig"
	volumekind "github.com/dockwell/dockwell/internal/resource/volume"
)

// errNoFiles answers a call for a server's files on an agent set up without them.
var errNoFiles = errors.New("this agent doesn't read files")

// errNoTerminals answers a call for a terminal on an agent set up without them.
var errNoTerminals = errors.New("this agent doesn't open terminals")

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
	unfinished     *unfinished
	inventory      inventoryWatches
	inventoryReads chan struct{}

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

	// transfers is what uploads and downloads go over: a connection of its own,
	// as logs have, kept for this stream's calls, so a file's next part doesn't wait
	// on a handshake. It goes with the stream, so a renewed certificate reaches it.
	transfers, artifactTransfers transferConnection
}

type transferConnection struct {
	sync.Mutex
	client  agentv1connect.AgentServiceClient
	release func()
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
		grace: grace, unfinished: held, inventoryReads: make(chan struct{}, 3),
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
	// An attachment lasts as long as someone watches, so it doesn't hold up a move
	// to a renewed certificate either: it stops, its terminal goes on, and the
	// control plane attaches again over the next stream.
	if attach := call.GetAttachTerminal(); attach != nil {
		ctx, cancel := context.WithCancel(c.ctx)
		c.reading[call.GetId()] = cancel
		c.wg.Add(1)
		c.mu.Unlock()
		go func() {
			defer c.wg.Done()
			defer c.doneReading(call.GetId(), cancel)
			c.reply(call.GetId(), c.attachTerminal(ctx, attach))
		}()
		return
	}
	// A forward lasts as long as its connection, as an attachment lasts as long as
	// someone watches, so it doesn't hold up a move either.
	if forward := call.GetForward(); forward != nil {
		ctx, cancel := context.WithCancel(c.ctx)
		c.reading[call.GetId()] = cancel
		c.wg.Add(1)
		c.mu.Unlock()
		go func() {
			defer c.wg.Done()
			defer c.doneReading(call.GetId(), cancel)
			c.reply(call.GetId(), c.forward(ctx, forward))
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
		take := (*files.Server).Take
		switch {
		case call.GetWriteUpload() != nil || call.GetDownloadFile() != nil:
			take = (*files.Server).TakeTransfer
		case call.GetReadFile().GetAllowBinary():
			take = (*files.Server).TakePreview
		}
		if c.cfg.Files != nil && (isFileCall(call) || call.GetWriteUpload() != nil || call.GetDownloadFile() != nil) {
			release, err := take(c.cfg.Files, ctx)
			if errors.Is(err, files.ErrBusy) {
				c.reply(call.GetId(), busy(call))
				return
			}
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

// attachTerminal carries a terminal's bytes over a connection of its own, as logs
// go, so a shell printing as fast as it can shares nothing with stats, plans and
// applies, and flow control on it holds up that shell alone. The connection goes
// with the attachment; the terminal stays.
func (c *calls) attachTerminal(ctx context.Context, call *agentv1.AttachTerminalCall) *agentv1.CallResult {
	fail := func(err error) *agentv1.CallResult {
		return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: err.Error(), Draining: c.isClosing() && c.ctx.Err() == nil}}}
	}
	if c.cfg.Terminals == nil {
		return fail(errNoTerminals)
	}
	client, release, err := c.cfg.Identity.Client()
	if err != nil {
		return fail(err)
	}
	defer release()
	// A send that a slow reader holds up ends only with the stream's context.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := client.Terminal(streamCtx)
	defer func() { _ = stream.CloseRequest(); _ = stream.CloseResponse() }()
	result, err := c.cfg.Terminals.Attach(ctx, call, stream)
	switch {
	case err != nil && ctx.Err() != nil && c.isClosing():
		return fail(errors.New("this agent is moving to a new stream, so it stopped carrying a terminal; attach again once the agent reconnects"))
	case err != nil && ctx.Err() == nil:
		return fail(err)
	case result == nil:
		result = &agentv1.AttachTerminalResult{}
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_AttachTerminal{AttachTerminal: result}}
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
	defer c.dropTransfers()
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

// callFailed is a call's result for err.
func callFailed(err error) *agentv1.CallResult {
	plain, detail := resource.Explain(err)
	if detail == "" {
		plain = ""
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{
		Message: err.Error(), Plain: plain, OutOfTime: errors.Is(err, context.DeadlineExceeded), Reason: resource.ReasonOf(err),
	}}}
}

// run carries out one call with the agent's drivers.
func (c *calls) run(ctx context.Context, call *agentv1.Call) *agentv1.CallResult {
	fail := callFailed
	switch {
	case call.GetPlan() != nil:
		if result, checked := c.planDockerMaintenance(ctx, call.GetPlan()); checked {
			return result
		}
		wanted := make([]resource.Desired, 0, len(call.GetPlan().GetDesired()))
		names := map[string]string{}
		for _, d := range call.GetPlan().GetDesired() {
			ref, spec, err := c.decode(d)
			if err != nil {
				return fail(err)
			}
			want := resource.Desired{Ref: ref, Spec: spec, Pull: d.GetPull(), Run: d.GetRun(), Restart: d.GetRestart(), ExactRestart: d.GetExactRestartId(), Rollback: d.GetRollback(), Adopt: d.GetAdopt()}
			if id := d.GetBackupId(); id != "" {
				want.Backup = &resource.BackupAsked{ID: id, Database: d.GetBackupDatabase()}
				for _, also := range d.GetBackupAlso() {
					want.Backup.Also = append(want.Backup.Also, resource.DatabaseDump{ID: also.GetBackupId(), Database: also.GetDatabase()})
				}
			}
			if change := d.GetReboot(); change != "" {
				want.Reboot = &resource.RebootAsked{Change: change}
			}
			if u := d.GetUnit(); u != nil {
				want.Unit = &resource.UnitAsked{Change: u.GetChangeId(), Name: u.GetName(), Action: resourcepb.UnitActionFrom(u.GetAction())}
			}
			if p := d.GetPanel(); p != nil {
				want.Panel = &resource.PanelAsked{Change: p.GetChangeId(), Kind: resourcepb.PanelKindFrom(p.GetPanel()), Start: p.GetStart(), Proxy: p.GetProxy(), Apps: p.GetApps(), Parts: resourcepb.PanelPartsFrom(p.GetParts()), Domains: p.GetDomains(), Evidence: p.GetEvidence()}
			}
			if u := d.GetUpgradeAgent(); u != nil {
				want.Upgrade = &resource.AgentUpgradeAsked{Change: u.GetChange(), Version: u.GetVersion(), ScriptSHA256: u.GetScriptSha256()}
			}
			if in := d.GetInstallUpdates(); in != nil {
				want.Install = &resource.InstallAsked{Change: in.GetChangeId(), All: in.GetAll(), Names: in.GetNames(), Packages: resourcepb.PackagesFrom(in.GetPackages())}
			}
			if r := d.GetRestore(); r != nil {
				want.Restore = &resource.RestoreSource{Backup: r.GetBackupId(), Undoes: r.GetUndoes(), Change: r.GetChangeId()}
				if r.GetFrom() != nil {
					want.Restore.From = resourcepb.FromRef(r.GetFrom())
				}
			}
			want.MoveVolumeFrom = d.GetMoveVolumeFrom()
			if m := d.GetMoveDatabase(); m != nil {
				want.Move = &resource.MoveAsked{Backup: m.GetBackupId(), From: m.GetFromVolume(), To: m.GetToVolume(), Change: m.GetChangeId(), Finish: m.GetFinishOnly()}
			}
			if sealed := d.GetRegistryAuth(); len(sealed) > 0 {
				prepare, err := c.signedIn(ctx, ref, sealed)
				if err != nil {
					return fail(err)
				}
				want.Prepare = prepare
			}
			wanted = append(wanted, want)
			if n := d.GetScopeName(); n != "" {
				names[ref.Scope] = n
			}
		}
		// The firewall's checks of this plan share one read of the server, its own
		// (#850's review, L3, and its re-check, N4).
		if p, ok := c.cfg.Access.(interface {
			Planning(ctx context.Context) context.Context
		}); ok {
			ctx = p.Planning(ctx)
		}
		steps, err := c.local.Plan(resource.WithScopeNames(ctx, names), wanted)
		if err != nil {
			return fail(err)
		}
		out := &agentv1.PlanResult{}
		for i := range steps {
			out.Steps = append(out.Steps, resourcepb.Step(steps[i], ""))
		}
		// What containers Dockwell didn't make mount, and the links on the way to each
		// path, are the server's, so once.
		for i := range steps {
			if len(steps[i].Alongside) > 0 {
				for _, u := range steps[i].Alongside {
					out.Unmanaged = append(out.Unmanaged, &agentv1.UnmanagedMounts{Name: u.Name, Paths: u.Paths, Root: u.Root})
				}
				break
			}
		}
		for i := range steps {
			if len(steps[i].Via) > 0 {
				for _, p := range slices.Sorted(maps.Keys(steps[i].Via)) {
					out.Via = append(out.Via, &agentv1.PathVia{Path: p, Links: steps[i].Via[p]})
				}
				break
			}
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Plan{Plan: out}}
	case call.GetApply() != nil:
		step, err := resourcepb.FromStep(call.GetApply().GetStep())
		if err != nil {
			return fail(fmt.Errorf("this agent can't read the step it was sent: %w. "+
				"The control plane is newer than the agent, so update the agent to the control plane's version", err))
		}
		if want := call.GetApply().GetDesired(); want.GetDockerMaintenance() != nil || step.DockerMaintenance != nil {
			if step.DockerMaintenance == nil || !proto.Equal(want.GetDockerMaintenance(), step.DockerMaintenance) ||
				resourcepb.FromRef(want.GetResource()) != step.Ref {
				return fail(errors.New("the Docker cleanup scope differs from its plan"))
			}
			if step.DockerMaintenance.GetImageAction() != nil {
				image := call.GetApply().GetDesired()
				if !proto.Equal(step.DockerMaintenance, image.GetDockerMaintenance()) {
					return fail(errors.New("the image action differs from its reviewed step"))
				}
				return c.runImageAction(ctx, call.GetId(), step.Ref, image, call.GetApply().GetDockerCleanupProtection())
			}
			return c.runDockerMaintenance(ctx, call.GetId(), step.Ref, step.DockerMaintenance, call.GetApply().GetDockerCleanupProtection())
		}
		ref, spec, err := c.decode(call.GetApply().GetDesired())
		if err != nil {
			return fail(err)
		}
		step.ExactRestart = call.GetApply().GetDesired().GetExactRestartId()
		step.Ref, step.Pulled, step.Finishing = ref, call.GetApply().GetPulled(), call.GetApply().GetFinishing()
		// A jail's step writes what the fail2ban read looks at, so the read asked next
		// starts again, written or put back: the Fail2ban card refetches as the plan
		// sheet closes, and would otherwise show the jail as it was for a minute.
		if ref.Kind == fail2ban.Kind {
			defer c.cfg.fail2ban.forget()
			if c.cfg.ssh != nil && c.cfg.ssh.access != nil {
				release, err := c.cfg.ssh.access.HoldSSHKeys()
				if err != nil {
					return fail(err)
				}
				defer release()
			}
		}
		if ref.Kind == sshconfig.Kind {
			defer c.cfg.fail2ban.forget()
		}
		step.PlannedLeads = call.GetApply().GetPlannedLeads()
		// A restore into a new database reads its backup from the volume the call
		// names (ADR 0106).
		if r := call.GetApply().GetDesired().GetRestore(); r != nil && r.GetFrom() != nil && step.Restore != nil && step.Restore.IntoNew {
			step.Restore.From = resourcepb.FromRef(r.GetFrom())
		}
		// An upgrade of the agent runs the script its call carries, which only an
		// apply does (ADR 0094).
		if u := call.GetApply().GetDesired().GetUpgradeAgent(); u != nil {
			ctx = resource.WithAgentUpgrade(ctx, resource.AgentUpgradeAsked{
				Change: u.GetChange(), Version: u.GetVersion(), ScriptSHA256: u.GetScriptSha256(), Script: u.GetScript(), Mirror: u.GetMirror(), CosignMirror: u.GetCosignMirror(),
			})
		}
		if n := call.GetApply().GetDesired().GetScopeName(); n != "" {
			ctx = resource.WithScopeNames(ctx, map[string]string{ref.Scope: n})
		}
		progress := c.progress(call.GetId())
		defer progress.stop()
		ctx = resource.WithActivities(ctx, progress.say)
		ctx = resource.WithOutcome(ctx, func(o resource.Outcome) {
			_ = c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Progress{Progress: &agentv1.CallProgress{
				Id: call.GetId(), Outcome: &agentv1.StepOutcome{DockerReceipt: o.DockerReceipts, LocalImageId: o.LocalImageID, Digest: o.Digest, Previous: o.Previous, Replacing: o.Replacing, Note: o.Note, Untouched: o.Untouched, Did: resourcepb.Did(o.Did), Backup: backupTaken(o.Backup), Waits: accessWait(o.Waits)},
			}}})
		})
		if d := call.GetApply().GetDeploy(); d != nil {
			ctx = resource.WithDeploy(ctx, resource.Deploy{
				Revision: int(d.GetRevision()), Change: d.GetChangeId(), Rollback: d.GetRollback(), LastAccess: d.GetLastAccessStep(),
			})
			ctx = resource.WithBuilds(ctx, c.buildUpdates(d.GetChangeId(), ref))
		}
		// The values of what the spec uses, opened in memory for this
		// step. They reach no log, and the disk only through Desired, which
		// seals them (ADR 0054).
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
		// The registry login the step's pull signs in with, for this step alone: it's
		// never kept with what the server runs.
		pullCtx := applyCtx
		if sealed := call.GetApply().GetDesired().GetRegistryAuth(); len(sealed) > 0 {
			prepare, err := c.signedIn(ctx, ref, sealed)
			if err != nil {
				return fail(err)
			}
			pullCtx = prepare(applyCtx)
		}
		if sealed := call.GetApply().GetDesired().GetBuildRegistryAuths(); len(sealed) > 0 {
			auths, err := c.buildLogins(ctx, ref, sealed)
			if err != nil {
				return fail(err)
			}
			pullCtx = registry.WithBuildRecheck(registry.WithBuildAuths(pullCtx, auths), c.checkBuildLogins)
		}
		// What fetches a build's private repository, for this step alone (ADR 0098).
		if sealed := call.GetApply().GetDesired().GetBuildGitAuth(); len(sealed) > 0 {
			header, err := c.buildGitAuth(ref, sealed)
			if err != nil {
				return fail(err)
			}
			pullCtx = resource.WithBuildGitAuth(pullCtx, header)
		}
		if guard := step.DockerStorageGuard.GetAttachment(); guard != nil {
			if err := c.checkDockerAttachmentApply(pullCtx, step, guard); err != nil {
				resource.Report(ctx, resource.Outcome{Untouched: true})
				return fail(err)
			}
			if !step.Finishing {
				pullCtx = containerkind.WithDockerAttachment(pullCtx, guard)
			}
		}
		if err := c.local.Apply(pullCtx, step, spec); err != nil {
			if step.Action == resource.ActionDelete && c.cfg.Desired != nil {
				// What's left of it is the control plane's to finish, not the agent's to
				// put back while it's away.
				said := c.cfg.Desired.Drop(ref, fmt.Errorf("deleting %s failed", ref))
				c.logger.Info("a delete step failed, so the agent doesn't put what's left of it back while the control plane is away",
					slog.String("resource", ref.String()), slog.String("how", said.Error()))
			}
			return fail(err)
		}
		// A kind under the firewall's timer is kept only once confirmed, which
		// the step can't say: the access manager keeps what was confirmed itself
		// (ADR 0052), so it isn't desired state until then.
		if c.cfg.Desired != nil && !c.guardsAccess(ref.Kind) && step.ExactRestart == "" {
			deploy := call.GetApply().GetDeploy()
			r := desired.Record{
				Ref: ref, ScopeName: call.GetApply().GetDesired().GetScopeName(), Revision: deploy.GetRevision(), Change: deploy.GetChangeId(),
				Values: resource.ValuesFrom(applyCtx),
				// A step that made it, made it again or restarted it started it, so
				// what was seen of it stopped is gone.
				Started: resource.RunIntent(spec) != resourcev1.DesiredRunState_DESIRED_RUN_STATE_STOPPED &&
					(step.Action == resource.ActionCreate || step.Action == resource.ActionReplace || step.Effect != resource.EffectNone || step.ChangesRunIntent()),
			}
			if err := c.keep(ctx, r, spec); err != nil {
				c.logger.Warn("applied a step, but can't keep it as what this server runs, so the server can't put it back while the control plane is away. "+
					"The control plane sends it again when the agent next connects. Check that the agent's data folder is writable",
					slog.String("resource", ref.String()), slog.String("error", err.Error()))
				return &agentv1.CallResult{Result: &agentv1.CallResult_Apply{Apply: &agentv1.ApplyResult{Unkept: true}}}
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
	case call.GetDockerSettings() != nil:
		return c.dockerSettings(ctx, call.GetDockerSettings(), fail)
	case call.GetDockerInventoryList() != nil:
		select {
		case c.inventoryReads <- struct{}{}:
			defer func() { <-c.inventoryReads }()
		case <-ctx.Done():
			return fail(errDockerInventory)
		}
		ask := call.GetDockerInventoryList()
		var out *agentv1.DockerInventoryListResult
		var err error
		switch ask.GetKind() {
		case dockerNetworkKind:
			out, err = c.dockerNetworkList(ctx, ask)
		case dockerVolumeKind:
			out, err = c.dockerVolumeList(ctx, ask)
		case dockerImageKind:
			out, err = c.dockerImageList(ctx, ask)
		default:
			out, err = dockerInventoryList(ctx, c.cfg.Docker, ask)
			if summary := out.GetOverview(); summary != nil && c.cfg.artifacts != nil && c.cfg.images != nil {
				_, supported := c.cfg.Builds.(service.Builder)
				summary.ImageOperations = &dockwellv1.DockerImageSupport{CanBuild: supported, CanPush: true}
			}
		}
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_DockerInventoryList{DockerInventoryList: out}}
	case call.GetDockerInventoryDetail() != nil:
		select {
		case c.inventoryReads <- struct{}{}:
			defer func() { <-c.inventoryReads }()
		case <-ctx.Done():
			return fail(errDockerInventory)
		}
		ask := call.GetDockerInventoryDetail()
		var out *agentv1.DockerInventoryDetailResult
		var err error
		switch {
		case ask.GetVolumeKey() != "":
			out, err = c.dockerVolumeDetail(ctx, ask.GetVolumeKey())
		case ask.GetNetworkId() != "":
			out, err = c.dockerNetworkDetail(ctx, ask.GetNetworkId())
		case ask.GetImageId() != "":
			out, err = c.dockerImageDetail(ctx, ask.GetImageId())
		default:
			out, err = dockerInventoryDetail(ctx, c.cfg.Docker, ask)
			if err == nil {
				c.volumeLinks(ctx, out.GetObject().GetContainer())
			}
		}
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_DockerInventoryDetail{DockerInventoryDetail: out}}
	case call.GetCheckDockerNetwork() != nil:
		select {
		case c.inventoryReads <- struct{}{}:
			defer func() { <-c.inventoryReads }()
		case <-ctx.Done():
			return fail(errDockerInventory)
		}
		out, err := c.checkDockerNetwork(ctx, call.GetCheckDockerNetwork())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_CheckDockerNetwork{CheckDockerNetwork: out}}
	case call.GetDockerArtifactControl() != nil:
		return c.dockerArtifact(ctx, call.GetDockerArtifactControl())
	case call.GetDockerMaintenancePreview() != nil:
		out, err := c.dockerCleanupPreview(ctx, call.GetDockerMaintenancePreview())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_DockerMaintenancePreview{DockerMaintenancePreview: out}}
	case call.GetDockerInventoryWatch() != nil:
		if err := c.watchInventory(call.GetDockerInventoryWatch()); err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_DockerInventoryWatch{DockerInventoryWatch: &agentv1.DockerInventoryWatchResult{}}}
	case call.GetContainers() != nil:
		found, err := containers(ctx, c.cfg.Docker, call.GetContainers())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Containers{Containers: found}}
	case call.GetFirewall() != nil:
		read, err := c.cfg.firewall.read(ctx)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Firewall{Firewall: read}}
	case call.GetDiskUse() != nil:
		found, err := c.dockerDiskUse(ctx, call.GetDiskUse().GetDockerCleanupProtection())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_DiskUse{DiskUse: found}}
	case call.GetSystemServices() != nil:
		if c.cfg.System == nil {
			return fail(errors.New("this agent doesn't read system services"))
		}
		list, err := c.cfg.System.Services(ctx)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_SystemServices{SystemServices: &agentv1.SystemServicesResult{List: list}}}
	case call.GetSystemLog() != nil:
		if c.cfg.System == nil {
			return fail(errors.New("this agent doesn't read the system log"))
		}
		ask := call.GetSystemLog()
		want := system.Ask{Unit: ask.GetUnit(), Level: ask.GetLevel(), Before: ask.GetBefore(), Limit: int(ask.GetLimit()), Query: ask.GetQuery()}
		if t := ask.GetSince(); t != nil {
			want.Since = t.AsTime()
		}
		if t := ask.GetUntil(); t != nil {
			want.Until = t.AsTime()
		}
		page, err := c.cfg.System.Journal(ctx, want)
		if err != nil {
			return fail(err)
		}
		res := &agentv1.SystemLogResult{Lines: page.Lines, More: page.More, Source: page.Source, CutShort: page.CutShort}
		if !page.SearchedTo.IsZero() {
			res.SearchedTo = timestamppb.New(page.SearchedTo)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_SystemLog{SystemLog: res}}
	case call.GetProcesses() != nil:
		list, err := c.cfg.procs.list(ctx)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Processes{Processes: &agentv1.ProcessesResult{List: list}}}
	case call.GetEndProcess() != nil:
		end := call.GetEndProcess()
		was, killed, err := c.cfg.procs.end(ctx, end.GetPid(), end.GetStartTicks())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_EndProcess{EndProcess: &agentv1.EndProcessResult{Killed: killed, Process: was}}}
	case call.GetListSshKeys() != nil:
		result, err := c.cfg.ssh.listSSHKeys(ctx, call.GetListSshKeys())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ListSshKeys{ListSshKeys: result}}
	case call.GetEditSshKey() != nil:
		result, err := c.cfg.ssh.editSSHKey(ctx, call.GetEditSshKey())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_EditSshKey{EditSshKey: result}}
	case call.GetListSshSessions() != nil:
		result, err := c.cfg.ssh.listSSHSessions(ctx)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ListSshSessions{ListSshSessions: result}}
	case call.GetDisconnectSshSession() != nil:
		err := c.cfg.ssh.disconnectSSH(ctx, call.GetDisconnectSshSession().GetSessionId())
		result := &dockwellv1.DisconnectServerSshSessionResponse{}
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_DisconnectSshSession{DisconnectSshSession: result}}
	case call.GetListSshLogins() != nil:
		result, err := c.cfg.ssh.loginLog(ctx, call.GetListSshLogins())
		if result != nil && !result.Unverified && len(result.Failures) > 0 && !c.cfg.ssh.panelOverSSH {
			result.BanAvailable = c.cfg.ssh.sshJailCanBan(ctx, c.cfg.fail2ban) == nil
		}
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ListSshLogins{ListSshLogins: result}}
	case call.GetBanSshAddress() != nil:
		err := c.cfg.ssh.banSSH(ctx, c.cfg.fail2ban, call.GetBanSshAddress().GetAddress())
		result := &dockwellv1.BanServerSshAddressResponse{}
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_BanSshAddress{BanSshAddress: result}}
	case call.GetListSockets() != nil:
		res, err := c.cfg.procs.sockets(ctx)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ListSockets{ListSockets: &agentv1.ListSocketsResult{
			Time: res.GetTime(), Connections: res.GetConnections(), Total: res.GetTotal(), Truncated: res.GetTruncated(),
		}}}
	case call.GetProcessDetail() != nil:
		ask := call.GetProcessDetail()
		detail, err := c.cfg.procs.detail(ctx, ask.GetPid(), ask.GetStartTicks())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ProcessDetail{ProcessDetail: &agentv1.ProcessDetailResult{Detail: detail}}}
	case call.GetSsh() != nil:
		read, err := c.cfg.ssh.read(ctx)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Ssh{Ssh: c.cfg.ssh.editDetails(ctx, read)}}
	case call.GetSetFail2BanAddressBan() != nil:
		if err := c.cfg.fail2ban.setBan(ctx, call.GetSetFail2BanAddressBan()); err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_SetFail2BanAddressBan{SetFail2BanAddressBan: &dockwellv1.SetFail2BanAddressBanResponse{}}}
	case call.GetFail2Ban() != nil:
		read, err := c.cfg.fail2ban.read(ctx)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Fail2Ban{Fail2Ban: read}}
	case call.GetRemember() != nil:
		kept, err := c.remember(ctx, call.GetRemember())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Remember{Remember: &agentv1.RememberResult{Kept: kept}}}
	case call.GetAdoption() != nil:
		found, err := c.adoption(ctx, call.GetAdoption())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Adoption{Adoption: found}}
	case call.GetDatabase() != nil:
		if c.cfg.Databases == nil {
			return fail(errors.New("this agent can't read databases, as it has no Docker driver"))
		}
		read, err := c.cfg.Databases.Read(ctx, call.GetDatabase())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Database{Database: read}}
	case call.GetCancelBuild() != nil:
		cb := call.GetCancelBuild()
		stopping := false
		if c.cfg.Builds != nil {
			var err error
			stopping, err = c.cfg.Builds.CancelWithStatus(cb.GetChangeId(), resourcepb.FromRef(cb.GetResource()), cb.GetBy())
			if err != nil {
				return fail(err)
			}
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_CancelBuild{CancelBuild: &agentv1.CancelBuildResult{Stopping: stopping}}}
	case call.GetDetectBuild() != nil:
		if c.cfg.Builds == nil {
			return fail(errors.New("this agent can't read how a commit builds, as it has no Docker"))
		}
		b := call.GetDetectBuild().GetBuild()
		if b.GetGit().GetCommit() == "" {
			return fail(errors.New("reading how a commit builds needs the commit, in full"))
		}
		req := service.Request(&resourcev1.Service{Build: b})
		req.OwnCommand = call.GetDetectBuild().GetOwnCommand()
		// A private repository's reading fetches it as its build would (ADR 0098).
		if sealed := call.GetDetectBuild().GetGitAuth(); len(sealed) > 0 {
			header, err := c.openGitAuth(resource.DetectGitRef, sealed)
			if err != nil {
				return fail(fmt.Errorf("can't open what fetches the repository: %w", err))
			}
			req.GitAuth = header
		}
		d, err := c.cfg.Builds.Detect(ctx, req)
		if err != nil {
			plain, _ := resource.Explain(err)
			return fail(errors.New(plain))
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_DetectBuild{DetectBuild: &agentv1.DetectBuildResult{Detection: resourcepb.Detection(d)}}}
	case call.GetTaskRuns() != nil, call.GetRunTask() != nil:
		if c.cfg.Tasks == nil {
			return fail(errors.New("this agent runs no scheduled tasks. Update it to the control plane's version"))
		}
		if r := call.GetRunTask(); r != nil {
			run, err := c.cfg.Tasks.RunNow(ctx, resourcepb.FromRef(r.GetTask()))
			if err != nil {
				return fail(err)
			}
			return &agentv1.CallResult{Result: &agentv1.CallResult_RunTask{RunTask: &agentv1.RunTaskResult{Run: run}}}
		}
		runs, err := c.cfg.Tasks.Runs(call.GetTaskRuns().GetAfter())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_TaskRuns{TaskRuns: runs}}
	case call.GetBackupPreview() != nil, call.GetBackup() != nil, call.GetDeleteBackup() != nil, call.GetFetchBackup() != nil, call.GetSendBackup() != nil, call.GetDrillBackup() != nil:
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
		listing, err := c.cfg.Files.List(ctx, list.GetPath(), list.GetMatch(), list.GetPage())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ListFiles{ListFiles: listing}}
	}
	if read := call.GetReadFile(); read != nil {
		if c.cfg.Files == nil {
			return fail(errNoFiles)
		}
		file, err := c.cfg.Files.ReadWithOptions(ctx, read.GetPath(), read.GetAllowBinary())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ReadFile{ReadFile: file}}
	}
	if open := call.GetOpenTerminal(); open != nil {
		if c.cfg.Terminals == nil {
			return fail(errNoTerminals)
		}
		opened, err := c.cfg.Terminals.Open(ctx, open)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_OpenTerminal{OpenTerminal: opened}}
	}
	if closing := call.GetCloseTerminal(); closing != nil {
		if c.cfg.Terminals == nil {
			return fail(errNoTerminals)
		}
		end, err := c.cfg.Terminals.Close(closing.GetId(), closing.GetReason())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_CloseTerminal{CloseTerminal: &agentv1.CloseTerminalResult{End: end}}}
	}
	if cwd := call.GetTerminalCwd(); cwd != nil {
		if c.cfg.Terminals == nil {
			return fail(errNoTerminals)
		}
		res, err := c.cfg.Terminals.WorkingDirectory(cwd.GetId())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_TerminalCwd{TerminalCwd: res}}
	}
	if confirm := call.GetConfirmAccess(); confirm != nil {
		if c.cfg.Access == nil {
			return &agentv1.CallResult{Result: &agentv1.CallResult_ConfirmAccess{ConfirmAccess: &agentv1.ConfirmAccessResult{Verdict: agentv1.AccessVerdict_ACCESS_VERDICT_NOT_WAITING}}}
		}
		var v resource.AccessVerdict
		var failure string
		if ssh, ok := c.cfg.Access.(*access.Manager); ok {
			v, failure = ssh.ConfirmDetails(ctx, confirm.GetChangeId())
		} else {
			v = c.cfg.Access.Confirm(ctx, confirm.GetChangeId())
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ConfirmAccess{ConfirmAccess: &agentv1.ConfirmAccessResult{Verdict: accessVerdicts[v], SshFailure: failure}}}
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
	if list := call.GetListUpdates(); list != nil {
		if c.cfg.Updates == nil {
			return fail(errors.New("this agent doesn't read package updates"))
		}
		found, err := c.cfg.Updates.Read(ctx, list.GetRefresh(), list.GetForce())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ListUpdates{ListUpdates: &agentv1.ListUpdatesResult{Updates: found}}}
	}
	if p := call.GetProbe(); p != nil {
		probe := c.cfg.Probe
		if probe == nil {
			probe = Probe
		}
		found := probe(ctx, p)
		if found == nil {
			return fail(errors.New("this server asks nothing beyond itself, so it didn't ask"))
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Probe{Probe: found}}
	}
	if p := call.GetPorts(); p != nil {
		if c.cfg.Docker == nil {
			return fail(errNoPorts)
		}
		held, err := portsHeld(ctx, c.cfg.ports, p.GetPorts())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Ports{Ports: held}}
	}
	if call.GetImage() != nil {
		ctx, err := c.readsAs(ctx, call.GetImage().GetRegistryAuth())
		if err != nil {
			return fail(err)
		}
		read, err := c.cfg.images.read(ctx, call.GetImage())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Image{Image: read}}
	}
	if call.GetImageUpdates() != nil {
		ctx, err := c.readsAs(ctx, call.GetImageUpdates().GetRegistryAuth())
		if err != nil {
			return fail(err)
		}
		found, err := c.cfg.images.imageUpdates(ctx, call.GetImageUpdates())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ImageUpdates{ImageUpdates: found}}
	}
	if check := call.GetCheckDatabaseSignIn(); check != nil {
		if c.cfg.Databases == nil {
			return fail(errors.New("this agent can't check a sign-in, as it has no Docker driver"))
		}
		if c.delivery == nil {
			return fail(errors.New("this agent has no delivery key to open the password sent with this call"))
		}
		values, err := c.delivery.Open(c.server, resource.DatabaseSignInRef(check.GetEnvironment(), check.GetUser()), check.GetPassword())
		if err != nil {
			return fail(err)
		}
		password := values["password"]
		clear(values)
		said, err := c.cfg.Databases.CheckSignIn(ctx, check, password)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_CheckDatabaseSignIn{CheckDatabaseSignIn: said}}
	}
	if check := call.GetCheckRegistryLogin(); check != nil {
		if host := check.GetProbe(); host != "" {
			return &agentv1.CallResult{Result: &agentv1.CallResult_CheckRegistryLogin{CheckRegistryLogin: c.cfg.images.probeRegistry(ctx, host)}}
		}
		a, err := c.openLogin(resource.Ref{}, check.GetRegistryAuth())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_CheckRegistryLogin{CheckRegistryLogin: c.cfg.images.checkLogin(ctx, a, check.GetFirst())}}
	}
	if call.GetReadTunnels() != nil {
		if c.cfg.Tunnels == nil {
			return fail(errors.New("this agent doesn't read Cloudflare tunnels"))
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ReadTunnels{ReadTunnels: &agentv1.ReadTunnelsResult{Tunnels: c.cfg.Tunnels.Find(ctx)}}}
	}
	if check := call.GetProbeConnection(); check != nil {
		found, err := c.probeConnection(ctx, check)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ProbeConnection{ProbeConnection: found}}
	}
	if ask := call.GetMakeAppOwner(); ask != nil {
		made, err := c.makeAppOwner(ctx, ask)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_MakeAppOwner{MakeAppOwner: made}}
	}
	if ask := call.GetAppPorts(); ask != nil {
		got, err := c.appPorts(ctx, ask)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_AppPorts{AppPorts: got}}
	}
	if read := call.GetReadAppOwner(); read != nil {
		got, err := c.readAppOwner(ctx, read)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ReadAppOwner{ReadAppOwner: got}}
	}
	if read := call.GetReadPanel(); read != nil {
		if c.cfg.Panels == nil {
			return fail(errors.New("this agent doesn't read what Coolify or Dokploy runs"))
		}
		found, err := c.cfg.Panels(ctx, read.GetPanel(), read.GetCheckDomains())
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ReadPanel{ReadPanel: found}}
	}
	if put := call.GetPutCertificates(); put != nil {
		return c.putCertificates(ctx, put)
	}
	if call.GetReadOnepanel() != nil {
		if c.cfg.OnePanel == nil {
			return fail(errors.New("this agent doesn't read 1Panel's records"))
		}
		found, err := c.cfg.OnePanel(ctx)
		if err != nil {
			return fail(err)
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_ReadOnepanel{ReadOnepanel: found}}
	}
	if w := call.GetWriteUpload(); w != nil {
		if c.cfg.Files == nil {
			return fail(errNoFiles)
		}
		return c.writeUpload(ctx, w)
	}
	if d := call.GetDownloadFile(); d != nil {
		if c.cfg.Files == nil {
			return fail(errNoFiles)
		}
		return c.download(ctx, d)
	}
	if isFileCall(call) || isArchiveJobCall(call) {
		if c.cfg.Files == nil {
			return fail(errNoFiles)
		}
		result, err := manageFiles(ctx, c.cfg.Files, call)
		if err != nil {
			return fail(err)
		}
		return result
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
		out, err := c.cfg.Backups.Take(resource.WithActivities(ctx, progress.say), call.GetBackup())
		if err != nil {
			return nil, err
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_Backup{Backup: out}}, nil
	case call.GetDrillBackup() != nil:
		progress := c.progress(call.GetId())
		defer progress.stop()
		out, err := c.cfg.Backups.Drill(resource.WithActivities(ctx, progress.say), call.GetDrillBackup())
		if err != nil {
			return nil, err
		}
		return &agentv1.CallResult{Result: &agentv1.CallResult_DrillBackup{DrillBackup: out}}, nil
	case call.GetSendBackup() != nil:
		send := call.GetSendBackup()
		destination, err := c.openDestination(send.GetVolume(), send.GetDestination())
		if err != nil {
			return nil, err
		}
		defer clear(destination)
		progress := c.progress(call.GetId())
		defer progress.stop()
		out, err := c.cfg.Backups.Send(resource.WithActivities(ctx, progress.say), send, destination)
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
		out, err := c.cfg.Backups.Fetch(resource.WithActivities(ctx, progress.say), f, destination)
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
	return &agentv1.BackupResult{Id: b.ID, Method: resourcepb.BackupMethod(b.Method), Database: b.Database, Sha256: b.SHA256, SizeBytes: b.Size, FilesChanged: b.Changed, OneDatabase: b.One}
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
	return newThrottle(progressEvery, func(r resource.Activity) {
		_ = c.send(&agentv1.ConnectRequest{Message: &agentv1.ConnectRequest_Progress{Progress: &agentv1.CallProgress{
			Id: id, Message: r.Message, Phase: dockwellv1.StepPhase(r.Phase), DoneBytes: r.Done, TotalBytes: r.Total,
		}}})
	})
}

// throttle passes messages on at most once every interval. One that comes
// sooner isn't lost: the latest waits, and goes when the interval ends. A new
// phase, such as from pulling to starting, goes at once, after what waited, so
// each phase a step reaches shows, however soon the step ends (ADR 0080).
type throttle struct {
	every time.Duration
	send  func(resource.Activity)

	mu      sync.Mutex
	last    time.Time         // when the last message went
	phase   resource.Phase    // the last phase that went
	waiting resource.Activity // the latest message to go next
	timer   *time.Timer       // set while one waits
	stopped bool
}

func newThrottle(every time.Duration, send func(resource.Activity)) *throttle {
	return &throttle{every: every, send: send}
}

// say passes a message on now, or once the interval ends.
func (t *throttle) say(message resource.Activity) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch wait := t.every - time.Since(t.last); {
	case t.stopped:
	case message.Phase != resource.PhaseNone && message.Phase != t.phase:
		if t.timer != nil {
			t.timer.Stop()
			t.timer = nil
			t.pass(t.waiting)
		}
		t.pass(message)
	case t.timer != nil:
		t.waiting = message
	case wait > 0:
		t.waiting = message
		t.timer = time.AfterFunc(wait, t.flush)
	default:
		t.pass(message)
	}
}

// pass sends a message now. Hold mu.
func (t *throttle) pass(message resource.Activity) {
	t.last = time.Now()
	if message.Phase != resource.PhaseNone {
		t.phase = message.Phase
	}
	t.send(message)
}

func (t *throttle) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timer = nil
	if t.stopped {
		return
	}
	t.pass(t.waiting)
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

// record is r with what spec leaves its resource as, and the image it runs; a nil
// spec leaves r's spec nil.
func (c *calls) record(ctx context.Context, r desired.Record, spec proto.Message) (desired.Record, error) {
	if spec == nil {
		return r, nil
	}
	b, err := resource.Encode(volumekind.WithoutOptions(spec))
	if err != nil {
		return r, err
	}
	// An empty spec is set: only nil is a deletion.
	r.Spec = append([]byte{}, b...)
	if r.Image, err = c.local.RunsImage(ctx, r.Ref); err != nil {
		return r, fmt.Errorf("can't tell which image %s runs: %w", r.Ref, err)
	}
	return r, nil
}

// keep has Desired keep what a step that passed left r's resource as, or a
// tombstone when spec is nil. What can't be kept is forgotten, as it no longer
// says what runs, and the error says so.
func (c *calls) keep(ctx context.Context, r desired.Record, spec proto.Message) error {
	r, err := c.record(ctx, r, spec)
	if err != nil {
		return c.cfg.Desired.Drop(r.Ref, err)
	}
	return c.cfg.Desired.Keep(r)
}

// remember keeps what the control plane says a resource on this server runs, or,
// for one with no spec, forgets it, and says whether it did: it doesn't over a
// later revision or a deletion, nor forget what a step changed since.
func (c *calls) remember(ctx context.Context, call *agentv1.RememberCall) (bool, error) {
	if c.cfg.Desired == nil {
		return false, errors.New("this agent keeps nothing of what its server should run")
	}
	ref, spec, err := c.decode(call.GetDesired())
	if err != nil {
		return false, err
	}
	revision := call.GetDeploy().GetRevision()
	if revision <= 0 {
		return false, fmt.Errorf("there's no revision to keep or forget %s at", ref)
	}
	if spec == nil {
		return c.cfg.Desired.Forget(ref, revision)
	}
	if c.guardsAccess(ref.Kind) {
		// Under the firewall's timer: the access manager keeps what was
		// confirmed (ADR 0052), so it isn't desired state here.
		return false, nil
	}
	scopeName := call.GetDesired().GetScopeName()
	if scopeName != "" {
		ctx = resource.WithScopeNames(ctx, map[string]string{ref.Scope: scopeName})
	}
	r := desired.Record{Ref: ref, ScopeName: scopeName, Revision: revision, Change: call.GetDeploy().GetChangeId()}
	if sealed := call.GetValues(); len(sealed) > 0 {
		if c.delivery == nil {
			return false, fmt.Errorf("this agent has no delivery key to open the values sent for %s", ref)
		}
		values, err := c.delivery.Open(c.server, ref, sealed)
		if err != nil {
			return false, err
		}
		defer clear(values)
		r.Values = values
	}
	if r, err = c.record(ctx, r, spec); err != nil {
		return false, err
	}
	// A container that's gone runs no image to read, so the record takes what
	// the control plane kept, for a pull by digest.
	if r.Image == (resource.Image{}) {
		r.Image.Digest = call.GetImageDigest()
	}
	return c.cfg.Desired.Remember(r)
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

// busy is a transfer's answer when the agent has no place for it.
func busy(call *agentv1.Call) *agentv1.CallResult {
	if call.GetDownloadFile() != nil {
		return &agentv1.CallResult{Result: &agentv1.CallResult_DownloadFile{DownloadFile: &agentv1.DownloadFileResult{Refusal: files.Busy()}}}
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_WriteUpload{WriteUpload: &agentv1.UploadResult{Refusal: files.Busy()}}}
}

// isFileCall reports a call about the server's files, which takes one of their places.
// CompressFiles and ExtractArchive use their own dedicated archive job pool so
// long archive jobs never occupy the 4 listing and read slots.
func isFileCall(call *agentv1.Call) bool {
	return call.GetListFiles() != nil || call.GetReadFile() != nil || call.GetSaveFile() != nil || call.GetMakeFile() != nil ||
		call.GetMoveFile() != nil || call.GetDeleteFile() != nil || call.GetListDeleted() != nil || call.GetRestoreFile() != nil ||
		call.GetPurgeDeleted() != nil || call.GetBeginUpload() != nil || call.GetFinishUpload() != nil
}

// isArchiveJobCall reports a call that starts or queries a background archive job.
func isArchiveJobCall(call *agentv1.Call) bool {
	return call.GetCompressFiles() != nil || call.GetExtractArchive() != nil ||
		call.GetGetArchiveJob() != nil || call.GetListArchiveJobs() != nil
}

// manageFiles makes, moves, deletes and puts back files, and says what's in Recently
// deleted (ADR 0045).
func manageFiles(ctx context.Context, f *files.Server, call *agentv1.Call) (*agentv1.CallResult, error) {
	switch {
	case call.GetMakeFile() != nil:
		m := call.GetMakeFile()
		out, err := f.Make(ctx, m.GetPath(), m.GetType(), m.GetConfirmedPath())
		return &agentv1.CallResult{Result: &agentv1.CallResult_MakeFile{MakeFile: out}}, err
	case call.GetMoveFile() != nil:
		m := call.GetMoveFile()
		out, err := f.Move(ctx, m.GetFrom(), m.GetTo(), m.GetConfirmedFrom(), m.GetConfirmedTo())
		return &agentv1.CallResult{Result: &agentv1.CallResult_MoveFile{MoveFile: out}}, err
	case call.GetDeleteFile() != nil:
		d := call.GetDeleteFile()
		out, err := f.Delete(ctx, d.GetPath(), d.GetForGood(), d.GetConfirmedPath())
		return &agentv1.CallResult{Result: &agentv1.CallResult_DeleteFile{DeleteFile: out}}, err
	case call.GetListDeleted() != nil:
		out, err := f.ListDeleted()
		return &agentv1.CallResult{Result: &agentv1.CallResult_ListDeleted{ListDeleted: out}}, err
	case call.GetRestoreFile() != nil:
		r := call.GetRestoreFile()
		out, err := f.Restore(ctx, r.GetId(), r.GetConfirmedPath())
		return &agentv1.CallResult{Result: &agentv1.CallResult_RestoreFile{RestoreFile: out}}, err
	case call.GetPurgeDeleted() != nil:
		out, err := f.Purge(call.GetPurgeDeleted().GetId())
		return &agentv1.CallResult{Result: &agentv1.CallResult_PurgeDeleted{PurgeDeleted: out}}, err
	case call.GetBeginUpload() != nil:
		b := call.GetBeginUpload()
		out, err := f.BeginUpload(ctx, b.GetPath(), b.GetSizeBytes(), b.GetUnpack(), b.GetUploadId(), b.GetConfirmedPath(), b.GetOwner())
		return &agentv1.CallResult{Result: &agentv1.CallResult_BeginUpload{BeginUpload: out}}, err
	case call.GetFinishUpload() != nil:
		fin := call.GetFinishUpload()
		finish := f.FinishUpload
		if fin.GetCancel() {
			finish = f.CancelUpload
		}
		out, err := finish(ctx, fin.GetUploadId(), fin.GetOwner())
		return &agentv1.CallResult{Result: &agentv1.CallResult_FinishUpload{FinishUpload: out}}, err
	case call.GetCompressFiles() != nil:
		cf := call.GetCompressFiles()
		out, err := f.CompressWithRecord(ctx, cf.GetPaths(), cf.GetArchivePath(), cf.GetConfirmedPath(), cf.GetOwner(), cf.GetJobId(), cf.GetRecord())
		return &agentv1.CallResult{Result: &agentv1.CallResult_CompressFiles{CompressFiles: out}}, err
	case call.GetExtractArchive() != nil:
		ex := call.GetExtractArchive()
		out, err := f.ExtractWithRecord(ctx, ex.GetPath(), ex.GetConfirmedArchive(), ex.GetConfirmedPath(), ex.GetOwner(), ex.GetJobId(), ex.GetRecord())
		return &agentv1.CallResult{Result: &agentv1.CallResult_ExtractArchive{ExtractArchive: out}}, err
	case call.GetGetArchiveJob() != nil:
		gj := call.GetGetArchiveJob()
		out, err := f.GetArchiveJob(gj.GetJobId(), gj.GetOwner())
		return &agentv1.CallResult{Result: &agentv1.CallResult_GetArchiveJob{GetArchiveJob: out}}, err
	case call.GetListArchiveJobs() != nil:
		lj := call.GetListArchiveJobs()
		out, err := f.ListArchiveJobs(lj.GetOwner())
		return &agentv1.CallResult{Result: &agentv1.CallResult_ListArchiveJobs{ListArchiveJobs: out}}, err
	}
	return nil, errors.New("this isn't a call about files")
}

// transferClient is the client uploads and downloads go over, made the first time
// one asks.
func (c *calls) transferClient(artifact bool) (agentv1connect.AgentServiceClient, error) {
	conn := &c.transfers
	if artifact {
		conn = &c.artifactTransfers
	}
	conn.Lock()
	defer conn.Unlock()
	if conn.client == nil {
		client, release, err := c.cfg.Identity.TransferClient(artifact)
		if err != nil {
			return nil, err
		}
		conn.client, conn.release = client, release
	}
	return conn.client, nil
}

// dropTransfers lets the transfers' connections go once the stream's calls end.
func (c *calls) dropTransfers() {
	for _, conn := range []*transferConnection{&c.transfers, &c.artifactTransfers} {
		conn.Lock()
		if conn.release != nil {
			conn.release()
		}
		conn.client, conn.release = nil, nil
		conn.Unlock()
	}
}

// writeUpload writes an upload's bytes as ReceiveFile brings them, over the
// transfers' connection: what's sent waits on flow control, so the agent holds a
// part at a time, and its stream goes on.
func (c *calls) writeUpload(ctx context.Context, call *agentv1.WriteUploadCall) *agentv1.CallResult {
	fail := func(err error) *agentv1.CallResult {
		return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: err.Error()}}}
	}
	client, err := c.transferClient(false)
	if err != nil {
		return fail(err)
	}
	stream, err := client.ReceiveFile(ctx, connect.NewRequest(&agentv1.ReceiveFileRequest{Stream: call.GetStream()}))
	if err != nil {
		return fail(err)
	}
	defer stream.Close()
	next := func() ([]byte, error) {
		if stream.Receive() {
			return stream.Msg().GetData(), nil
		}
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	out, err := c.cfg.Files.WriteUpload(ctx, call.GetUploadId(), call.GetOwner(), call.GetOffset(), next)
	if err != nil {
		return fail(err)
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_WriteUpload{WriteUpload: out}}
}

// download sends a file, or a folder as an archive, over SendFile, on the
// transfers' connection.
func (c *calls) download(ctx context.Context, call *agentv1.DownloadFileCall) *agentv1.CallResult {
	fail := func(err error) *agentv1.CallResult {
		return &agentv1.CallResult{Result: &agentv1.CallResult_Error{Error: &agentv1.CallError{Message: err.Error()}}}
	}
	client, err := c.transferClient(false)
	if err != nil {
		return fail(err)
	}
	// A reader that stopped taking parts leaves a send blocked, which only ending
	// the stream's context ends.
	sendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := client.SendFile(sendCtx)
	send := func(m *agentv1.SendFileRequest) error {
		m.Stream = call.GetStream()
		return stream.Send(m)
	}
	var out *agentv1.DownloadFileResult
	if len(call.GetPaths()) > 0 {
		out, err = c.cfg.Files.DownloadMany(ctx, call.GetPaths(), send)
	} else {
		out, err = c.cfg.Files.Download(ctx, call.GetPath(), send)
	}
	if err != nil {
		cancel()
		return fail(err)
	}
	if out.GetRefusal() != nil {
		cancel()
	} else if _, err := stream.CloseAndReceive(); err != nil && !errors.Is(err, io.EOF) {
		return fail(err)
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_DownloadFile{DownloadFile: out}}
}

// accessVerdicts are the verdicts as the agent protocol says them.
var accessVerdicts = map[resource.AccessVerdict]agentv1.AccessVerdict{
	resource.AccessKept:       agentv1.AccessVerdict_ACCESS_VERDICT_KEPT,
	resource.AccessRolledBack: agentv1.AccessVerdict_ACCESS_VERDICT_ROLLED_BACK,
	resource.AccessNotWaiting: agentv1.AccessVerdict_ACCESS_VERDICT_NOT_WAITING,
	resource.AccessNotKept:    agentv1.AccessVerdict_ACCESS_VERDICT_NOT_KEPT,
	resource.AccessNotProved:  agentv1.AccessVerdict_ACCESS_VERDICT_NOT_PROVED,
}

// accessWait is what a step waits for, as the agent protocol says it.
func accessWait(w *resource.AccessWait) *agentv1.AccessWait {
	if w == nil {
		return nil
	}
	return &agentv1.AccessWait{Deadline: timestamppb.New(w.Deadline), SshPorts: w.SSHPorts, SshAccount: w.SSHAccount, Left: durationpb.New(w.Left)}
}

// guardsAccess says whether a kind applies under the firewall's timer.
func (c *calls) guardsAccess(kind resource.Kind) bool {
	t, err := c.cfg.Drivers.Type(kind)
	return err == nil && resource.GuardsAccess(t)
}

// openLogin opens a registry login sent sealed for ref's call (ADR 0086). It's never
// logged, and lives as long as the call.
func (c *calls) openLogin(ref resource.Ref, sealed []byte) (registry.Auth, error) {
	if len(sealed) == 0 {
		return registry.Auth{}, errors.New("the call carries no registry login")
	}
	if c.delivery == nil {
		return registry.Auth{}, errors.New("this agent has no delivery key to open the registry login sent with this call")
	}
	values, err := c.delivery.Open(c.server, resource.LoginRef(ref), sealed)
	if err != nil {
		return registry.Auth{}, err
	}
	defer clear(values)
	return registry.AuthFrom(values)
}

// readsAs is ctx carrying the registry login sent sealed with an image read, if
// any, for Dockwell's own reads: Docker isn't asked to pull with it.
func (c *calls) readsAs(ctx context.Context, sealed []byte) (context.Context, error) {
	if len(sealed) == 0 {
		return ctx, nil
	}
	a, err := c.openLogin(resource.Ref{}, sealed)
	if err != nil {
		return ctx, err
	}
	return registry.WithAuth(ctx, a), nil
}

// signedIn opens the registry login sent for ref's plan or step, and says how a
// call carries it: to Dockwell's reads, and to Docker's pull of an image on its
// registry, once the registry's challenge names no token service but the one it
// was saved with. A registry that doesn't answer gets no login from Docker.
func (c *calls) signedIn(ctx context.Context, ref resource.Ref, sealed []byte) (func(context.Context) context.Context, error) {
	a, err := c.openLogin(ref, sealed)
	if err != nil {
		return nil, err
	}
	toDocker, err := c.cfg.images.challenge(ctx, a)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) context.Context {
		ctx = registry.WithAuth(ctx, a)
		if toDocker {
			ctx = docker.WithRegistryAuth(ctx, a.Registry, a.Header())
		}
		return ctx
	}, nil
}

// buildLogins opens the registry logins sent for ref's build, and keeps those whose
// registry's challenge names no token service but the one each was saved with, as
// BuildKit's client, like Docker, follows whatever realm a registry names (ADR
// 0086). A registry that doesn't answer gets no login, and its base images pull
// without one.
func (c *calls) buildLogins(ctx context.Context, ref resource.Ref, sealed []byte) ([]registry.Auth, error) {
	if c.delivery == nil {
		return nil, errors.New("this agent has no delivery key to open the registry logins sent with this build")
	}
	values, err := c.delivery.Open(c.server, resource.BuildLoginRef(ref), sealed)
	if err != nil {
		return nil, err
	}
	defer clear(values)
	auths, err := registry.AuthsFrom(values)
	if err != nil {
		return nil, err
	}
	return c.checkBuildLogins(ctx, auths)
}

// buildGitAuth opens the Authorization sent for ref's build to fetch its private
// repository with: a GitHub App's token for that repository alone (ADR 0098).
func (c *calls) buildGitAuth(ref resource.Ref, sealed []byte) (string, error) {
	return c.openGitAuth(resource.BuildGitRef(ref), sealed)
}

// openGitAuth opens the Authorization sealed for purpose that fetches a private
// repository.
func (c *calls) openGitAuth(purpose resource.Ref, sealed []byte) (string, error) {
	if c.delivery == nil {
		return "", errors.New("this agent has no delivery key to open what fetches this build's repository")
	}
	values, err := c.delivery.Open(c.server, purpose, sealed)
	if err != nil {
		return "", err
	}
	defer clear(values)
	header := values["authorization"]
	if header == "" {
		return "", errors.New("what was sent to fetch this build's repository is empty")
	}
	return header, nil
}

// checkBuildLogins is images.forBuild, with this agent's log. It runs as the step
// starts, and again as the build leaves the queue.
func (c *calls) checkBuildLogins(ctx context.Context, auths []registry.Auth) ([]registry.Auth, error) {
	return c.cfg.images.forBuild(ctx, auths, c.logger)
}

// putCertificates loads the certificates the control plane proved by DNS-01 for this
// server's routes into its ingress, opened from what they were sealed to on this
// stream (ADR 0088).
func (c *calls) putCertificates(ctx context.Context, put *agentv1.PutCertificatesCall) *agentv1.CallResult {
	fail := callFailed
	if c.cfg.Certificates == nil {
		return fail(errors.New("this agent has no ingress to load certificates into"))
	}
	values, err := c.delivery.Open(c.server, resource.CertificatesRef, put.GetSealed())
	if err != nil {
		return fail(err)
	}
	defer clear(values)
	certs := make([]ingress.DNSCert, 0, len(put.GetCertificates()))
	for _, pc := range put.GetCertificates() {
		chain, key := values[pc.GetId()+".chain"], values[pc.GetId()+".key"]
		if chain == "" || key == "" {
			return fail(fmt.Errorf("the certificate %s came without its chain or its key", pc.GetName()))
		}
		certs = append(certs, ingress.DNSCert{ID: pc.GetId(), Name: pc.GetName(), Names: pc.GetNames(), Serves: pc.GetServes(), Chain: []byte(chain), Key: []byte(key)})
	}
	loaded, err := c.cfg.Certificates.PutCertificates(ctx, certs)
	if err != nil {
		return fail(err)
	}
	return &agentv1.CallResult{Result: &agentv1.CallResult_PutCertificates{PutCertificates: &agentv1.PutCertificatesResult{Loaded: int32(min(loaded, math.MaxInt32))}}} //nolint:gosec // bounded above
}
