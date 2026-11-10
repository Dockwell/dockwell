// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"log/slog"
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
	"github.com/dockwell/dockwell/internal/agent/desired"
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

// await is the result of call id, once it comes. A stream that stops sends no
// error of a call that was still running, so a test waits for one before it stops.
func (s *sent) await(t *testing.T, id uint64) *agentv1.CallResult {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if r := s.result(id); r != nil {
			return r
		}
	}
	t.Fatalf("call %d never answered", id)
	return nil
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

// keeping is an agent's calls with a cache of desired state, and the service
// it runs, which uses a secret.
type keeping struct {
	calls  *calls
	out    *sent
	store  *desired.Store
	dir    string
	key    *delivery.Key
	docker *containertest.Fake
	spec   []byte
	svc    *resourcev1.Service
}

var keptWeb = resource.Ref{Kind: service.Kind, Scope: "env_A", Name: "web"}

func newKeeping(t *testing.T) *keeping {
	t.Helper()
	key, err := delivery.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	k := &keeping{dir: t.TempDir(), key: key, docker: containertest.NewFake(), out: &sent{results: map[uint64]*agentv1.CallResult{}}}
	if k.store, err = desired.Open(k.dir, "srv_1", nil); err != nil {
		t.Fatal(err)
	}
	k.calls = newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: k.docker}}),
		Desired: k.store,
	}, k.out.send, key)
	k.svc = &resourcev1.Service{
		Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/web:1"}},
		Secrets:   map[string]string{"TOKEN": "API_TOKEN"},
		Revisions: map[string]int64{"TOKEN": 3},
	}
	if err := (&service.Driver{}).Validate(k.svc); err != nil {
		t.Fatal(err)
	}
	if k.spec, err = resource.Encode(k.svc); err != nil {
		t.Fatal(err)
	}
	return k
}

func (k *keeping) sealed(t *testing.T, token string) []byte {
	t.Helper()
	b, err := secrets.SealValues(k.key.Public(), "", keptWeb, map[string]string{"TOKEN": token})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// What a step leaves a resource as is kept, with the values its spec uses, the
// revision and change it ran toward, and the image it runs, sealed on disk.
func TestDesiredKeepsWhatAStepLeft(t *testing.T) {
	k := newKeeping(t)
	step := resource.Step{Ref: keptWeb, Decision: resource.Decision{Action: resource.ActionCreate}}
	k.calls.start(&agentv1.Call{Id: 1, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{
		Step: resourcepb.Step(step, ""), Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(keptWeb), Spec: k.spec},
		Values: k.sealed(t, "s3cret-for-the-cache"), Deploy: &agentv1.Deploy{Revision: 4, ChangeId: "chg_1"},
	}}})
	k.calls.stop()
	if r := k.out.result(1); r.GetApply() == nil {
		t.Fatalf("the step ended with %v, want it applied", r)
	}
	got, ok, err := k.store.Read(keptWeb)
	if err != nil || !ok {
		t.Fatalf("kept nothing: %v, %v", ok, err)
	}
	if got.Revision != 4 || got.Change != "chg_1" || got.Values["TOKEN"] != "s3cret-for-the-cache" || got.Image.ID != "sha256:fake-acme/web:1" {
		t.Errorf("kept revision %d of %s with %v, running %+v", got.Revision, got.Change, got.Values, got.Image)
	}
	spec, err := k.calls.cfg.Drivers.Decode(service.Kind, got.Spec)
	if err != nil || !proto.Equal(spec, k.svc) {
		t.Errorf("kept the spec %v, %v, want %v", spec, err, k.svc)
	}
	records, _ := filepath.Glob(filepath.Join(k.dir, desired.Folder, "*"))
	for _, f := range records {
		if b, _ := os.ReadFile(f); bytes.Contains(b, []byte("s3cret-for-the-cache")) {
			t.Errorf("%s has the secret in the clear", f)
		}
	}
	// A step that deletes it forgets it.
	gone := resource.Step{Ref: keptWeb, Decision: resource.Decision{Action: resource.ActionDelete}}
	k.calls = newCalls(context.Background(), k.calls.cfg, k.out.send, k.key)
	k.calls.start(&agentv1.Call{Id: 2, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{
		Step: resourcepb.Step(gone, ""), Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(keptWeb)}, Deploy: &agentv1.Deploy{Revision: 5},
	}}})
	k.calls.stop()
	if _, ok, err := k.store.Read(keptWeb); ok || err != nil {
		t.Errorf("still keeps it once deleted: %v, %v", ok, err)
	}
}

