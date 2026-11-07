// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
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

	sleeper := exec.Command("sleep", "60")
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
	if sleep == nil || sleep.GetName() != "sleep" || sleep.GetCommand() != "sleep 60" || sleep.GetStartTicks() == 0 {
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
	target := func(pid uint32) string { return server + ":" + strconv.FormatUint(uint64(pid), 10) }

	if err := end(pid, sleep.GetStartTicks(), ""); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("ending a process with no passkey: %v, want needs_step_up", err)
	}
	// A proof is for one process on one server.
	other := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_END_PROCESS, target(pid+1))
	if err := end(pid, sleep.GetStartTicks(), other); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("ending a process with another's proof: %v, want needs_step_up", err)
	}
	// One that started at another time isn't this one, and a refusal keeps the proof.
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_END_PROCESS, target(pid))
	if err := end(pid, sleep.GetStartTicks()+1, proof); connect.CodeOf(err) != connect.CodeNotFound || reasonOf(err) != "process_gone" {
		t.Fatalf("ending a process its PID gave way to: %v, want process_gone", err)
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
	proof = stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_END_PROCESS, target(mine))
	if err := end(mine, self.GetStartTicks(), proof); connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "process_protected" {
		t.Errorf("ending the agent's own process: %v, want process_protected", err)
	}

	events := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{ServerId: server})
	var endedOne, refused bool
	for _, e := range events {
		switch e.GetAction() {
		case "process.end":
			p := e.GetProcess()
			endedOne = p.GetPid() == pid && p.GetName() == "sleep" && p.GetCommand() == "sleep 60" && !p.GetKilled() &&
				e.GetKind() == dockwellv1.EventKind_EVENT_KIND_SERVER && e.GetSubject().GetId() == server && e.GetActor().GetName() != ""
		case "process.end_failed":
			refused = refused || e.GetProcess().GetPid() == mine && strings.Contains(e.GetProcess().GetError(), "agent")
		}
	}
	if !endedOne || !refused {
		t.Errorf("Activity has the end %v and the refusal %v, in %v", endedOne, refused, events)
	}
}
