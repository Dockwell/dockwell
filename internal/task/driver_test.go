// SPDX-License-Identifier: AGPL-3.0-only

package task

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// clock is a time a test moves by hand.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.at = t; c.mu.Unlock() }

func shell(command, schedule string) *resourcev1.Task {
	return &resourcev1.Task{Runs: &resourcev1.Task_Shell{Shell: &resourcev1.TaskShell{Command: command}}, Schedule: schedule}
}

// stand is a driver on a clock of its own, in a folder of its own.
func stand(t *testing.T, dir string, at time.Time) (*Driver, *clock) {
	t.Helper()
	c := &clock{at: at}
	return &Driver{Dir: dir, Now: c.now, retry: 10 * time.Millisecond}, c
}

func (d *Driver) put(t *testing.T, name string, spec *resourcev1.Task) resource.Ref {
	t.Helper()
	if err := (Type{}).Validate(spec); err != nil {
		t.Fatal(err)
	}
	ref := resource.Ref{Kind: Kind, Scope: "env_1", Name: name}
	action := resource.ActionCreate
	if got, _ := d.Observe(t.Context(), ref); got != nil {
		action = resource.ActionUpdate
	}
	if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: action}}, spec); err != nil {
		t.Fatal(err)
	}
	return ref
}

// ended waits until the log has n runs that ended, and returns every run in it.
func (d *Driver) ended(t *testing.T, n int) []*dockwellv1.TaskRun {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		res, err := d.log.read("")
		if err != nil {
			t.Fatal(err)
		}
		var runs []*dockwellv1.TaskRun
		done := 0
		for _, r := range res.GetRuns() {
			runs = append(runs, r.GetRun())
			if r.GetRun().GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_RUNNING {
				done++
			}
		}
		if done >= n {
			return runs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d runs ended, want %d: %v", done, n, runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lines(r *dockwellv1.TaskRun) []string {
	var out []string
	for _, l := range r.GetOutput() {
		out = append(out, l.GetText())
	}
	return out
}

func TestATaskRunsWhenItsDueAndKeepsWhatItPrinted(t *testing.T) {
	d, c := stand(t, t.TempDir(), time.Date(2026, 11, 4, 10, 2, 0, 0, time.UTC))
	d.put(t, "rotate", shell("echo rotated; echo some went wrong >&2", "*/5 * * * *"))
	if wait := d.tick(t.Context()); wait != 3*time.Minute && wait != lookEvery {
		t.Errorf("at 10:02 it waits %s for 10:05", wait)
	}
	c.set(time.Date(2026, 11, 4, 10, 4, 0, 0, time.UTC))
	d.tick(t.Context())
	if res, _ := d.log.read(""); len(res.GetRuns()) != 0 {
		t.Fatalf("it ran before it was due: %v", res.GetRuns())
	}
	c.set(time.Date(2026, 11, 4, 10, 5, 0, 0, time.UTC))
	d.tick(t.Context())
	runs := d.ended(t, 1)
	r := runs[0]
	if r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED || r.GetReason() != dockwellv1.TaskRunReason_TASK_RUN_REASON_SCHEDULE ||
		!r.GetDueTime().AsTime().Equal(time.Date(2026, 11, 4, 10, 5, 0, 0, time.UTC)) || len(r.GetAttempts()) != 1 || r.GetAttempts()[0].GetExitCode() != 0 {
		t.Errorf("the run is %v", r)
	}
	if got := lines(r); !slices.Contains(got, "rotated") || !slices.Contains(got, "some went wrong") {
		t.Errorf("it printed %q", got)
	}
	if len(runs) != 1 {
		t.Errorf("the log keeps the run that was going beside the one that ended: %v", runs)
	}
}

func TestARunMissedWhileTheAgentWasStoppedRunsOnce(t *testing.T) {
	dir := t.TempDir()
	d, c := stand(t, dir, time.Date(2026, 11, 4, 10, 2, 0, 0, time.UTC))
	d.put(t, "report", shell("echo report", "0 * * * *"))
	d.put(t, "warm", func() *resourcev1.Task { s := shell("echo warm", "0 * * * *"); s.SkipMissed = true; return s }())

	// The agent starts again at 15:30: 11:00 to 15:00 were missed.
	again, _ := stand(t, dir, time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC))
	c.set(again.now())
	again.tick(t.Context())
	runs := again.ended(t, 1)
	if len(runs) != 1 || runs[0].GetReason() != dockwellv1.TaskRunReason_TASK_RUN_REASON_MISSED || !runs[0].GetDueTime().AsTime().Equal(time.Date(2026, 11, 4, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("after five missed runs, it ran %v", runs)
	}
	// Then it's on time again, at 16:00, for both.
	again.Now = func() time.Time { return time.Date(2026, 11, 4, 16, 0, 0, 0, time.UTC) }
	again.tick(t.Context())
	runs = again.ended(t, 3)
	if len(runs) != 3 {
		t.Errorf("at 16:00 it ran %v", runs)
	}
}

func TestRunsNeverOverlap(t *testing.T) {
	d, c := stand(t, t.TempDir(), time.Date(2026, 11, 4, 10, 0, 30, 0, time.UTC))
	ref := d.put(t, "slow", shell("sleep 2", "* * * * *"))
	c.set(time.Date(2026, 11, 4, 10, 1, 0, 0, time.UTC))
	d.tick(t.Context())
	c.set(time.Date(2026, 11, 4, 10, 2, 0, 0, time.UTC))
	d.tick(t.Context())
	if _, err := d.RunNow(t.Context(), ref); err == nil || !strings.Contains(err.Error(), "never overlap") {
		t.Errorf("running it by hand while it runs: %v", err)
	}
	// A slot that came while it ran is counted on that run, and is no run of its own.
	runs := d.ended(t, 1)
	if len(runs) != 1 || runs[0].GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED || runs[0].GetSkippedSlots() != 1 {
		t.Errorf("a run due while the last was going: %v", runs)
	}
}

func TestATimeoutStopsTheWholeCommand(t *testing.T) {
	d, _ := stand(t, t.TempDir(), time.Now())
	spec := shell("sleep 30 & sleep 30; echo never", "@yearly")
	spec.TimeoutSeconds = 1
	ref := d.put(t, "stuck", spec)
	started := time.Now()
	if _, err := d.RunNow(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	r := d.ended(t, 1)[0]
	if r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_TIMED_OUT || !r.GetAttempts()[0].GetTimedOut() || r.GetReason() != dockwellv1.TaskRunReason_TASK_RUN_REASON_BY_HAND {
		t.Errorf("a run past its timeout is %v", r)
	}
	if took := time.Since(started); took > 8*time.Second {
		t.Errorf("stopping it took %s", took)
	}
	if !strings.Contains(r.GetError(), "longer than its timeout, 1s") {
		t.Errorf("it says %q", r.GetError())
	}
}

// Something a command starts that leaves its group outlives it, as under cron,
// and holds its output: the run ends with the shell all the same.
func TestADetachedChildDoesntHoldTheRun(t *testing.T) {
	grace := outputGrace
	outputGrace = 200 * time.Millisecond
	t.Cleanup(func() { outputGrace = grace })
	d, _ := stand(t, t.TempDir(), time.Now())
	spec := shell("setsid sleep 20 & echo started", "@yearly")
	spec.TimeoutSeconds = 5
	ref := d.put(t, "daemon", spec)
	started := time.Now()
	if _, err := d.RunNow(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	r := d.ended(t, 1)[0]
	if took := time.Since(started); took > 3*time.Second {
		t.Errorf("the run took %s, held by what it left", took)
	}
	if r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED || !slices.Contains(lines(r), "started") ||
		!slices.ContainsFunc(lines(r), func(l string) bool { return strings.Contains(l, "still holds its output") }) {
		t.Errorf("a command that left something running is %v", r)
	}
}

// Changing when a task runs never runs it: a new schedule, or resuming it, starts
// from now. The next slot runs as ever.
func TestChangingWhenATaskRunsNeverRunsIt(t *testing.T) {
	d, c := stand(t, t.TempDir(), time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC))
	d.put(t, "nightly", shell("echo nightly", "0 4 * * *"))
	d.put(t, "hourly", shell("echo hourly", "0 * * * *"))
	c.set(time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC))
	d.tick(t.Context())
	d.ended(t, 2)

	c.set(time.Date(2026, 11, 4, 11, 30, 0, 0, time.UTC))
	paused := shell("echo hourly", "0 * * * *")
	paused.Paused = true
	d.put(t, "hourly", paused)
	c.set(time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC))
	d.put(t, "nightly", shell("echo nightly", "0 12 * * *"))
	d.tick(t.Context())
	c.set(time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC))
	d.tick(t.Context())
	d.put(t, "hourly", shell("echo hourly", "0 * * * *"))
	d.tick(t.Context())
	time.Sleep(200 * time.Millisecond)
	if res, _ := d.log.read(""); len(res.GetRuns()) != 2 {
		t.Errorf("changing the schedule, or pausing and resuming, ran %d times", len(res.GetRuns())-2)
	}
	c.set(time.Date(2026, 11, 4, 16, 0, 0, 0, time.UTC))
	d.tick(t.Context())
	runs := d.ended(t, 3)
	if last := runs[len(runs)-1]; last.GetReason() != dockwellv1.TaskRunReason_TASK_RUN_REASON_SCHEDULE || !last.GetDueTime().AsTime().Equal(c.now()) {
		t.Errorf("the next slot after resuming is %v", last)
	}
}

