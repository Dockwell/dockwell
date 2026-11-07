// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/container/containertest"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/secrets"
)

// slowFirewall puts the rule "slow" in place only once released, or not at
// all if its call is cancelled first.
type slowFirewall struct {
	*firewall.Fake
	started chan struct{}
	release chan struct{}
}

func (f *slowFirewall) Put(ctx context.Context, name string, rule *resourcev1.FirewallRule) error {
	if name == "slow" {
		f.started <- struct{}{}
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.Fake.Put(ctx, name, rule)
}

// sent keeps the results a stream sent back.
type sent struct {
	mu      sync.Mutex
	results map[uint64]*agentv1.CallResult
}

func (s *sent) send(m *agentv1.ConnectRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[m.GetResult().GetId()] = m.GetResult()
	return nil
}

func (s *sent) result(id uint64) *agentv1.CallResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.results[id]
}

func applyCall(t *testing.T, id uint64, name string) *agentv1.Call {
	t.Helper()
	ref := resource.Ref{Kind: firewall.Kind, Scope: "srv_A", Name: name}
	rule := &resourcev1.FirewallRule{Port: 8080}
	if err := (&firewall.Driver{}).Validate(rule); err != nil {
		t.Fatal(err)
	}
	spec, err := resource.Encode(rule)
	if err != nil {
		t.Fatal(err)
	}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}
	return &agentv1.Call{Id: id, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{
		Step: resourcepb.Step(step, ""), Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(ref), Spec: spec},
	}}}
}

// A stream that closes refuses the calls that arrive from then on, saying
// so, and runs none of them; the calls it took on finish, and their results
// go back. Stopping then waits for what still runs, and no call starts
// while it waits, however many arrive.
func TestCallsCloseBeforeTheyStop(t *testing.T) {
	fw := &slowFirewall{Fake: firewall.NewFake(), started: make(chan struct{}, 1), release: make(chan struct{})}
	out := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&firewall.Driver{Backend: fw}),
	}, out.send, nil)

	c.start(applyCall(t, 1, "slow"))
	<-fw.started
	c.close() // moving to a renewed certificate
	c.start(applyCall(t, 2, "late"))
	if r := out.result(2); !r.GetError().GetDraining() || r.GetError().GetMessage() == "" {
		t.Errorf("a call that arrived as the stream closed got %v, want it refused as draining", r)
	}
	if rule, _ := fw.Rule(t.Context(), "late"); rule != nil {
		t.Error("a refused call ran")
	}
	if !c.busy() {
		t.Fatal("closing stopped the call under way")
	}
	close(fw.release)
	deadline := time.Now().Add(5 * time.Second)
	for c.busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r := out.result(1); r.GetApply() == nil {
		t.Errorf("the call under way ended with %v, want it applied", r)
	}

	// Calls keep arriving while the stream stops, as from a receiver that
	// hasn't stopped yet: none starts once stop waits.
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() { c.start(applyCall(t, uint64(10+i), "rule")) })
	}
	c.stop()
	wg.Wait()
	for i := range 100 {
		if r := out.result(uint64(10 + i)); r == nil || !r.GetError().GetDraining() {
			t.Fatalf("call %d, after the stream closed, ended with %v; want it refused", 10+i, r)
		}
	}
}

// Stopping cancels the calls still running, and waits for them.
func TestStoppingCancelsWhatStillRuns(t *testing.T) {
	fw := &slowFirewall{Fake: firewall.NewFake(), started: make(chan struct{}, 1), release: make(chan struct{})}
	out := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&firewall.Driver{Backend: fw}),
	}, out.send, nil)
	c.start(applyCall(t, 1, "slow"))
	<-fw.started
	c.stop()
	if c.busy() || fw.Len() != 0 {
		t.Errorf("after stopping, busy %t, %d rules; want the call cancelled", c.busy(), fw.Len())
	}
}

