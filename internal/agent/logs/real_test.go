// SPDX-License-Identifier: AGPL-3.0-only

package logs_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/logs"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

const realImage = "alpine:3.21"

// runReal runs a command as a service's container on a real engine, with the
// rotation Dockwell sets, as the driver does.
func runReal(t *testing.T, client *docker.Client, rt *container.Docker, project, service, command string, revision int) *docker.Container {
	t.Helper()
	if err := client.PullImage(t.Context(), realImage, nil); err != nil {
		t.Fatal(err)
	}
	target := container.Target{Name: docker.ContainerName(project, service), Project: project, Service: service, Scoped: true}
	ctx := resource.WithDeploy(t.Context(), resource.Deploy{Revision: revision, Change: "chg_" + strconv.Itoa(revision)})
	spec := &resourcev1.Container{Image: realImage, Command: []string{"sh", "-c", command}, Restart: resourcev1.RestartPolicy_RESTART_POLICY_NO}
	if err := rt.Run(ctx, target, spec, false); err != nil {
		t.Fatal(err)
	}
	c, err := client.ContainerNamed(t.Context(), target.Name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// On a real engine: Docker's framing, its times, its since and its follow are as
// the reader takes them, a long line comes joined, a replace keeps the log, and
// the log rotates within its bound, and reads back whole across its files.
func TestLogsOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	scope := dockertest.Scope(t)
	project := docker.Project(scope)
	dockertest.Cleanup(t, client, project)
	dir := t.TempDir()
	archive := &logs.Archive{Dir: dir, Client: client}
	reader := &logs.Reader{Client: client, Archive: archive}
	rt := &container.Docker{Client: client, Logs: archive}
	ref := &dockwellv1.ResourceRef{Kind: "service", Scope: scope, Name: "web"}

	ctr := runReal(t, client, rt, project, "web", `echo out; echo err >&2; head -c 40000 /dev/zero | tr '\0' x; echo; printf '\033[31mERROR\033[0m red\n'; while sleep 0.1; do echo tick; done`, 1)
	if lc := ctr.HostConfig.LogConfig; lc == nil || lc.Type != docker.LogDriverJSONFile || lc.Config["max-size"] != "10m" {
		t.Errorf("Docker has the log config as %+v", lc)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := newCollector()
	done := make(chan error, 1)
	go func() {
		done <- reader.Stream(ctx, &agentv1.LogsCall{Resource: ref, Follow: true, Tail: 100}, c.send)
	}()
	waitFor(t, "the lines it printed", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.byLine["tick"] != nil && c.byLine["out"] != nil && c.byLine["err"] != nil
	})
	c.mu.Lock()
	if l := c.byLine["err"]; l.GetStream() != dockwellv1.LogStream_LOG_STREAM_STDERR {
		t.Errorf("stderr's line came as %v", l.GetStream())
	}
	var long *dockwellv1.LogLine
	for text, l := range c.byLine {
		if strings.HasPrefix(text, "xxxx") {
			long = l
		}
	}
	if long == nil || len(long.GetText()) != docker.MaxLogLine || int(long.GetCutBytes())+len(long.GetText()) != 40000 {
		t.Errorf("a 40 KB line came as %d bytes with %d cut", len(long.GetText()), long.GetCutBytes())
	}
	if l := c.byLine["\x1b[31mERROR\x1b[0m red"]; l.GetLevel() != dockwellv1.LogLevel_LOG_LEVEL_ERROR {
		t.Errorf("the coloured line came as %v", l)
	}
	last := c.byLine["tick"].GetTime()
	c.mu.Unlock()
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Stream = %v after its reader left", err)
	}

	// Carrying on from a time reads what came after it, once.
	again := newCollector()
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	go func() {
		_ = reader.Stream(ctx2, &agentv1.LogsCall{
			Resource: ref, Follow: true,
			After: []*agentv1.LogCursor{{SourceId: ctr.ID[:12], Time: last, Count: 1}}, ResumeFrom: last,
		}, again.send)
	}()
	waitFor(t, "lines after the place it left", func() bool { return again.count() > 3 })
	for _, text := range again.got() {
		if text != "tick" {
			t.Errorf("after carrying on, a line %.40q that came before it", text)
		}
	}
	cancel2()

	// A replace keeps the log, which reads back when the container has gone.
	runReal(t, client, rt, project, "web", `echo second; sleep 60`, 2)
	found, err := reader.Sources(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	var kept *dockwellv1.LogSource
	for _, s := range found {
		if s.GetState() == dockwellv1.LogSourceState_LOG_SOURCE_STATE_KEPT {
			kept = s
		}
	}
	if kept == nil || kept.GetId() != ctr.ID[:12] || kept.GetRevision() != 1 {
		t.Fatalf("sources = %v, want the first container's log kept", found)
	}
	back := newCollector()
	if err := reader.Stream(t.Context(), &agentv1.LogsCall{Resource: ref, Revision: 1}, back.send); err != nil {
		t.Fatal(err)
	}
	if back.byLine["out"] == nil || back.byLine["tick"] == nil {
		t.Errorf("the kept log has %d lines, without those it printed", back.count())
	}

	// Rotation: it writes past what five files of 10 MiB hold, as json-file counts
	// a line with its JSON around it. It pauses before its last line, which a live
	// stream's limit would otherwise drop with the rest of the flood.
	const written = 400_000
	rot := "rotates"
	writer := `i=0; while [ $i -lt ` + strconv.Itoa(written) + ` ]; do echo "line $i ` + strings.Repeat("y", 100) + `"; i=$((i+1)); done; sleep 3; echo finished; sleep 60`
	runReal(t, client, rt, project, rot, writer, 1)
	refRot := &dockwellv1.ResourceRef{Kind: "service", Scope: scope, Name: rot}
	seen := newCollector()
	ctx3, cancel3 := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel3()
	go func() {
		_ = reader.Stream(ctx3, &agentv1.LogsCall{Resource: refRot, Follow: true, Tail: 1}, seen.send)
	}()
	waitForLong(t, "it to finish writing", func() bool {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		return seen.byLine["finished"] != nil
	})
	cancel3()

	// What is left reads back in order across its compressed files, and what
	// rotated away is gone, so the log kept within its bound.
	all := newCollector()
	if err := reader.Stream(t.Context(), &agentv1.LogsCall{Resource: refRot, Query: "line ", Tail: 5000}, all.send); err != nil {
		t.Fatal(err)
	}
	if len(all.scans) != 1 {
		t.Fatalf("scans = %v, want one", all.scans)
	}
	read := all.scans[0].GetLines()
	if read < 100_000 || read >= written {
		t.Errorf("a search read %d of the %d lines written; want many files' worth, and fewer than all, as the oldest rotated away", read, written)
	}
	if all.scans[0].GetCutShort() {
		t.Error("a search of a rotated log stopped at its limit")
	}
	got := all.got()
	if len(got) != 5000 {
		t.Fatalf("the search kept %d lines, want 5000", len(got))
	}
	if last := got[len(got)-1]; !strings.HasPrefix(last, "line 399999 ") {
		t.Errorf("the last line it kept is %.20q", last)
	}
	first := newCollector()
	if err := reader.Stream(t.Context(), &agentv1.LogsCall{Resource: refRot, Query: "line 0 yyy", Tail: 10}, first.send); err != nil {
		t.Fatal(err)
	}
	if first.count() != 0 {
		t.Error("the first line it wrote is still there, so it never rotated away")
	}
	t.Logf("read back %d of %d lines of %s across its rotated files, oldest %v", read, written, rot, all.scans[0].GetOldestTime().AsTime())
}

func waitForLong(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