// A step that fails keeps nothing: only what passed is desired.
func TestAFailedStepKeepsNothing(t *testing.T) {
	k := newKeeping(t)
	k.docker.Broken["acme/web:1"] = true
	health := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "acme/web:1", Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD", "true"}, Interval: "1s", Timeout: "1s", StartPeriod: "1s", Retries: 1},
	}}}
	spec, err := resource.Encode(health)
	if err != nil {
		t.Fatal(err)
	}
	step := resource.Step{Ref: keptWeb, Decision: resource.Decision{Action: resource.ActionCreate}}
	k.calls.start(&agentv1.Call{Id: 1, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{
		Step: resourcepb.Step(step, ""), Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(keptWeb), Spec: spec}, Deploy: &agentv1.Deploy{Revision: 1},
	}}})
	r := k.out.await(t, 1)
	k.calls.stop()
	if r.GetError() == nil {
		t.Fatalf("the step ended with %v, want it failed", r)
	}
	if _, ok, err := k.store.Read(keptWeb); ok || err != nil {
		t.Errorf("kept a step that failed: %v, %v", ok, err)
	}
}

// What the control plane has the agent remember is kept as what runs now, with its
// values, unless the agent keeps a later revision.
func TestRememberKeepsWhatRunsNow(t *testing.T) {
	k := newKeeping(t)
	remember := func(id uint64, revision int64, token string) *agentv1.CallResult {
		calls := newCalls(context.Background(), k.calls.cfg, k.out.send, k.key)
		calls.start(&agentv1.Call{Id: id, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Remember{Remember: &agentv1.RememberCall{
			Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(keptWeb), Spec: k.spec}, Values: k.sealed(t, token),
			Deploy: &agentv1.Deploy{Revision: revision, ChangeId: "chg_" + token},
		}}})
		defer calls.stop()
		return k.out.await(t, id)
	}
	if r := remember(1, 3, "three"); !r.GetRemember().GetKept() {
		t.Fatalf("revision 3: %v, want it kept", r)
	}
	if r := remember(2, 2, "two"); r.GetRemember() == nil || r.GetRemember().GetKept() {
		t.Fatalf("revision 2 over 3: %v, want it answered and not kept", r)
	}
	got, _, err := k.store.Read(keptWeb)
	if err != nil || got.Revision != 3 || got.Values["TOKEN"] != "three" {
		t.Errorf("keeps revision %d with %v, %v; want 3 with its own value", got.Revision, got.Values, err)
	}
	if held := k.store.Holds().GetHeld(); len(held) != 1 || held[0].GetRevision() != 3 {
		t.Errorf("its hello would say %v", held)
	}
	if r := remember(3, 0, "none"); r.GetError() == nil {
		t.Errorf("no revision: %v, want it refused", r)
	}
	none := newCalls(context.Background(), Config{Logger: slog.New(slog.DiscardHandler)}, k.out.send, k.key)
	none.start(&agentv1.Call{Id: 4, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Remember{Remember: &agentv1.RememberCall{
		Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(keptWeb), Spec: k.spec}, Deploy: &agentv1.Deploy{Revision: 1},
	}}})
	r := k.out.await(t, 4)
	none.stop()
	if r.GetError() == nil {
		t.Errorf("an agent that keeps nothing: %v, want it refused", r)
	}
}

func listCall(id uint64, path string, timeout time.Duration) *agentv1.Call {
	return &agentv1.Call{Id: id, Timeout: durationpb.New(timeout), Call: &agentv1.Call_ListFiles{ListFiles: &agentv1.ListFilesCall{Path: path}}}
}

