// SPDX-License-Identifier: AGPL-3.0-only

package logs_test

import (
	"context"
	"path/filepath"
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

// logFilesOf lists a container's log files on a real engine, by name and size.
// Only root can read where Docker keeps them, so a container reads them.
func logFilesOf(t *testing.T, client *docker.Client, project string, c *docker.Container) map[string]int64 {
	t.Helper()
	if c.LogPath == "" {
		t.Fatalf("Docker doesn't say where %s keeps its log", c.Name)
	}
	name := "log-files-" + c.ID[:12]
	id, err := client.CreateContainer(t.Context(), name, &docker.CreateRequest{
		Config: docker.Config{
			Image:  realImage,
			Cmd:    []string{"sh", "-c", `cd /logs && for f in *; do echo "$f $(wc -c <"$f")"; done`},
			Labels: map[string]string{docker.LabelProject: project, docker.LabelService: "log-files"},
		},
		HostConfig: docker.HostConfig{Binds: []string{filepath.Dir(c.LogPath) + ":/logs:ro"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.RemoveContainer(context.WithoutCancel(t.Context()), id, true) }()
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	waitForLong(t, "it to list them", func() bool {
		c, err := client.InspectContainer(t.Context(), id)
		return err == nil && !c.State.Running
	})
	rd, err := client.ContainerLogs(t.Context(), id, false, docker.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	files := map[string]int64{}
	for {
		e, err := rd.Next()
		if err != nil {
			return files
		}
		file, size, ok := strings.Cut(string(e.Text), " ")
		if !ok || !strings.Contains(file, "-json.log") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
		if err != nil {
			t.Fatalf("a log file's size reads %q", size)
		}
		files[file] = n
	}
}

// On a real engine: Docker's framing, its times, its since and its follow are as
// the reader takes them, a long line comes joined, a replace keeps the log, with
// what the container writes as it stops, and the log rotates within its 50 MiB,
// its older files compressed, and reads back whole across them.
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

	ctr := runReal(t, client, rt, project, "web", `trap 'echo last-words; exit 0' TERM; echo out; echo err >&2; head -c 40000 /dev/zero | tr '\0' x; echo; printf '\033[31mERROR\033[0m red\n'; while sleep 0.1; do echo tick; done`, 1)
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
	// Printed once the first copy was done, as Docker stopped it: only the second
	// copy has this.
	if back.byLine["last-words"] == nil {
		t.Errorf("the kept log has %d lines, without what it printed as it stopped", back.count())
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

	// What Docker keeps of it stays within the bound, which what it wrote is past,
	// with its older files compressed. Compressing comes a moment after rotating.
	rotating, err := client.ContainerNamed(t.Context(), docker.ContainerName(project, rot))
	if err != nil {
		t.Fatal(err)
	}
	compressed := func(files map[string]int64) int {
		n := 0
		for name := range files {
			if strings.HasSuffix(name, ".gz") {
				n++
			}
		}
		return n
	}
	var files map[string]int64
	waitForLong(t, "Docker to compress its older files", func() bool {
		files = logFilesOf(t, client, project, rotating)
		return compressed(files) >= 2
	})
	total, plain := int64(0), int64(0)
	for name, size := range files {
		total += size
		if !strings.HasSuffix(name, ".gz") {
			plain += size
		}
	}
	t.Logf("Docker keeps %v, %d bytes in all", files, total)
	if len(files) > 5 {
		t.Errorf("Docker keeps %d files of its log, want at most 5: %v", len(files), files)
	}
	if total > 50<<20 {
		t.Errorf("Docker keeps %d bytes of its log, past 50 MiB: %v", total, files)
	}
	if plain > 10<<20+1<<20 {
		t.Errorf("Docker keeps %d bytes uncompressed, more than one file of 10 MiB: %v", plain, files)
	}
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
