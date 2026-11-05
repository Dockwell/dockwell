// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// runImage runs image as the service web, on the only server.
func runImage(image string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "web"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: image}},
		}}},
	}
}

func logClient(c connect.HTTPClient, origin string) dockwellv1connect.LogServiceClient {
	return dockwellv1connect.NewLogServiceClient(c, origin, connect.WithProtoJSON())
}

// reader follows a stream of logs, keeping what it is sent.
type reader struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan error

	mu      sync.Mutex
	lines   []string
	sources map[string]*dockwellv1.LogSource
	servers map[string]*dockwellv1.ServerLogStatus
	gaps    []*dockwellv1.LogGap
	caught  int
	token   string
}

func follow(t *testing.T, c dockwellv1connect.LogServiceClient, req *dockwellv1.WatchLogsRequest) *reader {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	r := &reader{t: t, cancel: cancel, done: make(chan error, 1), sources: map[string]*dockwellv1.LogSource{}, servers: map[string]*dockwellv1.ServerLogStatus{}}
	stream, err := c.WatchLogs(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("WatchLogs: %v", err)
	}
	go func() {
		defer stream.Close()
		for stream.Receive() {
			r.take(stream.Msg())
		}
		r.done <- stream.Err()
	}()
	t.Cleanup(cancel)
	return r
}

func (r *reader) take(m *dockwellv1.WatchLogsResponse) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range m.GetSources() {
		r.sources[s.GetId()] = s
	}
	for _, s := range m.GetServers() {
		r.servers[s.GetServerName()] = s
	}
	if m.GetCaughtUp() {
		r.caught++
	}
	if m.GetResumeToken() != "" {
		r.token = m.GetResumeToken()
	}
	for _, c := range m.GetChunks() {
		if c.GetGap() != nil {
			r.gaps = append(r.gaps, c.GetGap())
		}
		for _, l := range c.GetLines() {
			r.lines = append(r.lines, l.GetText())
		}
	}
}

func (r *reader) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lines)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *reader) waitLines(want ...string) {
	r.t.Helper()
	eventually(r.t, fmt.Sprintf("lines %q", want), func() bool { return slices.Equal(r.got(), want) })
}

func (r *reader) waitCaughtUp() {
	r.t.Helper()
	eventually(r.t, "the stream to catch up", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.caught > 0
	})
}

// A service's logs follow it through a deploy. A deploy that fails and is rolled
// back leaves its logs readable, by its revision and its change, and so does the
// container it replaced. Rotation is on every container Dockwell makes, and none
// of it reads as drift.
func TestLogsThroughADeploy(t *testing.T) { storetest.Each(t, testLogsThroughADeploy) }