// An apply the agent can't read, such as from a newer control plane, runs
// nothing, and says to update the agent.
func TestAnApplyWithAnUnknownActionRunsNothing(t *testing.T) {
	fw := firewall.NewFake()
	out := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&firewall.Driver{Backend: fw}),
	}, out.send, nil)
	call := applyCall(t, 1, "http")
	call.GetApply().GetStep().Action = 42
	c.start(call)
	deadline := time.Now().Add(5 * time.Second)
	for c.busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	c.stop()
	msg := out.result(1).GetError().GetMessage()
	if !strings.Contains(msg, "action, number 42") || !strings.Contains(msg, "update the agent") || fw.Len() != 0 {
		t.Errorf("an apply with action 42 = %q, with %d rules; want it refused, saying to update the agent", msg, fw.Len())
	}
}

// stalling is a stream whose sends write in parts, and stall partway until
// released, as a send does while the transport takes what it wrote. It
// notes a half-close that comes while a send is under way, which would cut
// the message off.
type stalling struct {
	entered, release chan struct{}
	sending, cut     atomic.Bool
	closed           atomic.Bool
}

func (s *stalling) Send(*agentv1.ConnectRequest) error {
	s.sending.Store(true)
	defer s.sending.Store(false)
	s.entered <- struct{}{}
	<-s.release
	return nil
}

func (s *stalling) CloseRequest() error {
	s.cut.Store(s.sending.Load())
	s.closed.Store(true)
	return nil
}

// The request side of the stream closes only between two sends: a call's
// result may still be going out as the stream ends.
func TestTheStreamHalfClosesOnlyBetweenSends(t *testing.T) {
	stream := &stalling{entered: make(chan struct{}, 1), release: make(chan struct{})}
	out := newSender(stream)
	sent := make(chan error, 1)
	go func() { sent <- out.send(&agentv1.ConnectRequest{}) }()
	<-stream.entered
	closed := make(chan struct{})
	go func() { out.closeRequest(time.Minute); close(closed) }()
	select {
	case <-closed:
		t.Fatal("the stream half-closed while a result was going out")
	case <-time.After(100 * time.Millisecond):
	}
	close(stream.release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	<-closed
	if stream.cut.Load() || !stream.closed.Load() {
		t.Errorf("half-closed during a send: %t, half-closed at all: %t; want it closed once the send was done", stream.cut.Load(), stream.closed.Load())
	}
}

// A send that never finishes, as on a stream nobody reads, doesn't keep the
// stream from ending: the half-close gives up on it.
func TestAStuckSendDoesntHoldTheStreamOpen(t *testing.T) {
	stream := &stalling{entered: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(stream.release)
	out := newSender(stream)
	go func() { _ = out.send(&agentv1.ConnectRequest{}) }()
	<-stream.entered
	start := time.Now()
	out.closeRequest(50 * time.Millisecond)
	if waited := time.Since(start); waited > 5*time.Second || stream.cut.Load() {
		t.Errorf("the half-close waited %s for a stuck send, and cut it: %t", waited, stream.cut.Load())
	}
}

// A step cut short because the stream is ending says nothing. "Context
// cancelled" isn't what happened: the control plane sees the stream end and says
// the agent disconnected, and that the step may or may not have run.
func TestAStepCutShortByStoppingSendsNoResult(t *testing.T) {
	fw := &slowFirewall{Fake: firewall.NewFake(), started: make(chan struct{}, 1), release: make(chan struct{})}
	out := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&firewall.Driver{Backend: fw}),
	}, out.send, nil)
	c.start(applyCall(t, 1, "slow"))
	<-fw.started
	c.stop()
	if r := out.result(1); r != nil {
		t.Errorf("a step cut short by stopping sent %v, want nothing", r)
	}

	// One the control plane cancelled itself still answers, on a stream that carries on.
	fw = &slowFirewall{Fake: firewall.NewFake(), started: make(chan struct{}, 1), release: make(chan struct{})}
	out = &sent{results: map[uint64]*agentv1.CallResult{}}
	c = newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&firewall.Driver{Backend: fw}),
	}, out.send, nil)
	defer c.stop()
	c.start(applyCall(t, 2, "slow"))
	<-fw.started
	c.cancel(2)
	deadline := time.Now().Add(5 * time.Second)
	for c.busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r := out.result(2); r == nil {
		t.Error("a step the control plane cancelled sent nothing, want it to answer")
	}
}

