// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/processes"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A person watches what runs on a server, process by process, and ends one with a
// passkey just now. What Dockwell needs, and a process that has gone, are refused,
// and the end is on Activity (ADR 0041).
func TestProcessesAreWatchedAndEnded(t *testing.T) {
	storetest.Each(t, testProcessesAreWatchedAndEnded)
}

func testProcessesAreWatchedAndEnded(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	server := f.lastDocker.id.ServerID
	pc := dockwellv1connect.NewProcessServiceClient(b, b.origin, connect.WithProtoJSON())

	// Its command line holds a password, as one can, typed with it: the list shows
	// it to the person who looks, and nothing keeps it.
	sleeper := exec.Command("sleep", "60")
	sleeper.Args = []string{"sleep --password=hunter2", "60"}
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- sleeper.Wait() }()
	t.Cleanup(func() { _ = sleeper.Process.Kill() })
	pid := uint32(sleeper.Process.Pid)

	watch, err := pc.WatchProcesses(t.Context(), connect.NewRequest(&dockwellv1.WatchProcessesRequest{ServerId: server}))
	if err != nil {
		t.Fatal(err)
	}
	find := func(list *dockwellv1.ProcessList, pid uint32) *dockwellv1.Process {
		for _, p := range list.GetProcesses() {
			if p.GetPid() == pid {
				return p
			}
		}
		return nil
	}
	var sleep, self *dockwellv1.Process
	var lists int
	for watch.Receive() {
		list := watch.Msg().GetProcesses()
		lists++
		sleep, self = find(list, pid), find(list, uint32(os.Getpid()))
		if list.GetCpuWindow() != nil {
			break
		}
	}
	if err := watch.Err(); err != nil {
		t.Fatal(err)
	}
	if lists != 2 {
		t.Errorf("CPU came with list %d, want the second, a second after the first", lists)
	}
	if sleep == nil || sleep.GetName() != "sleep" || sleep.GetCommand() != "sleep --password=hunter2 60" || sleep.GetStartTicks() == 0 {
		t.Fatalf("the sleep this test started reads %v", sleep)
	}
	if self.GetProtected() != "dockwell" {
		t.Errorf("the process the agent runs in reads %v, want it protected", self)
	}
	_ = watch.Close()

	end := func(pid uint32, start uint64, proof string) error {
		_, err := pc.EndProcess(t.Context(), connect.NewRequest(&dockwellv1.EndProcessRequest{ServerId: server, Pid: pid, StartTicks: start, Proof: proof}))
		return err
	}
	target := func(pid uint32, start uint64) string { return processes.Target(server, pid, start) }

	if err := end(pid, sleep.GetStartTicks(), ""); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("ending a process with no passkey: %v, want needs_step_up", err)
	}
	// A proof is for one process on one server.
	other := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_END_PROCESS, target(pid+1, sleep.GetStartTicks()))
	if err := end(pid, sleep.GetStartTicks(), other); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("ending a process with another's proof: %v, want needs_step_up", err)
	}
	// A proof names when the process started, so one given back after it went
	// ends no other that has its PID now.
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_END_PROCESS, target(pid, sleep.GetStartTicks()+1))
	if err := end(pid, sleep.GetStartTicks(), proof); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("ending a process with the proof for one that started at another time: %v, want needs_step_up", err)
	}
	// One that started at another time isn't this one. A proof shown for the wrong
	// process is spent, so this takes a new one.
	proof = stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_END_PROCESS, target(pid, sleep.GetStartTicks()+1))
	if err := end(pid, sleep.GetStartTicks()+1, proof); connect.CodeOf(err) != connect.CodeNotFound || reasonOf(err) != "process_gone" {
		t.Fatalf("ending a process its PID gave way to: %v, want process_gone", err)
	}
	proof = stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_END_PROCESS, target(pid, sleep.GetStartTicks()))
	// A number past what Linux gives, which a signal would read as every process,
	// never reaches the agent.
	if err := end(processes.MaxPID+1, 1, proof); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ending PID %d: %v, want InvalidArgument", processes.MaxPID+1, err)
	}
	if err := end(4294967295, 1, proof); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ending PID 4294967295, which is -1 as a signal reads it: %v, want InvalidArgument", err)
	}
	if err := end(pid, sleep.GetStartTicks(), proof); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err == nil || !strings.Contains(err.Error(), "terminated") {
			t.Errorf("the sleep ended with %v, want SIGTERM", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sleep is still running")
	}
	if err := end(pid, sleep.GetStartTicks(), proof); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Errorf("a proof ended a second process: %v", err)
	}

	mine := uint32(os.Getpid())
	proof = stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_END_PROCESS, target(mine, self.GetStartTicks()))
	if err := end(mine, self.GetStartTicks(), proof); connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "process_protected" {
		t.Errorf("ending the agent's own process: %v, want process_protected", err)
	}

	events := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{ServerId: server})
	var endedOne, refused bool
	for _, e := range events {
		switch e.GetAction() {
		case "process.end":
			p := e.GetProcess()
			endedOne = p.GetPid() == pid && p.GetName() == "sleep" && !p.GetKilled() &&
				e.GetKind() == dockwellv1.EventKind_EVENT_KIND_SERVER && e.GetSubject().GetId() == server && e.GetActor().GetName() != ""
		case "process.end_failed":
			refused = refused || e.GetProcess().GetPid() == mine && strings.Contains(e.GetProcess().GetError(), "agent")
		}
	}
	if !endedOne || !refused {
		t.Errorf("Activity has the end %v and the refusal %v, in %v", endedOne, refused, events)
	}
	// Nothing on the activity log holds the command line, for a read-only token
	// or anyone else.
	reader := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}
	for _, e := range listEvents(t, dockwellv1connect.NewEventServiceClient(reader, b.origin), &dockwellv1.ListEventsRequest{ServerId: server}) {
		if s := protojson.Format(e); strings.Contains(s, "hunter2") {
			t.Errorf("a read-only token reads the command line on Activity: %s", s)
		}
	}
	var details []string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		all, err := tx.ListEvents(t.Context(), 1000)
		for i := range all {
			details = append(details, all[i].Detail)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(details, "\n"), "hunter2") {
		t.Errorf("the store keeps the command line: %v", details)
	}
}

