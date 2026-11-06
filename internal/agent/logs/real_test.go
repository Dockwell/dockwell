// SPDX-License-Identifier: AGPL-3.0-only

package logs_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

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
	if err := rt.Run(ctx, target, spec, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	return newestReal(t, client, project, service)
}

// newestReal is a service's newest container on a real engine, by its labels, as a
// rolling deploy gives each new one the next number.
func newestReal(t *testing.T, client *docker.Client, project, service string) *docker.Container {
	t.Helper()
	list, err := client.ListContainers(t.Context(), map[string][]string{
		"label": {docker.LabelProject + "=" + project, docker.LabelService + "=" + service},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatalf("Docker has no container of %s", service)
	}
	newest := &list[0]
	for i := range list {
		if list[i].Number() > newest.Number() {
			newest = &list[i]
		}
	}
	c, err := client.InspectContainer(t.Context(), newest.ID)
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
		t.Errorf("a 40 KB line came as %d bytes with %d cut, and the markers said %v", len(long.GetText()), long.GetCutBytes(), c.gaps)
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

	// A rolling deploy has both running at once: what follows reads both, each by the
	// name a person knows it by, and each line once, what the old one says as it stops
	// included, though Docker has it under another name by then.
	moving := newCollector()
	ctx4, cancel4 := context.WithCancel(t.Context())
	defer cancel4()
	go func() {
		_ = reader.Stream(ctx4, &agentv1.LogsCall{Resource: ref, Follow: true, Tail: 1}, moving.send)
	}()
	waitFor(t, "the follower to be reading", func() bool { return moving.count() > 0 })
	moving.mu.Lock()
	place := moving.lastTime()
	moving.mu.Unlock()

	// A replace keeps the log, which reads back when the container has gone.
	next := runReal(t, client, rt, project, "web", `echo second; sleep 60`, 2)
	if want := docker.InstanceName(project, "web", 2); strings.TrimPrefix(next.Name, "/") != want {
		t.Errorf("the new one is %s, want %s: it should have rolled", next.Name, want)
	}
	waitFor(t, "the follower to read both", func() bool {
		moving.mu.Lock()
		defer moving.mu.Unlock()
		return moving.byLine["second"] != nil && moving.byLine["last-words"] != nil
	})
	moving.mu.Lock()
	if second, last := moving.byLine["second"].GetTime().AsTime(), moving.byLine["last-words"].GetTime().AsTime(); !second.Before(last) {
		t.Errorf("the new one said %s, and the old one %s as it stopped: they should overlap", second, last)
	}
	for id, want := range map[string]string{ctr.ID[:12]: docker.ContainerName(project, "web"), next.ID[:12]: docker.InstanceName(project, "web", 2)} {
		if s := moving.sources[id]; s.GetContainerName() != want || s.GetReplica() != 1 {
			t.Errorf("source %s reads %q, replica %d, want %q, replica 1", id, s.GetContainerName(), s.GetReplica(), want)
		}
	}
	moving.mu.Unlock()
	cancel4()

	// One who comes back once the old one has gone reads what it missed of both, from
	// what was kept and from Docker, and what it had already, not again.
	returned := newCollector()
	ctx5, cancel5 := context.WithCancel(t.Context())
	defer cancel5()
	go func() {
		_ = reader.Stream(ctx5, &agentv1.LogsCall{
			Resource: ref, Follow: true,
			After: []*agentv1.LogCursor{{SourceId: ctr.ID[:12], Time: place, Count: 1}}, ResumeFrom: place,
		}, returned.send)
	}()
	waitFor(t, "what it missed of both", func() bool {
		returned.mu.Lock()
		defer returned.mu.Unlock()
		return returned.byLine["second"] != nil && returned.byLine["last-words"] != nil
	})
	for _, text := range returned.got() {
		if text == "out" || text == "err" {
			t.Errorf("it read %q again, from before it left", text)
		}
	}
	cancel5()

	found, err := reader.Sources(t.Context(), ref, 0)
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
	if want := docker.ContainerName(project, "web"); kept.GetContainerName() != want {
		t.Errorf("what was kept reads as %q, want %q, and not what a rollout had it under", kept.GetContainerName(), want)
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

	// A reader who comes back with a place from before what rotated away hears that
	// those lines are gone, and from when to when.
	rotated, err := client.ContainerNamed(t.Context(), docker.ContainerName(project, rot))
	if err != nil {
		t.Fatal(err)
	}
	back2 := newCollector()
	ctx6, cancel6 := context.WithCancel(t.Context())
	defer cancel6()
	place2 := timestamppb.New(rotated.State.StartedAt.Add(-2 * time.Second))
	go func() {
		_ = reader.Stream(ctx6, &agentv1.LogsCall{
			Resource: refRot, Follow: true,
			After: []*agentv1.LogCursor{{SourceId: rotated.ID[:12], Time: place2, Count: 1}}, ResumeFrom: place2,
		}, back2.send)
	}()
	waitForLong(t, "a marker for what rotated away", func() bool {
		back2.mu.Lock()
		defer back2.mu.Unlock()
		for _, g := range back2.gaps {
			if g.GetReason() == dockwellv1.LogGapReason_LOG_GAP_REASON_GONE && g.GetEndTime().AsTime().After(place2.AsTime()) {
				return true
			}
		}
		return false
	})
	cancel6()

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

// runWith runs a command as a service's container on a real engine, as someone
// running docker by hand would, with a log config of its own, which the driver
// doesn't offer. It says which driver Docker took.
func runWith(t *testing.T, client *docker.Client, project, service string, lc *docker.LogConfig, command string) *docker.Container {
	t.Helper()
	id, err := client.CreateContainer(t.Context(), project+"-"+service+"-1", &docker.CreateRequest{
		Config: docker.Config{
			Image:  realImage,
			Cmd:    []string{"sh", "-c", command},
			Labels: map[string]string{docker.LabelProject: project, docker.LabelService: service, docker.LabelNumber: "1"},
		},
		HostConfig: docker.HostConfig{LogConfig: lc},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	c, err := client.InspectContainer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// sourceSays waits for a source to say why its log can't be read, however it's read.
func sourceSays(t *testing.T, reader *logs.Reader, ref *dockwellv1.ResourceRef, c *docker.Container, follows bool) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := newCollector()
	go func() {
		_ = reader.Stream(ctx, &agentv1.LogsCall{Resource: ref, Follow: follows, Tail: 10}, seen.send)
	}()
	var said string
	waitForLong(t, "the source to say why", func() bool {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		said = seen.sources[c.ID[:12]].GetLogError()
		return said != ""
	})
	if n := seen.count(); n != 0 {
		t.Errorf("%d lines came from a log that can't be read", n)
	}
	return said
}

// Docker reads some drivers' logs and not others', and that's all Dockwell sees of
// them. A driver it can't read says so where a reader is, and how to look further,
// whether they follow or read to the end. One it can, through its dual logging, as
// syslog with its cache on, reads and follows as any other.
func TestLogDriversOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	scope := dockertest.Scope(t)
	project := docker.Project(scope)
	dockertest.Cleanup(t, client, project)
	if err := client.PullImage(t.Context(), realImage, nil); err != nil {
		t.Fatal(err)
	}
	reader := &logs.Reader{Client: client, Archive: &logs.Archive{Dir: t.TempDir(), Client: client}}
	syslog := func(extra map[string]string) *docker.LogConfig {
		// Nothing has to listen: UDP sends either way, and what is read is Docker's copy.
		config := map[string]string{"syslog-address": "udp://127.0.0.1:5514"}
		for k, v := range extra {
			config[k] = v
		}
		return &docker.LogConfig{Type: "syslog", Config: config}
	}
	for service, lc := range map[string]*docker.LogConfig{
		"none":    {Type: "none"},
		"nocache": syslog(map[string]string{"cache-disabled": "true"}),
	} {
		t.Run(service, func(t *testing.T) {
			c := runWith(t, client, project, service, lc, `echo printed; sleep 60`)
			ref := &dockwellv1.ResourceRef{Kind: "service", Scope: scope, Name: service}
			for _, follows := range []bool{true, false} {
				said := sourceSays(t, reader, ref, c, follows)
				if !strings.Contains(said, "logging driver") || !strings.Contains(said, "docker inspect "+c.ID[:12]) {
					t.Errorf("following %v, a log Docker can't read says %q", follows, said)
				}
			}
		})
	}
	for service, lc := range map[string]*docker.LogConfig{
		"cached": syslog(nil),
		"local":  {Type: "local"},
	} {
		t.Run(service, func(t *testing.T) {
			runWith(t, client, project, service, lc, `echo printed; sleep 1; echo later; sleep 60`)
			ref := &dockwellv1.ResourceRef{Kind: "service", Scope: scope, Name: service}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			seen := newCollector()
			go func() { _ = reader.Stream(ctx, &agentv1.LogsCall{Resource: ref, Follow: true, Tail: 10}, seen.send) }()
			waitFor(t, "what it printed", func() bool {
				seen.mu.Lock()
				defer seen.mu.Unlock()
				return seen.byLine["printed"] != nil && seen.byLine["later"] != nil
			})
			cancel()
			read := newCollector()
			if err := reader.Stream(t.Context(), &agentv1.LogsCall{Resource: ref, Tail: 10}, read.send); err != nil {
				t.Fatal(err)
			}
			if read.byLine["printed"] == nil {
				t.Errorf("a read of this log found %q", read.got())
			}
		})
	}
}

// EnvSmallDisk names a directory on a disk of a few MiB, which root can remount
// read-only with sudo, as CI's job makes one.
const envSmallDisk = "DOCKWELL_TEST_SMALL_DISK"

// smallDisk makes a directory of its own on the small disk, or skips.
func smallDisk(t *testing.T) string {
	t.Helper()
	disk := os.Getenv(envSmallDisk)
	if disk == "" {
		t.Skipf("%s names no small disk", envSmallDisk)
	}
	dir, err := os.MkdirTemp(disk, "logs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// waitForLine waits for a line in a container's log, as a real engine has it.
func waitForLine(t *testing.T, client *docker.Client, id, text string) {
	t.Helper()
	waitForLong(t, "the line "+text, func() bool {
		rd, err := client.ContainerLogs(t.Context(), id, false, docker.LogOptions{Tail: 1})
		if err != nil {
			return false
		}
		defer rd.Close()
		for {
			e, err := rd.Next()
			if errors.Is(err, io.EOF) || err != nil {
				return false
			}
			if string(e.Text) == text {
				return true
			}
		}
	})
}

// A log bigger than the disk, past what the 1 GiB rule would have stopped as this
// says there's room, as a disk that fills during a copy would: the replace goes on,
// nothing half done stays, and the archive works when there's room again.
func TestADiskThatFillsOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	dir := smallDisk(t)
	scope := dockertest.Scope(t)
	project := docker.Project(scope)
	dockertest.Cleanup(t, client, project)
	archive := &logs.Archive{Dir: filepath.Join(dir, "kept"), Client: client, Free: func(string) (uint64, error) { return 1 << 40, nil }}
	reader := &logs.Reader{Client: client, Archive: archive}
	rt := &container.Docker{Client: client, Logs: archive}
	ref := &dockwellv1.ResourceRef{Kind: "service", Scope: scope, Name: "web"}

	big := runReal(t, client, rt, project, "web", `head -c 6000000 /dev/urandom | base64 -w 120; echo written; sleep 60`, 1)
	waitForLine(t, client, big.ID, "written")
	next := runReal(t, client, rt, project, "web", `echo second; sleep 60`, 2)
	archive.WaitPruned()

	dir2 := filepath.Join(archive.Dir, project, "web")
	entries, err := os.ReadDir(dir2)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".copying-") {
			t.Errorf("a copy that couldn't finish stayed, as %s", e.Name())
		}
	}
	for _, k := range archive.List(project, "web") {
		t.Logf("what fit of it stayed, as %s", k.ContainerID)
	}
	seen := newCollector()
	if err := reader.Stream(t.Context(), &agentv1.LogsCall{Resource: ref, Tail: 10}, seen.send); err != nil {
		t.Fatal(err)
	}
	if seen.byLine["second"] == nil {
		t.Errorf("reading the new container found %q", seen.got())
	}

	// With room again, the next one is kept.
	if err := archive.Prune(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(archive.Dir, project)); err != nil {
		t.Fatal(err)
	}
	runReal(t, client, rt, project, "web", `echo third; sleep 60`, 3)
	archive.WaitPruned()
	kept := false
	for _, k := range archive.List(project, "web") {
		kept = kept || k.ContainerID == next.ID[:12]
	}
	if !kept {
		t.Error("with room again, a replace kept no log")
	}
}

// A disk that turns read-only, as a failing one does: what was kept still reads, a
// replace goes on without keeping a log, and a prune that can't remove anything says
// so and leaves it for when it can.
func TestADiskThatTurnsReadOnlyOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	dir := smallDisk(t)
	mount := func(how string) error {
		out, err := exec.CommandContext(context.WithoutCancel(t.Context()), "sudo", "-n", "mount", "-o", "remount,"+how, os.Getenv(envSmallDisk)).CombinedOutput()
		if err != nil {
			return errors.New(string(out))
		}
		return nil
	}
	scope := dockertest.Scope(t)
	project := docker.Project(scope)
	dockertest.Cleanup(t, client, project)
	at := time.Now()
	archive := &logs.Archive{
		Dir: filepath.Join(dir, "kept"), Client: client,
		Free: func(string) (uint64, error) { return 1 << 40, nil },
		Now:  func() time.Time { return at },
	}
	reader := &logs.Reader{Client: client, Archive: archive}
	rt := &container.Docker{Client: client, Logs: archive}
	ref := &dockwellv1.ResourceRef{Kind: "service", Scope: scope, Name: "web"}

	first := runReal(t, client, rt, project, "web", `echo first; sleep 60`, 1)
	waitForLine(t, client, first.ID, "first")
	runReal(t, client, rt, project, "web", `echo second; sleep 60`, 2)
	archive.WaitPruned()
	if got := archive.List(project, "web"); len(got) != 1 {
		t.Fatalf("the archive holds %d logs, want the first", len(got))
	}

	if err := mount("ro"); err != nil {
		t.Fatalf("making the disk read-only: %v", err)
	}
	t.Cleanup(func() { _ = mount("rw") })

	third := runReal(t, client, rt, project, "web", `echo third; sleep 60`, 3)
	archive.WaitPruned()
	waitForLine(t, client, third.ID, "third")
	read := newCollector()
	if err := reader.Stream(t.Context(), &agentv1.LogsCall{Resource: ref, IncludeKept: true, Revision: 1, Tail: 10}, read.send); err != nil {
		t.Fatal(err)
	}
	if read.byLine["first"] == nil {
		t.Errorf("what was kept doesn't read on a read-only disk: %q", read.got())
	}

	at = at.Add(logs.RetainFor + time.Hour)
	if err := archive.Prune(); err == nil {
		t.Error("a prune that couldn't remove a log past its time said nothing")
	}
	if got := archive.List(project, "web"); len(got) != 1 {
		t.Errorf("the archive lists %d logs on a read-only disk, want what it couldn't remove", len(got))
	}
	if err := mount("rw"); err != nil {
		t.Fatalf("making the disk writable again: %v", err)
	}
	if err := archive.Prune(); err != nil {
		t.Fatal(err)
	}
	if got := archive.List(project, "web"); len(got) != 0 {
		t.Errorf("the archive lists %d logs past their time once it could remove them", len(got))
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