// peeking is a Desired that notes what its context carries.
type peeking struct {
	mu     sync.Mutex
	calls  int
	values map[string]string
}

func (p *peeking) Applied(ctx context.Context, _ resource.Ref, _ proto.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if v := resource.ValuesFrom(ctx); v != nil {
		p.values = maps.Clone(v) // the agent clears its own once the step ends
	}
	return nil
}

// The values of what a step's spec uses reach its driver, and never
// Desired, which may keep what it hears, as #32's cache will: it hears
// the step with a context that holds none.
func TestDesiredHearsNoValues(t *testing.T) {
	key, err := delivery.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	docker, desired := containertest.NewFake(), &peeking{}
	out := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: docker}}),
		Desired: desired,
	}, out.send, key)
	web := resource.Ref{Kind: service.Kind, Scope: "env_A", Name: "web"}
	svc := &resourcev1.Service{
		Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/web:1"}},
		Secrets:   map[string]string{"TOKEN": "API_TOKEN"},
		Revisions: map[string]int64{"TOKEN": 3},
	}
	if err := (&service.Driver{}).Validate(svc); err != nil {
		t.Fatal(err)
	}
	spec, err := resource.Encode(svc)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secrets.SealValues(key.Public(), "", web, map[string]string{"TOKEN": "s3cret-not-for-desired"})
	if err != nil {
		t.Fatal(err)
	}
	step := resource.Step{Ref: web, Decision: resource.Decision{Action: resource.ActionCreate}}
	c.start(&agentv1.Call{Id: 1, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{
		Step: resourcepb.Step(step, ""), Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(web), Spec: spec}, Values: sealed,
	}}})
	c.stop()
	if r := out.result(1); r.GetApply() == nil {
		t.Fatalf("the step ended with %v, want it applied", r)
	}
	if got, _ := docker.Inspect(t.Context(), service.Target(web)); got.GetEnv()["TOKEN"] != "s3cret-not-for-desired" {
		t.Fatalf("the driver ran the container with %v; the test didn't send what it meant to", got.GetEnv())
	}
	desired.mu.Lock()
	defer desired.mu.Unlock()
	if desired.calls != 1 || desired.values != nil {
		t.Errorf("Desired heard of %d steps, with values %v; want one, and no values", desired.calls, desired.values)
	}
}

func listCall(id uint64, path string, timeout time.Duration) *agentv1.Call {
	return &agentv1.Call{Id: id, Timeout: durationpb.New(timeout), Call: &agentv1.Call_ListFiles{ListFiles: &agentv1.ListFilesCall{Path: path}}}
}

// A call into a dead mount can stay in the kernel for as long as the mount is
// down, and nothing can stop it. The agent's next stream, and so the server being
// online, can't wait on that.
func TestAFileCallThatNeverReturnsDoesntHoldUpTheStream(t *testing.T) {
	gone := make(chan struct{})
	t.Cleanup(func() { close(gone) })
	var held atomic.Int32
	disk := files.New(t.TempDir(), nil, nil)
	disk.AnswerWithin = time.Hour
	disk.Touching = func(string) { held.Add(1); <-gone }
	s := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{Logger: slog.New(slog.DiscardHandler), Files: disk}, s.send, nil)
	for i := range uint64(4) {
		c.start(listCall(i+1, "/mnt/dead", time.Hour))
	}
	deadline := time.Now().Add(5 * time.Second)
	for held.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d calls reached the mount", held.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopped := make(chan struct{})
	go func() { c.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("stop is waiting on calls that will never return, so this agent would never reconnect")
	}
}