// Every watch of a server's processes shares one read of its /proc, and a session
// may hold only a few watches open (#218's review, L4).
func TestProcessWatchesShareOneRead(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	server := f.lastDocker.id.ServerID
	pc := dockwellv1connect.NewProcessServiceClient(b, b.origin, connect.WithProtoJSON())
	open := func() *connect.ServerStreamForClient[dockwellv1.WatchProcessesResponse] {
		t.Helper()
		w, err := pc.WatchProcesses(t.Context(), connect.NewRequest(&dockwellv1.WatchProcessesRequest{ServerId: server}))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		return w
	}
	first, second := open(), open()
	// The second list, the one with CPU, which both had to wait for.
	nth := func(w *connect.ServerStreamForClient[dockwellv1.WatchProcessesResponse]) *dockwellv1.ProcessList {
		t.Helper()
		for w.Receive() {
			if list := w.Msg().GetProcesses(); list.GetCpuWindow() != nil {
				return list
			}
		}
		t.Fatalf("the watch ended: %v", w.Err())
		return nil
	}
	a, c := nth(first), nth(second)
	if !a.GetTime().AsTime().Equal(c.GetTime().AsTime()) {
		t.Errorf("two watches of one server read it apart, at %v and %v", a.GetTime().AsTime(), c.GetTime().AsTime())
	}

	for range processes.MaxWatchesPerSession - 2 {
		w := open()
		if !w.Receive() {
			t.Fatalf("a watch within the limit ended: %v", w.Err())
		}
	}
	over := open()
	if over.Receive() || connect.CodeOf(over.Err()) != connect.CodeResourceExhausted {
		t.Errorf("watch %d of one session: %v, want ResourceExhausted", processes.MaxWatchesPerSession+1, over.Err())
	}
}

// No API token reads or ends processes, whatever its scope, over Connect or REST:
// a command line can hold a password, and ending one is as much as root.
func TestATokenNeedsASessionForProcesses(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	server := f.lastDocker.id.ServerID
	for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
		secret := createToken(t, b, scope)
		pc := dockwellv1connect.NewProcessServiceClient(bearer{token: secret, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
		watch, err := pc.WatchProcesses(t.Context(), connect.NewRequest(&dockwellv1.WatchProcessesRequest{ServerId: server}))
		if err == nil {
			watch.Receive()
			err = watch.Err()
		}
		if reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("WatchProcesses with a %v token: %v, want needs_session", scope, err)
		}
		_, err = pc.EndProcess(t.Context(), connect.NewRequest(&dockwellv1.EndProcessRequest{ServerId: server, Pid: 1, StartTicks: 1}))
		if reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("EndProcess with a %v token: %v, want needs_session", scope, err)
		}
		anon := newBrowser(t, b.origin, f.api, nil)
		status, why, body := callREST(t, anon, http.MethodPost, "/v1/servers/"+server+"/processes/1:end",
			map[string]string{"Origin": "", "Sec-Fetch-Site": "", "Authorization": "Bearer " + secret}, `{"startTicks":"1"}`)
		if why != authapi.ReasonNeedsSession {
			t.Errorf("POST .../processes/1:end with a %v token = %d (%q): %s, want needs_session", scope, status, why, body)
		}
	}
}