func testLogsThroughADeploy(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.Registry["ghcr.io/acme/shop:bad"] = dockertest.Image{Exits: true, Prints: []string{"ERROR can't reach the database", "    at main.go:12"}, Config: docker.Config{Cmd: []string{"shop"}}}
	c := changeClient(b, b.origin)
	lc := logClient(b, b.origin)

	first := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	if first.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the first deploy ended %v: %s", first.GetState(), first.GetError())
	}
	envID := first.GetPlan().GetResources()[0].GetResource().GetScope()
	name := docker.ContainerName(docker.Project(envID), "web")
	ctr := e.Container(name)
	if ctr == nil {
		t.Fatalf("Docker has %v", e.Containers())
	}
	// It carries its deploy, and has a bound on its log.
	if ctr.Config.Labels[container.LabelRevision] != "1" || ctr.Config.Labels[container.LabelChange] != first.GetId() {
		t.Errorf("labels = %v, want revision 1 and change %s", ctr.Config.Labels, first.GetId())
	}
	if lc := ctr.HostConfig.LogConfig; lc == nil || lc.Type != "json-file" || lc.Config["max-size"] != "10m" || lc.Config["max-file"] != "5" {
		t.Errorf("its log config = %+v, want json-file with rotation", lc)
	}
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Fatalf("drift after a deploy = %v, %v", drift.Msg.GetDrift(), err)
	}

	e.Print(name, docker.Stdout, "booted", "serving on :8080")
	r := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "web", Tail: 100})
	r.waitLines("booted", "serving on :8080")
	r.waitCaughtUp()
	src := r.sources[ctr.ID[:12]]
	if src.GetServerName() != "hz-1" || src.GetRevision() != 1 || src.GetChangeId() != first.GetId() || src.GetReplica() != 1 {
		t.Errorf("source = %v, want hz-1, revision 1, change %s, replica 1", src, first.GetId())
	}
	if r.servers["hz-1"].GetState() != dockwellv1.ServerLogState_SERVER_LOG_STATE_READING {
		t.Errorf("server = %v, want reading", r.servers["hz-1"])
	}

	// A deploy that fails: the new container prints why, and exits.
	e.Print(name, docker.Stdout, "last line of 1.4")
	failed := apply(t, c, propose(t, c, "Deploy bad", runImage("ghcr.io/acme/shop:bad")))
	if failed.GetState() != dockwellv1.ChangeState_CHANGE_STATE_FAILED {
		t.Fatalf("the bad deploy ended %v", failed.GetState())
	}
	var rollback *dockwellv1.Change
	eventually(t, "the rollback", func() bool {
		res, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: failed.GetId()}))
		if err != nil || res.Msg.GetChange().GetRolledBackBy() == "" {
			return false
		}
		rb, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: res.Msg.GetChange().GetRolledBackBy()}))
		if err != nil || rb.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			return false
		}
		rollback = rb.Msg.GetChange()
		return true
	})

	// The stream that follows saw it all, in order, through the deploy.
	eventually(t, "the failed deploy's lines in the stream", func() bool { return slices.Contains(r.got(), "ERROR can't reach the database") })

	// Both containers are gone from Docker, and readable all the same.
	res, err := lc.ListLogSources(t.Context(), connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Id: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	var kept, replaced *dockwellv1.LogSource
	for _, s := range res.Msg.GetSources() {
		if s.GetKept() == dockwellv1.LogKept_LOG_KEPT_FAILED {
			kept = s
		}
		if s.GetKept() == dockwellv1.LogKept_LOG_KEPT_REPLACED {
			replaced = s
		}
	}
	if kept == nil || kept.GetChangeId() != failed.GetId() || kept.GetState() != dockwellv1.LogSourceState_LOG_SOURCE_STATE_KEPT {
		t.Fatalf("sources = %v, want the failed deploy's kept, as failed", res.Msg.GetSources())
	}
	if replaced == nil || replaced.GetRevision() != 1 {
		t.Fatalf("sources = %v, want revision 1's kept, as replaced", res.Msg.GetSources())
	}
	if res.Msg.GetService().GetName() != "web" || res.Msg.GetService().GetDeleted() {
		t.Errorf("service = %v", res.Msg.GetService())
	}
	if len(res.Msg.GetSources()) != 3 {
		t.Errorf("sources = %d, want the two kept, and the one the rollback made", len(res.Msg.GetSources()))
	}

	// Choosing a deploy shows its logs.
	deploy := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "web", ChangeId: failed.GetId(), Once: true, Tail: 100})
	deploy.waitLines("ERROR can't reach the database", "    at main.go:12")
	if err := <-deploy.done; err != nil {
		t.Errorf("reading a kept log to its end = %v", err)
	}
	old, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web", Revision: 1}))
	if err != nil {
		t.Fatal(err)
	}
	var oldLines []string
	for _, c := range old.Msg.GetChunks() {
		for _, l := range c.GetLines() {
			oldLines = append(oldLines, l.GetText())
		}
	}
	if !slices.Equal(oldLines, []string{"booted", "serving on :8080", "last line of 1.4"}) {
		t.Errorf("revision 1's lines = %q, every one kept", oldLines)
	}

	// A search reaches what was kept, and says how far it looked.
	found, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web", Query: "DATABASE"}))
	if err != nil {
		t.Fatal(err)
	}
	if n := countLines(found.Msg.GetChunks()); n != 1 {
		t.Errorf("a search found %d lines, want 1", n)
	}
	if found.Msg.GetSearchedBackTo() == nil || found.Msg.GetSearchedLines() < 5 {
		t.Errorf("a search says it read %d lines back to %v, want at least 5", found.Msg.GetSearchedLines(), found.Msg.GetSearchedBackTo())
	}
	levels, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web", Levels: []dockwellv1.LogLevel{dockwellv1.LogLevel_LOG_LEVEL_ERROR}}))
	if err != nil {
		t.Fatal(err)
	}
	if n := countLines(levels.Msg.GetChunks()); n != 2 {
		t.Errorf("errors = %d lines, want the line and its stack frame", n)
	}

	// Nothing about any of it reads as drift, or forces a replace.
	drift, err = c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Fatalf("drift after a rollback = %v, %v", drift.Msg.GetDrift(), err)
	}
	if rollback.GetPlan().GetSummary().GetReplace() != 1 {
		t.Errorf("rollback's plan = %v", rollback.GetPlan().GetSummary())
	}
}