// A clock that ran ahead and is set back doesn't stop the tasks until it catches up.
func TestAClockSetBackDoesntStopTheTasks(t *testing.T) {
	d, c := stand(t, t.TempDir(), time.Date(2026, 11, 4, 10, 2, 0, 0, time.UTC))
	d.put(t, "often", shell("true", "*/5 * * * *"))
	c.set(time.Date(2031, 1, 1, 0, 5, 0, 0, time.UTC)) // years ahead
	d.tick(t.Context())
	d.ended(t, 1)
	for m := 0; m <= 60; m += 5 { // an hour once the clock is right
		c.set(time.Date(2026, 11, 4, 10, 5, 0, 0, time.UTC).Add(time.Duration(m) * time.Minute))
		d.tick(t.Context())
		time.Sleep(20 * time.Millisecond)
	}
	if runs := d.ended(t, 12); len(runs) < 12 {
		t.Errorf("in the hour after the clock went back, it ran %d times", len(runs)-1)
	}
}

// keeping starts the driver's scheduler, as the agent does at its start, and
// returns once runs are the agent's, so stopping it stops them.
func (d *Driver) keeping(t *testing.T) context.CancelFunc {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	go d.Keep(ctx)
	for {
		d.mu.Lock()
		set := d.life != nil
		d.mu.Unlock()
		if set {
			break
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		stop()
		d.Drain(DrainWait)
	})
	return stop
}