// Each result can hold 4 MiB of a listing, for as long as the stream is slower than
// the agent, so a call keeps its place until its result has gone.
func TestResultsWaitingToSendAreBounded(t *testing.T) {
	dir := t.TempDir()
	disk := files.New(filepath.Join(dir, "data"), nil, nil)
	var waiting atomic.Int32
	release := make(chan struct{})
	c := newCalls(context.Background(), Config{Logger: slog.New(slog.DiscardHandler), Files: disk}, func(*agentv1.ConnectRequest) error {
		waiting.Add(1)
		<-release
		return nil
	}, nil)
	for i := range uint64(12) {
		c.start(listCall(i+1, dir, time.Minute))
	}
	time.Sleep(300 * time.Millisecond)
	if n := waiting.Load(); n > 4 {
		t.Errorf("%d results are held waiting to send, with 4 places", n)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for waiting.Load() < 12 {
		if time.Now().After(deadline) {
			t.Fatalf("%d of 12 results went out once the stream moved", waiting.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.stop()
}

// deafFirewall is a driver stuck as a plan's Lstat is on a dead mount: no context
// reaches it, so it comes back only when the mount does.
type deafFirewall struct {
	*firewall.Fake
	started chan struct{}
	gone    chan struct{}
	once    sync.Once
}

// answer lets what's stuck come back.
func (f *deafFirewall) answer() { f.once.Do(func() { close(f.gone) }) }

func (f *deafFirewall) Put(context.Context, string, *resourcev1.FirewallRule) error {
	f.started <- struct{}{}
	<-f.gone
	return nil
}

func newDeafFirewall(t *testing.T) *deafFirewall {
	t.Helper()
	f := &deafFirewall{Fake: firewall.NewFake(), started: make(chan struct{}, 32), gone: make(chan struct{})}
	t.Cleanup(f.answer)
	return f
}

// stops says whether a stream's calls let it end within a bound.
func stops(c *calls) bool {
	stopped := make(chan struct{})
	go func() { c.stop(); close(stopped) }()
	select {
	case <-stopped:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

// Whatever a call is stuck on, the stream ends, and the agent goes on to its next.
func TestACallThatNeverReturnsDoesntHoldUpTheStream(t *testing.T) {
	fw := newDeafFirewall(t)
	var log strings.Builder
	s := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger:    slog.New(slog.NewTextHandler(&syncWriter{w: &log}, nil)),
		Drivers:   resource.NewRegistry(&firewall.Driver{Backend: fw}),
		CallGrace: 50 * time.Millisecond,
	}, s.send, nil)
	c.start(applyCall(t, 1, "stuck"))
	<-fw.started
	if !stops(c) {
		t.Fatal("stop is waiting on a call that will never return, so this agent would never reconnect")
	}
	if !strings.Contains(log.String(), "left it behind") || !strings.Contains(log.String(), "call=apply") {
		t.Errorf("the log doesn't say what was left behind: %s", log.String())
	}
}

// One that runs out its time is left behind with nothing said: the control plane
// stopped at the same deadline, and said so itself, before the grace has passed.
func TestACallThatRunsOutOfTimeIsLeftBehind(t *testing.T) {
	fw := newDeafFirewall(t)
	s := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger:    slog.New(slog.DiscardHandler),
		Drivers:   resource.NewRegistry(&firewall.Driver{Backend: fw}),
		CallGrace: 20 * time.Millisecond,
	}, s.send, nil)
	call := applyCall(t, 1, "stuck")
	call.Timeout = durationpb.New(20 * time.Millisecond)
	c.start(call)
	<-fw.started
	waitFor(t, "it to be left behind", func() bool { return c.unfinished.left.Load() == 1 })
	c.wg.Wait()
	if got := s.result(1); got != nil {
		t.Errorf("it answered a control plane that has gone on: %v", got)
	}
	fw.answer()
	waitFor(t, "a call that came back to stop counting", func() bool { return c.unfinished.left.Load() == 0 })
	c.stop()
}

// What's left behind can't be stopped, so it's bounded across streams, as each holds
// a thread for as long as whatever it touches is down.
func TestWhatsLeftBehindIsBoundedAcrossStreams(t *testing.T) {
	fw := newDeafFirewall(t)
	cfg := Config{
		Logger:     slog.New(slog.DiscardHandler),
		Drivers:    resource.NewRegistry(&firewall.Driver{Backend: fw}),
		CallGrace:  time.Millisecond,
		unfinished: newUnfinished(),
	}
	for i := range maxLeftBehind {
		s := &sent{results: map[uint64]*agentv1.CallResult{}}
		c := newCalls(context.Background(), cfg, s.send, nil)
		c.start(applyCall(t, uint64(i+1), "stuck-"+strconv.Itoa(i)))
		<-fw.started
		if !stops(c) {
			t.Fatal("stop hung")
		}
	}
	// Each call counts itself as left behind on its own goroutine, which can be
	// a moment after its stream has stopped.
	waitFor(t, "every call to be left behind", func() bool { return cfg.unfinished.left.Load() == maxLeftBehind })
	if n := cfg.unfinished.left.Load(); n != maxLeftBehind {
		t.Fatalf("%d calls left behind", n)
	}
	s := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), cfg, s.send, nil)
	c.start(applyCall(t, 99, "another"))
	deadline := time.Now().Add(3 * time.Second)
	for s.result(99) == nil {
		if time.Now().After(deadline) {
			t.Fatal("no answer past the limit")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.result(99).GetError().GetMessage(); !strings.Contains(got, "never came back") {
		t.Errorf("past the limit a call says %q", got)
	}
	select {
	case <-fw.started:
		t.Error("a call ran past the limit")
	default:
	}
	// Nothing else tells what was refused that it can go.
	fw.answer()
	waitFor(t, "the calls to come back", func() bool { return cfg.unfinished.left.Load() == 0 })
	select {
	case <-cfg.unfinished.back:
	case <-time.After(3 * time.Second):
		t.Error("what was refused at the limit never hears that it can go")
	}
	c.stop()
}

// A send that never returns is as stuck as a call, and the stream still ends.
func TestASendThatNeverReturnsDoesntHoldUpTheStream(t *testing.T) {
	gone := make(chan struct{})
	t.Cleanup(func() { close(gone) })
	var log strings.Builder
	sending := make(chan struct{}, 1)
	disk := files.New(t.TempDir(), nil, nil)
	c := newCalls(context.Background(), Config{
		Logger: slog.New(slog.NewTextHandler(&syncWriter{w: &log}, nil)), Files: disk, CallGrace: 20 * time.Millisecond,
	}, func(*agentv1.ConnectRequest) error { sending <- struct{}{}; <-gone; return nil }, nil)
	c.start(listCall(1, t.TempDir(), time.Minute))
	<-sending
	if !stops(c) {
		t.Fatal("stop is waiting on a send that will never return")
	}
	if !strings.Contains(log.String(), "stream ended") {
		t.Errorf("the log says nothing of it: %s", log.String())
	}
}

// A root that can't be saved holds the save's call, and nothing else.
func TestARootThatWontSaveDoesntHoldUpTheStream(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(data, "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Writing to a FIFO no one reads blocks in a real system call, as a stalled disk does.
	fifo := filepath.Join(data, "agent", "docker-roots.new")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if fd, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK, 0); err == nil {
			time.Sleep(100 * time.Millisecond)
			_ = unix.Close(fd)
		}
	})
	disk := files.New(data, nil, func(context.Context) (string, error) { return "/data/docker", nil })
	disk.AnswerWithin = 100 * time.Millisecond
	s := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{Logger: slog.New(slog.DiscardHandler), Files: disk, CallGrace: 50 * time.Millisecond}, s.send, nil)
	c.start(listCall(1, dir, time.Minute))
	deadline := time.Now().Add(3 * time.Second)
	for s.result(1) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the listing never answered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !stops(c) {
		t.Fatal("a stalled save of Docker's root stopped the stream ending")
	}
}

// syncWriter lets a test read a log calls write from their own goroutines.
type syncWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