func countLines(chunks []*dockwellv1.LogChunk) int {
	n := 0
	for _, c := range chunks {
		n += len(c.GetLines())
	}
	return n
}

// A container with no rotation, as one made before Dockwell set it, isn't
// replaced for it: the plan is empty, and it isn't drift. The source says its log
// is unbounded, and the next deploy bounds it.
func TestRotationDoesntForceAReplace(t *testing.T) {
	storetest.Each(t, testRotationDoesntForceAReplace)
}

func testRotationDoesntForceAReplace(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	lc := logClient(b, b.origin)
	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	name := docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web")
	e.Edit(name, func(ctr *docker.Container) {
		ctr.HostConfig.LogConfig = &docker.LogConfig{Type: "json-file"}
		delete(ctr.Config.Labels, container.LabelRevision)
		delete(ctr.Config.Labels, container.LabelChange)
	})
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Fatalf("drift = %v, %v, want none", drift.Msg.GetDrift(), err)
	}
	same := propose(t, c, "Nothing new", runImage("ghcr.io/acme/shop:1.4"))
	if s := same.GetPlan().GetSummary(); s.GetReplace() != 0 || s.GetChange() != 0 {
		t.Errorf("the same spec plans %v, want nothing", s)
	}
	if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: same.GetId()})); err != nil {
		t.Fatal(err)
	}
	res, err := lc.ListLogSources(t.Context(), connect.NewRequest(&dockwellv1.ListLogSourcesRequest{Id: "web"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Msg.GetSources(); len(got) != 1 || !got[0].GetUnbounded() || got[0].GetRevision() != 0 {
		t.Errorf("sources = %v, want one, unbounded, of no known revision", got)
	}
	next := apply(t, c, propose(t, c, "Deploy 1.5", runImage("ghcr.io/acme/shop:1.5")))
	if next.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the deploy ended %v", next.GetState())
	}
	if lc := e.Container(name).HostConfig.LogConfig; lc == nil || lc.Config["max-size"] != "10m" {
		t.Errorf("after a deploy, its log config = %+v, want rotation", lc)
	}
}

// When a server's agent goes, a stream says so; when it returns, the stream
// carries on from where it was, with nothing twice and nothing missing, and the
// resume token does the same for a reader who was away too. A reader's going
// stops the reading on the agent at once.
func TestLogsCarryOnWhenAServerReturns(t *testing.T) {
	storetest.Each(t, testLogsCarryOnWhenAServerReturns)
}

func testLogsCarryOnWhenAServerReturns(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	lc := logClient(b, b.origin)
	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	name := docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web")

	e.Print(name, docker.Stdout, "one", "two")
	r := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "web"})
	r.waitLines("one", "two")

	server := f.lastDocker
	server.stop()
	eventually(t, "the stream to say the server is offline", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.servers["hz-1"].GetState() == dockwellv1.ServerLogState_SERVER_LOG_STATE_OFFLINE
	})
	if msg := r.servers["hz-1"].GetMessage(); !strings.Contains(msg, "the agent on hz-1 isn't connected") {
		t.Errorf("offline message = %q", msg)
	}
	// Lines written while it was away arrive when it returns.
	e.Print(name, docker.Stdout, "three")
	server.connect(t, f)
	r.waitLines("one", "two", "three")
	eventually(t, "the stream to say it reads again", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.servers["hz-1"].GetState() == dockwellv1.ServerLogState_SERVER_LOG_STATE_READING
	})
	e.Print(name, docker.Stdout, "four")
	r.waitLines("one", "two", "three", "four")

	// The reader goes away: Docker hears of it at once.
	r.mu.Lock()
	token := r.token
	r.mu.Unlock()
	gone := time.Now()
	r.cancel()
	if err := <-r.done; connect.CodeOf(err) != connect.CodeCanceled {
		t.Errorf("a stream whose reader left = %v", err)
	}
	eventually(t, "Docker's follow to end", func() bool { return e.Followers() == 0 })
	if took := time.Since(gone); took > time.Second {
		t.Errorf("Docker's follow took %s to end after the reader left", took)
	}

	// And carries on where it left off.
	e.Print(name, docker.Stdout, "five")
	again := follow(t, lc, &dockwellv1.WatchLogsRequest{Id: "web", ResumeToken: token})
	again.waitLines("five")
	e.Print(name, docker.Stdout, "six")
	again.waitLines("five", "six")
}