// A run the agent never got to keep the end of ends as interrupted when the agent
// starts again.
func TestARunTheAgentNeverKeptEndsInterruptedWhenItStartsAgain(t *testing.T) {
	dir := t.TempDir()
	d, _ := stand(t, dir, time.Now())
	ref := d.put(t, "long", shell("sleep 30", "@yearly"))
	d.keeping(t)
	run, err := d.RunNow(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := stand(t, dir, time.Now())
	res, err := again.Runs("")
	if err != nil {
		t.Fatal(err)
	}
	var seen *dockwellv1.TaskRun
	for _, r := range res.GetRuns() {
		if r.GetRun().GetId() == run.GetId() {
			seen = r.GetRun()
		}
	}
	if seen.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_INTERRUPTED || seen.GetEndTime() == nil || !strings.Contains(seen.GetError(), "agent stopped") {
		t.Errorf("after a start, the run that was going is %v", seen)
	}
}

// Stopping the agent stops a run, which is kept as interrupted before it goes.
func TestStoppingTheAgentStopsARunAndKeepsIt(t *testing.T) {
	d, _ := stand(t, t.TempDir(), time.Now())
	ref := d.put(t, "long", shell("sleep 30", "@yearly"))
	stop := d.keeping(t)
	if _, err := d.RunNow(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	stop()
	d.Drain(DrainWait)
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("stopping took %s", took)
	}
	if r := d.ended(t, 1)[0]; r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_INTERRUPTED || !strings.Contains(r.GetError(), "the agent stopped") {
		t.Errorf("a run the agent stopped is %v", r)
	}
}

// Tasks the agent can't read are read again, so mending the file is enough.
func TestTasksThatCantBeReadAreReadAgain(t *testing.T) {
	every := lookEvery
	lookEvery = 20 * time.Millisecond
	t.Cleanup(func() { lookEvery = every })
	dir := t.TempDir()
	file := filepath.Join(dir, "tasks", "tasks.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, c := stand(t, dir, time.Date(2026, 11, 4, 10, 0, 30, 0, time.UTC))
	ctx, stop := context.WithCancel(t.Context())
	kept := make(chan struct{})
	go func() {
		d.Keep(ctx)
		close(kept)
	}()
	t.Cleanup(func() {
		stop()
		<-kept
	})
	time.Sleep(100 * time.Millisecond)
	if _, err := d.Observe(t.Context(), resource.Ref{Kind: Kind, Scope: "env_1", Name: "x"}); err == nil || !strings.Contains(err.Error(), "move it aside") {
		t.Errorf("a torn file says %v", err)
	}
	if err := os.WriteFile(file, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.put(t, "x", shell("echo again", "* * * * *"))
	c.set(time.Date(2026, 11, 4, 10, 1, 0, 0, time.UTC))
	if r := d.ended(t, 1)[0]; r.GetReason() != dockwellv1.TaskRunReason_TASK_RUN_REASON_SCHEDULE {
		t.Errorf("once the file is mended, it ran %v", r)
	}
}

// Many short lines keep fewer of them, so what's kept is near MaxOutput on disk too.
func TestManyShortLinesKeepTheRecordSmall(t *testing.T) {
	dir := t.TempDir()
	d, _ := stand(t, dir, time.Now())
	ref := d.put(t, "lines", shell("yes '' | head -n 200000", "@yearly"))
	if _, err := d.RunNow(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	r := d.ended(t, 1)[0]
	if len(r.GetOutput()) > MaxOutput/lineCost || r.GetOutputCutBytes() == 0 {
		t.Errorf("it kept %d lines, cut %d bytes", len(r.GetOutput()), r.GetOutputCutBytes())
	}
	files, _ := filepath.Glob(filepath.Join(dir, "tasks", "runs", "*.json"))
	for _, f := range files {
		if st, err := os.Stat(f); err == nil && st.Size() > 3*MaxOutput/2 {
			t.Errorf("its record takes %d bytes", st.Size())
		}
	}
}

func TestAFailedRunTriesAgainAsAsked(t *testing.T) {
	d, _ := stand(t, t.TempDir(), time.Now())
	spec := shell("echo trying; exit 3", "@yearly")
	spec.Retries = 2
	ref := d.put(t, "flaky", spec)
	if _, err := d.RunNow(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	r := d.ended(t, 1)[0]
	if r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_FAILED || len(r.GetAttempts()) != 3 || r.GetAttempts()[2].GetExitCode() != 3 || r.GetError() != "it exited with 3" {
		t.Errorf("a run that fails three times is %v", r)
	}
	if n := strings.Count(strings.Join(lines(r), "\n"), "trying"); n != 3 {
		t.Errorf("its output has %d tries", n)
	}
}

func TestAPausedTaskRunsOnlyByHand(t *testing.T) {
	d, c := stand(t, t.TempDir(), time.Date(2026, 11, 4, 10, 2, 0, 0, time.UTC))
	spec := shell("echo by hand", "*/5 * * * *")
	spec.Paused = true
	ref := d.put(t, "paused", spec)
	c.set(time.Date(2026, 11, 4, 10, 5, 0, 0, time.UTC))
	d.tick(t.Context())
	time.Sleep(50 * time.Millisecond)
	if res, _ := d.log.read(""); len(res.GetRuns()) != 0 {
		t.Fatalf("a paused task ran on its schedule: %v", res.GetRuns())
	}
	if _, err := d.RunNow(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if r := d.ended(t, 1)[0]; r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED {
		t.Errorf("run by hand, it's %v", r)
	}
}

func TestARequestKeepsItsStatusAndNotItsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = fmt.Fprint(w, "the page's secret body")
	}))
	t.Cleanup(srv.Close)
	d, _ := stand(t, t.TempDir(), time.Now())
	up := d.put(t, "up", &resourcev1.Task{Runs: &resourcev1.Task_Request{Request: &resourcev1.TaskRequest{Url: srv.URL + "/up"}}, Schedule: "@yearly"})
	down := d.put(t, "down", &resourcev1.Task{Runs: &resourcev1.Task_Request{Request: &resourcev1.TaskRequest{Url: srv.URL + "/down", ExpectStatus: 200}}, Schedule: "@yearly"})
	metadata := d.put(t, "metadata", &resourcev1.Task{Runs: &resourcev1.Task_Request{Request: &resourcev1.TaskRequest{Url: "http://169.254.169.254/latest/meta-data/"}}, Schedule: "@yearly"})
	for _, ref := range []resource.Ref{up, down, metadata} {
		if _, err := d.RunNow(t.Context(), ref); err != nil {
			t.Fatal(err)
		}
	}
	res, err := d.log.read("")
	if err != nil {
		t.Fatal(err)
	}
	for len(res.GetRuns()) < 3 || slices.ContainsFunc(res.GetRuns(), func(r *agentv1.AgentTaskRun) bool {
		return r.GetRun().GetState() == dockwellv1.TaskRunState_TASK_RUN_STATE_RUNNING
	}) {
		time.Sleep(10 * time.Millisecond)
		res, _ = d.log.read("")
	}
	got := map[string]*dockwellv1.TaskRun{}
	for _, r := range res.GetRuns() {
		got[r.GetTask().GetName()] = r.GetRun()
		if strings.Contains(strings.Join(lines(r.GetRun()), "\n"), "secret body") {
			t.Errorf("%s kept the body", r.GetTask().GetName())
		}
	}
	if r := got["up"]; r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED || r.GetAttempts()[0].GetHttpStatus() != 200 || r.GetAttempts()[0].GetResponseBytes() != 22 {
		t.Errorf("a 200 is %v", r)
	}
	if r := got["down"]; r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_FAILED || r.GetError() != "it answered 503, and the task expects 200" {
		t.Errorf("a 503 is %v", r)
	}
	if r := got["metadata"]; r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_FAILED || !strings.Contains(r.GetError(), "metadata service") {
		t.Errorf("a request to the metadata service is %v", r)
	}
}

// Where clouds keep their metadata is refused however it's written, as is what's
// no one server. This server and its network are a task's to ask.
func TestARequestRefusesMetadataHoweverItsWritten(t *testing.T) {
	for address, refused := range map[string]bool{
		"169.254.169.254:80":          true,
		"[::ffff:169.254.169.254]:80": true,
		"[64:ff9b::a9fe:a9fe]:80":     true, // NAT64
		"[fe80::1%eth0]:80":           true,
		"100.100.100.200:80":          true,
		"[fd00:ec2::254]:80":          true,
		"0.0.0.0:7600":                true,
		"[::]:7600":                   true,
		"224.0.0.251:5353":            true,
		"127.0.0.1:80":                false,
		"10.0.0.5:8081":               false,
		"100.100.100.199:80":          false, // a Tailscale address, say
		"[fd00:ec2::253]:80":          false,
		"192.0.2.10:443":              false,
		"[2001:db8::1]:443":           false,
	} {
		if got := refuseAddress(address) != nil; got != refused {
			t.Errorf("%s: refused %v, want %v", address, got, refused)
		}
	}
}

func TestTasksAndRunsOutliveTheAgent(t *testing.T) {
	dir := t.TempDir()
	d, _ := stand(t, dir, time.Now())
	ref := d.put(t, "kept", shell("echo kept", "@daily"))
	if _, err := d.RunNow(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	d.ended(t, 1)
	again, _ := stand(t, dir, time.Now())
	if got, err := again.Observe(t.Context(), ref); err != nil || got.(*resourcev1.Task).GetShell().GetCommand() != "echo kept" {
		t.Fatalf("after a restart the task is %v, %v", got, err)
	}
	res, err := again.Runs("")
	if err != nil || len(res.GetRuns()) != 1 || res.GetCursor() == "" {
		t.Fatalf("after a restart the runs are %v, %v", res, err)
	}
	// A read past them lets them go, as the control plane has them.
	next, err := again.Runs(res.GetCursor())
	if err != nil || len(next.GetRuns()) != 0 || again.log.waiting() {
		t.Errorf("past the cursor there's %v, %v", next, err)
	}
	if err := again.Apply(context.Background(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionDelete}}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := again.Observe(t.Context(), ref); got != nil {
		t.Errorf("a deleted task is still kept: %v", got)
	}
}

func TestAnAppTaskRunsAsADockerComposeRunWould(t *testing.T) {
	e := dockertest.New(t)
	var ran []string
	var mu sync.Mutex
	e.AddLocalImage("ghcr.io/acme/api:1", dockertest.Image{Config: docker.Config{Cmd: []string{"serve"}}, Exec: func(x dockertest.Exec) int {
		mu.Lock()
		ran = append(ran, strings.Join(x.Cmd, " "))
		mu.Unlock()
		_, _ = fmt.Fprintln(x.Stdout, "412 orders")
		for _, v := range x.Env {
			if v == "DATABASE_URL=postgres://db/shop" {
				_, _ = fmt.Fprintln(x.Stdout, "has the app's environment")
			}
		}
		return 0
	}})
	client := e.Client()
	app, err := client.CreateContainer(t.Context(), "env_1-api-1", &docker.CreateRequest{Config: docker.Config{
		Image: "ghcr.io/acme/api:1", Env: []string{"DATABASE_URL=postgres://db/shop"},
		Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: "api", docker.LabelNumber: "1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), app); err != nil {
		t.Fatal(err)
	}
	d, _ := stand(t, t.TempDir(), time.Now())
	d.Client = client
	ref := d.put(t, "report", &resourcev1.Task{Runs: &resourcev1.Task_App{App: &resourcev1.TaskApp{Service: "api", Command: `/app/api report sales --day="last week"`}}, Schedule: "@daily"})
	if _, err := d.RunNow(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	r := d.ended(t, 1)[0]
	if r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED || !slices.Contains(lines(r), "412 orders") || !slices.Contains(lines(r), "has the app's environment") {
		t.Errorf("the app's task is %v", r)
	}
	mu.Lock()
	if !slices.Contains(ran, "/app/api report sales --day=last week") {
		t.Errorf("it ran %q", ran)
	}
	mu.Unlock()
	list, err := client.ListContainers(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("after the run there are %d containers, want the app's alone", len(list))
	}

	// An app that isn't running here: the run doesn't start, and says why.
	if err := client.StopContainer(t.Context(), app, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := d.RunNow(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	runs := d.ended(t, 2)
	last := runs[len(runs)-1]
	if last.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_NOT_STARTED || !strings.Contains(last.GetError(), "api isn't running on this server") {
		t.Errorf("with the app stopped, the run is %v", last)
	}
}

// A run leaves no volume behind: its image declares one the app doesn't mount,
// which Docker makes for the run. And an app on two networks runs its task on
// both, joining the one its network mode names first, as Docker takes it.
func TestAnAppTaskLeavesNoVolumeAndJoinsEachNetworkOnce(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("ghcr.io/acme/api:1", dockertest.Image{
		Config: docker.Config{Cmd: []string{"serve"}}, Volumes: []string{"/var/cache/api"},
		Exec: func(x dockertest.Exec) int { return 0 },
	})
	client := e.Client()
	for _, n := range []string{"env_1_back", "env_1_front"} {
		if _, err := client.CreateNetwork(t.Context(), &docker.NetworkCreate{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	app, err := client.CreateContainer(t.Context(), "env_1-api-1", &docker.CreateRequest{
		Config: docker.Config{Image: "ghcr.io/acme/api:1", Labels: map[string]string{docker.LabelProject: "env_1", docker.LabelService: "api", docker.LabelNumber: "1"}},
		// The mode names the network that sorts after the other.
		HostConfig:       docker.HostConfig{NetworkMode: "env_1_front"},
		NetworkingConfig: docker.NetworkingConfig{EndpointsConfig: map[string]*docker.EndpointSettings{"env_1_back": {}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), app); err != nil {
		t.Fatal(err)
	}
	before := e.VolumeNames()
	d, _ := stand(t, t.TempDir(), time.Now())
	d.Client = client
	ref := d.put(t, "report", &resourcev1.Task{Runs: &resourcev1.Task_App{App: &resourcev1.TaskApp{Service: "api", Command: "/app/api report"}}, Schedule: "@daily"})
	if _, err := d.RunNow(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if r := d.ended(t, 1)[0]; r.GetState() != dockwellv1.TaskRunState_TASK_RUN_STATE_SUCCEEDED {
		t.Fatalf("an app's task on two networks is %v", r)
	}
	if after := e.VolumeNames(); !slices.Equal(after, before) {
		t.Errorf("volumes before the run %v, after %v", before, after)
	}
}

func TestWordsSplitAsAShellWould(t *testing.T) {
	for in, want := range map[string][]string{
		`/app/api report sales`:         {"/app/api", "report", "sales"},
		`sh -c 'echo "$HOME" > /tmp/x'`: {"sh", "-c", `echo "$HOME" > /tmp/x`},
		`echo "a b" c\ d`:               {"echo", "a b", "c d"},
		`echo "say \"hi\""`:             {"echo", `say "hi"`},
		`  spaced   out  `:              {"spaced", "out"},
	} {
		got, err := Words(in)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("Words(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`echo 'open`, `echo "open`, `echo end\`} {
		if _, err := Words(bad); err == nil {
			t.Errorf("Words(%q) takes it", bad)
		}
	}
}
