// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func taskEdit(name string, spec *resourcev1.Task) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "task", Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Task{Task: spec}},
	}
}

func shellTask(command, schedule string) *resourcev1.Task {
	return &resourcev1.Task{Runs: &resourcev1.Task_Shell{Shell: &resourcev1.TaskShell{Command: command}}, Schedule: schedule}
}

// ended runs a task now and waits for the run to end, as the control plane reads
// it from the agent.
func ranTask(t *testing.T, tc dockwellv1connect.TaskServiceClient, name string) *dockwellv1.TaskRun {
	t.Helper()
	started, err := tc.RunTask(t.Context(), connect.NewRequest(&dockwellv1.RunTaskRequest{Id: name}))
	if err != nil {
		t.Fatalf("running %s: %v", name, err)
	}
	if started.Msg.GetRun().GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_RUNNING || started.Msg.GetRun().GetReason() != dockwellv1.TaskRunReason_TASK_RUN_REASON_BY_HAND {
		t.Errorf("Run now returns %v", started.Msg.GetRun())
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		runs, err := tc.ListTaskRuns(t.Context(), connect.NewRequest(&dockwellv1.ListTaskRunsRequest{Id: name}))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs.Msg.GetRuns() {
			if r.GetId() == started.Msg.GetRun().GetId() && r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_RUNNING {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's run didn't end: %v", name, runs.Msg.GetRuns())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func outputOf(r *dockwellv1.TaskRun) []string {
	var out []string
	for _, l := range r.GetOutput() {
		out = append(out, l.GetText())
	}
	return out
}

// A scheduled task is a plan, a shell command one that a passkey approves as it's
// root on the server, and its agent runs it: Run now, and the control plane keeps
// what it printed. A run that fails is on Activity, and so is the one that succeeds
// after it.
func TestAScheduledTaskRunsOnItsServer(t *testing.T) {
	storetest.Each(t, testAScheduledTaskRunsOnItsServer)
}

func testAScheduledTaskRunsOnItsServer(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	ch := changeClient(b, b.origin)
	tc := dockwellv1connect.NewTaskServiceClient(b, b.origin, connect.WithProtoJSON())

	// A shell command is root on the server, so a person approves it with a passkey.
	made := propose(t, ch, "Rotate logs", taskEdit("rotate", shellTask("echo rotated; echo also on stderr >&2", "30 2 * * *")))
	if !made.GetPlan().GetHostAccess() || !slices.ContainsFunc(made.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
		return slices.ContainsFunc(r.GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
			return a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_SHELL && a.GetSameAsRoot()
		})
	}) {
		t.Fatalf("a shell task plans no root on its server: %v", made.GetPlan())
	}
	if _, err := ch.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: made.GetId(), Fingerprint: made.GetPlan().GetFingerprint()})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("applying a shell task without a passkey: %v", err)
	}
	applyAsRoot(t, b, made)

	r := ranTask(t, tc, "rotate")
	if r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED || !slices.Contains(outputOf(r), "rotated") || !slices.Contains(outputOf(r), "also on stderr") ||
		r.GetServerName() != "hz-1" || !strings.HasPrefix(r.GetTaskId(), "tsk_") {
		t.Errorf("the run is %v", r)
	}

	// Its schedule is no new code, so changing it asks for no passkey; its command is.
	later := propose(t, ch, "Later", taskEdit("rotate", shellTask("echo rotated; echo also on stderr >&2", "30 3 * * *")))
	if later.GetPlan().GetHostAccess() {
		t.Errorf("a new schedule for a shell command asks for a passkey: %v", later.GetPlan())
	}
	if got := apply(t, ch, later); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the new schedule ended %v: %v", got.GetState(), got.GetError())
	}
	broken := propose(t, ch, "Break it", taskEdit("rotate", shellTask("echo trying; exit 4", "30 3 * * *")))
	if !broken.GetPlan().GetHostAccess() {
		t.Errorf("a new command for a shell task asks for no passkey: %v", broken.GetPlan())
	}
	applyAsRoot(t, b, broken)
	r = ranTask(t, tc, "rotate")
	if r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_FAILED || r.GetAttempts()[0].GetExitCode() != 4 || r.GetError() != "it exited with 4" {
		t.Errorf("a run that exits 4 is %v", r)
	}
	fixed := propose(t, ch, "Fix it", taskEdit("rotate", shellTask("echo fixed", "30 3 * * *")))
	applyAsRoot(t, b, fixed)
	if r = ranTask(t, tc, "rotate"); r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED {
		t.Errorf("the fixed run is %v", r)
	}
	var events []*dockwellv1.Event
	deadline := time.Now().Add(10 * time.Second)
	for {
		events = listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_TASK}})
		if len(events) >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var actions []string
	for _, e := range events {
		actions = append(actions, e.GetAction())
		if e.GetAction() == "task.failed" && (e.GetTaskRun().GetExitCode() != 4 || e.GetTaskRun().GetTask() != "rotate" || e.GetTaskRun().GetServerName() != "hz-1") {
			t.Errorf("the failure on Activity says %v", e.GetTaskRun())
		}
	}
	slices.Sort(actions)
	if !slices.Equal(actions, []string{"task.failed", "task.recovered", "task.run", "task.run", "task.run"}) {
		t.Errorf("Activity has %v", actions)
	}

	// A request asks the URL from the server, and keeps its status and not its body.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("a body no one keeps")) }))
	t.Cleanup(srv.Close)
	ping := &resourcev1.Task{Runs: &resourcev1.Task_Request{Request: &resourcev1.TaskRequest{Url: srv.URL + "/health"}}, Schedule: "*/5 * * * *"}
	if got := apply(t, ch, propose(t, ch, "Ping", taskEdit("ping", ping))); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("making a request task ended %v: %v", got.GetState(), got.GetError())
	}
	r = ranTask(t, tc, "ping")
	if r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED || r.GetAttempts()[0].GetHttpStatus() != 200 || strings.Contains(strings.Join(outputOf(r), "\n"), "no one keeps") {
		t.Errorf("the request's run is %v", r)
	}

	list, err := tc.ListTasks(t.Context(), connect.NewRequest(&dockwellv1.ListTasksRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, info := range list.Msg.GetTasks() {
		names = append(names, info.GetResource().GetName())
		on := info.GetServers()
		if len(on) != 1 || on[0].GetServerName() != "hz-1" || on[0].GetConnection() != dockwellv1.Connection_CONNECTION_CONNECTED ||
			on[0].GetNextRun() == nil || on[0].GetLastRun() == nil || len(on[0].GetLastRun().GetOutput()) != 0 {
			t.Errorf("%s stands as %v", info.GetResource().GetName(), on)
		}
	}
	if !slices.Equal(names, []string{"ping", "rotate"}) {
		t.Errorf("the tasks are %v", names)
	}
	if got := list.Msg.GetTasks()[1]; got.GetKind() != dockwellv1.TaskKind_TASK_KIND_SHELL || len(got.GetServers()[0].GetRecent()) != 3 {
		t.Errorf("rotate reads as %v", got)
	}
}