func saveCall(id uint64, path string, timeout time.Duration) *agentv1.Call {
	return &agentv1.Call{Id: id, Timeout: durationpb.New(timeout), Call: &agentv1.Call_SaveFile{SaveFile: &agentv1.SaveFileCall{Path: path, Content: "x", Version: "v"}}}
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
		if i%2 == 0 {
			c.start(listCall(i+1, "/mnt/dead", time.Hour))
		} else {
			c.start(saveCall(i+1, "/mnt/dead", time.Hour))
		}
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
		if i%3 == 2 {
			c.start(saveCall(i+1, filepath.Join(dir, "nothing"), time.Minute))
			continue
		}
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

// A firewall step isn't desired state: it's kept only once confirmed, which the
// step can't say, and the access manager keeps what was confirmed itself.
func TestAFirewallStepIsntDesiredState(t *testing.T) {
	store, err := desired.Open(t.TempDir(), "srv_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), Config{
		Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&firewall.Driver{Backend: firewall.NewFake()}),
		Desired: store,
	}, out.send, nil)
	ref := resource.Ref{Kind: firewall.Kind, Scope: "srv_1", Name: "http"}
	spec, err := resource.Encode(&resourcev1.FirewallRule{Port: 80, Action: resourcev1.FirewallAction_FIREWALL_ACTION_ALLOW, Protocol: resourcev1.Protocol_PROTOCOL_TCP})
	if err != nil {
		t.Fatal(err)
	}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}
	c.start(&agentv1.Call{Id: 1, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{
		Step: resourcepb.Step(step, ""), Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(ref), Spec: spec},
		Deploy: &agentv1.Deploy{Revision: 1},
	}}})
	c.stop()
	if r := out.result(1); r.GetApply() == nil || r.GetApply().GetUnkept() {
		t.Fatalf("the step ended with %v, want it applied, and not said unkept", r)
	}
	if held := store.Holds().GetHeld(); len(held) != 0 {
		t.Errorf("the cache keeps %v of a firewall step, want nothing", held)
	}
}

// A remember with no spec forgets what the server no longer runs, if the agent
// still holds it at the revision the control plane saw.
func TestRememberWithNoSpecForgets(t *testing.T) {
	k := newKeeping(t)
	call := func(id uint64, desired *agentv1.DesiredResource, revision int64) *agentv1.CallResult {
		calls := newCalls(context.Background(), k.calls.cfg, k.out.send, k.key)
		calls.start(&agentv1.Call{Id: id, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Remember{Remember: &agentv1.RememberCall{
			Desired: desired, Values: k.sealed(t, "kept"), Deploy: &agentv1.Deploy{Revision: revision},
		}}})
		defer calls.stop()
		return k.out.await(t, id)
	}
	if r := call(1, &agentv1.DesiredResource{Resource: resourcepb.Ref(keptWeb), Spec: k.spec}, 3); !r.GetRemember().GetKept() {
		t.Fatalf("remembering it: %v", r)
	}
	forget := &agentv1.DesiredResource{Resource: resourcepb.Ref(keptWeb)}
	if r := call(2, forget, 2); r.GetRemember() == nil || r.GetRemember().GetKept() {
		t.Fatalf("forgetting it at 2, when it's held at 3: %v, want it answered and kept", r)
	}
	if r := call(3, forget, 3); !r.GetRemember().GetKept() {
		t.Fatalf("forgetting it at 3: %v", r)
	}
	if _, ok, err := k.store.Read(keptWeb); ok || err != nil {
		t.Errorf("still keeps it: %v, %v", ok, err)
	}
}

// A step that passes but can't be kept says so, so the control plane sends it
// again, and forgets the older record, which no longer says what runs.
func TestAStepThatCantBeKeptSaysSo(t *testing.T) {
	k := newKeeping(t)
	apply := func(id uint64, revision int64) *agentv1.CallResult {
		calls := newCalls(context.Background(), k.calls.cfg, k.out.send, k.key)
		step := resource.Step{Ref: keptWeb, Decision: resource.Decision{Action: resource.ActionCreate}}
		calls.start(&agentv1.Call{Id: id, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{
			Step: resourcepb.Step(step, ""), Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(keptWeb), Spec: k.spec},
			Values: k.sealed(t, "s3cret"), Deploy: &agentv1.Deploy{Revision: revision},
		}}})
		defer calls.stop()
		return k.out.await(t, id)
	}
	if r := apply(1, 1); r.GetApply() == nil || r.GetApply().GetUnkept() {
		t.Fatalf("the first step: %v", r)
	}
	// Nothing can be renamed over a folder, so the next record can't be written.
	records, _ := filepath.Glob(filepath.Join(k.dir, desired.Folder, "*.rec"))
	if len(records) != 1 {
		t.Fatalf("records %v", records)
	}
	if err := os.Remove(records[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(records[0], 0o700); err != nil {
		t.Fatal(err)
	}
	if r := apply(2, 2); r.GetApply() == nil || !r.GetApply().GetUnkept() {
		t.Fatalf("a step that can't be kept: %v, want it applied and said unkept", r)
	}
	if _, ok, err := k.store.Read(keptWeb); ok || err != nil || len(k.store.Holds().GetHeld()) != 0 {
		t.Errorf("still keeps the older record: %v, %v, %v", ok, err, k.store.Holds().GetHeld())
	}
}