// A reader's token needs the grant, whatever its scope: a read-only token can
// read the server list, and not logs. Reading is in the audit log, once an hour
// for a reader and a service, and never on the timeline.
func TestReadingLogsNeedsTheGrantAndIsAudited(t *testing.T) {
	storetest.Each(t, testReadingLogsNeedsTheGrantAndIsAudited)
}

func testReadingLogsNeedsTheGrantAndIsAudited(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	f.addDockerServer(t)
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	tokens := dockwellv1connect.NewTokenServiceClient(b, b.origin, connect.WithProtoJSON())

	made := func(scope dockwellv1.TokenScope, grants ...dockwellv1.TokenGrant) dockwellv1connect.LogServiceClient {
		res, err := tokens.CreateToken(t.Context(), connect.NewRequest(&dockwellv1.CreateTokenRequest{Name: "CI " + scope.String(), Scope: scope, Grants: grants}))
		if err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		if got := res.Msg.GetToken().GetGrants(); !slices.Equal(got, grants) {
			t.Errorf("the token's grants = %v, want %v", got, grants)
		}
		return logClient(bearer{token: res.Msg.GetSecret(), rt: h2cTransport(t, f.api)}, "http://"+f.api)
	}
	refuses := func(client dockwellv1connect.LogServiceClient, what string) {
		t.Helper()
		_, err := client.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "token_logs" || !strings.Contains(err.Error(), "can't read logs") {
			t.Errorf("%s reading logs = %v, want token_logs", what, err)
		}
		stream, err := client.WatchLogs(t.Context(), connect.NewRequest(&dockwellv1.WatchLogsRequest{Id: "web"}))
		if err == nil {
			defer stream.Close()
			for stream.Receive() {
				t.Errorf("%s got a message of logs", what)
			}
			err = stream.Err()
		}
		if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "token_logs" {
			t.Errorf("%s following logs = %v, want token_logs", what, err)
		}
	}
	refuses(made(dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), "a read-only token")
	refuses(made(dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), "a read-write token")

	// Other reads don't need it.
	plain := made(dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY)
	_ = plain
	withGrant := made(dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenGrant_TOKEN_GRANT_LOGS)
	for range 3 {
		if _, err := withGrant.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web", Query: "s3cret-search"})); err != nil {
			t.Fatalf("a token with the grant reading logs = %v", err)
		}
	}
	// A session can.
	if _, err := logClient(b, b.origin).ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web"})); err != nil {
		t.Fatalf("a session reading logs = %v", err)
	}

	var events []store.Event
	err := f.store.View(t.Context(), func(tx *store.Tx) error {
		var err error
		events, err = tx.ListEvents(t.Context(), 200)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var reads []store.Event
	for i := range events {
		if events[i].Action == "logs.read" {
			reads = append(reads, events[i])
		}
		if strings.Contains(events[i].Detail, "s3cret-search") {
			t.Errorf("the audit log holds what was searched for: %v", events[i])
		}
	}
	if len(reads) != 2 {
		t.Fatalf("logs.read events = %v, want one for the token, and one for the session", reads)
	}
	surfaces := []string{reads[0].Surface, reads[1].Surface}
	if !slices.Contains(surfaces, store.SurfaceAPI) || !slices.Contains(surfaces, store.SurfacePanel) {
		t.Errorf("surfaces = %v", surfaces)
	}
	// None of it is on the timeline.
	timeline, err := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON()).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range timeline.Msg.GetEvents() {
		if e.GetAction() == "logs.read" {
			t.Errorf("logs.read is on the timeline: %v", e)
		}
	}
}

// A container writing as fast as it can, with a reader that has stopped reading,
// holds up nothing else on its server: the agent's stats keep coming, its plans
// and applies go through, and everything the container wrote is either delivered
// or counted.
func TestANoisyLogDoesntStallTheAgent(t *testing.T) {
	storetest.Each(t, testANoisyLogDoesntStallTheAgent)
}

func testANoisyLogDoesntStallTheAgent(t *testing.T, backend store.Backend) {
	if backend != store.SQLite {
		t.Skip("once is enough: this test is about the agent, and takes seconds")
	}
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	lc := logClient(b, b.origin)
	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	name := docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web")

	// A reader that reads the first message, then nothing: flow control stops
	// the control plane's send, then what it reads from the agent.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stuck, err := lc.WatchLogs(ctx, connect.NewRequest(&dockwellv1.WatchLogsRequest{Id: "web", Tail: -1}))
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.Close()
	if !stuck.Receive() {
		t.Fatalf("no first message: %v", stuck.Err())
	}
	flood := e.Flood(name, 512)
	defer flood.Stop()

	// While it floods, a plan's round trip stays quick, and a change applies.
	eventually(t, "the flood to get going", func() bool { return flood.Lines() > 10_000 })
	session, ok := f.agents.Session(f.lastDocker.id.ServerID)
	if !ok {
		t.Fatal("no session")
	}
	slowest := time.Duration(0)
	for i := range 20 {
		start := time.Now()
		ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		_, err := session.Observe(ctx, []resource.Ref{{Kind: "service", Scope: done.GetPlan().GetResources()[0].GetResource().GetScope(), Name: "web"}})
		stop()
		if err != nil {
			t.Fatalf("call %d through the agent's stream during a flood = %v", i, err)
		}
		slowest = max(slowest, time.Since(start))
		time.Sleep(50 * time.Millisecond)
	}
	if slowest > 2*time.Second {
		t.Errorf("the slowest call through the agent's stream took %s during a flood", slowest)
	}
	t.Logf("wrote %d lines; slowest observe through the agent's stream during the flood: %s", flood.Lines(), slowest)
	next := apply(t, c, propose(t, c, "Deploy during a flood", runImage("ghcr.io/acme/shop:1.5")))
	if next.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Errorf("a deploy during a flood ended %v: %s", next.GetState(), next.GetError())
	}
}

func TestLogsREST(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	done := apply(t, c, propose(t, c, "Run web", runImage("ghcr.io/acme/shop:1.4")))
	name := docker.ContainerName(docker.Project(done.GetPlan().GetResources()[0].GetResource().GetScope()), "web")
	base := time.Now().Add(-time.Hour).UTC()
	for i := range 12 {
		e.PrintAt(name, base.Add(time.Duration(i)*time.Second), docker.Stdout, fmt.Sprintf("line %02d", i))
	}
	token := func() string {
		res, err := dockwellv1connect.NewTokenServiceClient(b, b.origin, connect.WithProtoJSON()).CreateToken(t.Context(), connect.NewRequest(&dockwellv1.CreateTokenRequest{
			Name: "CI", Scope: dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, Grants: []dockwellv1.TokenGrant{dockwellv1.TokenGrant_TOKEN_GRANT_LOGS},
			Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_TOKEN, ""),
		}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetSecret()
	}()
	get := func(path string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+f.api+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, res.StatusCode, buf.String())
		}
		return buf.String()
	}
	body := get("/v1/services/web/logs?pageSize=2&query=line")
	if !strings.Contains(body, "line 11") || !strings.Contains(body, "line 10") || strings.Contains(body, "line 09") {
		t.Errorf("a page of 2 = %s, want the newest two", body)
	}
	if !strings.Contains(get("/v1/services/web/logSources"), `"serverName":"hz-1"`) {
		t.Error("the sources didn't name the server")
	}

	// Pages go backward in time, and meet: every line once.
	lc := logClient(b, b.origin)
	token2 := ""
	var all []string
	for page := 0; ; page++ {
		res, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web", PageSize: 5, PageToken: token2}))
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, c := range res.Msg.GetChunks() {
			for _, l := range c.GetLines() {
				lines = append(lines, l.GetText())
			}
		}
		all = append(lines, all...)
		token2 = res.Msg.GetNextPageToken()
		if token2 == "" {
			break
		}
		if page > 5 {
			t.Fatal("too many pages")
		}
	}
	want := []string{}
	for i := range 12 {
		want = append(want, fmt.Sprintf("line %02d", i))
	}
	if !slices.Equal(all, want) {
		t.Errorf("paged lines = %q, want every line once, in order", all)
	}

	// A bad token says so.
	_, err := lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web", PageToken: "nonsense!"}))
	if reasonOf(err) != "invalid" {
		t.Errorf("a bad page token = %v", err)
	}
	stream, err := lc.WatchLogs(t.Context(), connect.NewRequest(&dockwellv1.WatchLogsRequest{Id: "web", ResumeToken: "nonsense!"}))
	if err == nil {
		defer stream.Close()
		for stream.Receive() {
			t.Errorf("a bad resume token got a message: %v", stream.Msg())
		}
		err = stream.Err()
	}
	if reasonOf(err) != "invalid" {
		t.Errorf("a bad resume token = %v", err)
	}
	_, err = lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "nosuch"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a service that doesn't exist = %v", err)
	}
	_, err = lc.ReadLogs(t.Context(), connect.NewRequest(&dockwellv1.ReadLogsRequest{Id: "web", Last: durationpb.New(time.Minute), Since: done.GetCreateTime()}))
	if reasonOf(err) != "invalid" {
		t.Errorf("since and last together = %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
