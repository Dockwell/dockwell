// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	agentfiles "github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func fileClient(c connect.HTTPClient, origin string) dockwellv1connect.FileServiceClient {
	return dockwellv1connect.NewFileServiceClient(c, origin, connect.WithProtoJSON())
}

// fileEvents are the audit log's events about files, oldest first.
func fileEvents(t *testing.T, f *fullServer) []store.Event {
	t.Helper()
	var events []store.Event
	err := f.store.View(t.Context(), func(tx *store.Tx) error {
		var err error
		events, err = tx.ListEvents(t.Context(), 1000)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	events = slices.DeleteFunc(events, func(e store.Event) bool { return !strings.HasPrefix(e.Action, "file.") })
	slices.Reverse(events)
	return events
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAPersonReadsAServersFiles(t *testing.T) { storetest.Each(t, testAPersonReadsAServersFiles) }

func testAPersonReadsAServersFiles(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	server := f.addServer(t, false)
	id := server.id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "nginx.conf")
	writeFile(t, config, "listen 80; # s3cret-in-a-file\n")
	c := fileClient(b, b.origin)

	list, err := c.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: id, Path: dir}))
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if got := list.Msg.GetEntries(); len(got) != 1 || got[0].GetName() != "nginx.conf" || list.Msg.GetPath() != dir {
		t.Fatalf("listed %v", list.Msg)
	}
	if got := fileEvents(t, f); len(got) != 0 {
		t.Errorf("a listing went on record: %v", got)
	}

	read, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: config}))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if read.Msg.GetContent() != "listen 80; # s3cret-in-a-file\n" || read.Msg.GetBinary() {
		t.Fatalf("read %v", read.Msg)
	}
	events := fileEvents(t, f)
	if len(events) != 1 {
		t.Fatalf("the audit log has %v", events)
	}
	e := events[0]
	if e.Action != "file.read" || e.Subject != config || e.Target != id || e.Surface != store.SurfacePanel || !strings.HasPrefix(e.Actor, "user:") ||
		!strings.Contains(e.Detail, config) || !strings.Contains(e.Detail, "hz-1") {
		t.Errorf("the read went on record as %+v", e)
	}
	if strings.Contains(e.Detail, "s3cret") {
		t.Errorf("the audit log holds the file's contents: %s", e.Detail)
	}

	// REST takes the same paths, as a client of its own would.
	code, _, body := callREST(t, b, http.MethodGet, "/v1/servers/"+id+"/files?path="+url.QueryEscape(dir), nil, "")
	if code != http.StatusOK || !strings.Contains(body, "nginx.conf") {
		t.Errorf("GET files: %d %s", code, body)
	}
	code, _, body = callREST(t, b, http.MethodPost, "/v1/servers/"+id+"/files:read", nil, `{"path":"`+config+`"}`)
	if code != http.StatusOK || !strings.Contains(body, "listen 80") {
		t.Errorf("POST files:read: %d %s", code, body)
	}
}

// No token reaches a server's files, whatever its scope, and refusing it asks
// nothing of the server.
func TestNoTokenReachesAServersFiles(t *testing.T) {
	storetest.Each(t, testNoTokenReachesAServersFiles)
}

func testNoTokenReachesAServersFiles(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	config := filepath.Join(t.TempDir(), "config")
	writeFile(t, config, "s3cret")
	for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
		token := createToken(t, b, scope)
		script := fileClient(bearer{token: token, rt: b.client.Transport}, b.origin)
		_, err := script.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: id, Path: "/"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("%v listing files = %v, want needs_session", scope, err)
		}
		_, err = script.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: config}))
		if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("%v reading a file = %v, want needs_session", scope, err)
		}
		_, err = script.SaveFile(t.Context(), connect.NewRequest(&dockwellv1.SaveFileRequest{ServerId: id, Path: config, Content: "written", Version: "v"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("%v saving a file = %v, want needs_session", scope, err)
		}
		// A working session beside the token changes nothing: the token alone counts.
		both := fileClient(tokenAndCookie{browser: b, token: token}, b.origin)
		_, err = both.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: config}))
		if reasonOf(err) != authapi.ReasonNeedsSession {
			t.Errorf("%v with a session beside it = %v, want needs_session", scope, err)
		}
		for _, r := range []struct{ verb, path, body string }{
			{http.MethodGet, "/v1/servers/" + id + "/files?path=/", ""},
			{http.MethodPost, "/v1/servers/" + id + "/files:read", `{"path":"` + config + `"}`},
			{http.MethodPost, "/v1/servers/" + id + "/files:save", `{"path":"` + config + `","content":"written","version":"v"}`},
		} {
			code, reason, body := callREST(t, b, r.verb, r.path, map[string]string{"Authorization": "Bearer " + token}, r.body)
			if code != http.StatusForbidden || reason != authapi.ReasonNeedsSession || strings.Contains(body, "s3cret") {
				t.Errorf("%v: %s %s = %d %s %s", scope, r.verb, r.path, code, reason, body)
			}
		}
	}
	if got := fileEvents(t, f); len(got) != 0 {
		t.Errorf("a refused token got as far as the audit log: %v", got)
	}
	if got, err := os.ReadFile(config); err != nil || string(got) != "s3cret" {
		t.Errorf("the file holds %q: %v", got, err)
	}
}

func TestAPersonSavesAFile(t *testing.T) { storetest.Each(t, testAPersonSavesAFile) }

func testAPersonSavesAFile(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "app.env")
	writeFile(t, config, "PORT=80\n")
	c := fileClient(b, b.origin)
	read, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: config}))
	if err != nil {
		t.Fatal(err)
	}
	changes := changeClient(b, b.origin)
	before, err := changes.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
	if err != nil {
		t.Fatal(err)
	}

	saved, err := c.SaveFile(t.Context(), connect.NewRequest(&dockwellv1.SaveFileRequest{
		ServerId: id, Path: config, Content: "PORT=8080 # s3cret-in-a-file\n", Version: read.Msg.GetVersion(),
	}))
	if err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if got, err := os.ReadFile(config); err != nil || string(got) != "PORT=8080 # s3cret-in-a-file\n" {
		t.Fatalf("the file holds %q: %v", got, err)
	}
	if saved.Msg.GetVersion() == "" || saved.Msg.GetVersion() == read.Msg.GetVersion() || saved.Msg.GetPath() != config {
		t.Errorf("saved as %v", saved.Msg)
	}

	// A save isn't a change: no plan, no revision.
	after, err := changes.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Msg.GetChanges()) != len(before.Msg.GetChanges()) {
		t.Errorf("a save made a change: %v", after.Msg.GetChanges())
	}

	events := fileEvents(t, f)
	var actions []string
	for i := range events {
		actions = append(actions, events[i].Action)
		if strings.Contains(events[i].Detail, "s3cret") {
			t.Errorf("the audit log holds what was saved: %s", events[i].Detail)
		}
	}
	if want := []string{"file.read", "file.saving", "file.save"}; !slices.Equal(actions, want) {
		t.Fatalf("the audit log has %v, want %v", actions, want)
	}
	last := events[2]
	if last.Subject != config || last.Target != id || !strings.HasPrefix(last.Actor, "user:") || last.Surface != store.SurfacePanel {
		t.Errorf("the save went on record as %+v", last)
	}

	// It's on the timeline as what it is, under its server, after the read.
	timeline := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON())
	list, err := timeline.ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{ServerId: id}))
	if err != nil {
		t.Fatal(err)
	}
	var seen []*dockwellv1.Event
	for _, e := range list.Msg.GetEvents() {
		if strings.HasPrefix(e.GetAction(), "file.") {
			seen = append(seen, e)
		}
	}
	slices.Reverse(seen)
	if len(seen) != 2 || seen[0].GetAction() != "file.read" {
		t.Fatalf("the timeline has %v", seen)
	}
	e := seen[1]
	if e.GetAction() != "file.save" || e.GetKind() != dockwellv1.EventKind_EVENT_KIND_SERVER ||
		e.GetSubject().GetId() != id || e.GetSubject().GetName() != "hz-1" || e.GetSubject().GetPath() != config || e.GetActor() == nil {
		t.Errorf("the timeline says %v", e)
	}

	// A save over what changed since writes nothing, and isn't on the timeline as saved.
	writeFile(t, config, "PORT=9000\n")
	_, err = c.SaveFile(t.Context(), connect.NewRequest(&dockwellv1.SaveFileRequest{
		ServerId: id, Path: config, Content: "PORT=1\n", Version: saved.Msg.GetVersion(),
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "file_changed" {
		t.Fatalf("saving over what changed = %v, want file_changed", err)
	}
	if got, err := os.ReadFile(config); err != nil || string(got) != "PORT=9000\n" {
		t.Fatalf("the file holds %q: %v", got, err)
	}
	if got := fileEvents(t, f); len(got) != 5 || got[3].Action != "file.saving" || got[4].Action != "file.save_failed" {
		t.Errorf("after a refused save the audit log has %v", got)
	}

	// REST takes it too.
	again, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: config}))
	if err != nil {
		t.Fatal(err)
	}
	code, _, body := callREST(t, b, http.MethodPost, "/v1/servers/"+id+"/files:save", nil,
		`{"path":"`+config+`","content":"PORT=2\n","version":"`+again.Msg.GetVersion()+`"}`)
	if code != http.StatusOK || !strings.Contains(body, config) {
		t.Errorf("POST files:save: %d %s", code, body)
	}
}

func TestASaveThatCantBeMadeSaysWhy(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	server := f.addServer(t, false)
	id := server.id.ServerID
	config := filepath.Join(t.TempDir(), "config")
	writeFile(t, config, "x")
	key := filepath.Join(server.dataDir, "control", "key")
	writeFile(t, key, "the install's key")
	c := fileClient(b, b.origin)
	save := func(path, content, version string) error {
		_, err := c.SaveFile(t.Context(), connect.NewRequest(&dockwellv1.SaveFileRequest{ServerId: id, Path: path, Content: content, Version: version}))
		return err
	}
	for _, s := range []struct {
		name, path, content, version, reason string
	}{
		{"no version", config, "y", "", "invalid"},
		{"too long", config, strings.Repeat("y", 1<<20+1), "v", "invalid"},
		{"a relative path", "etc/config", "y", "v", "invalid"},
		{"what's refused", key, "y", "v", "file_refused"},
		{"nothing there", config + ".nope", "y", "v", "not_found"},
	} {
		t.Run(s.name, func(t *testing.T) {
			if err := save(s.path, s.content, s.version); reasonOf(err) != s.reason {
				t.Errorf("got %v, want %s", err, s.reason)
			}
		})
	}
	if got, err := os.ReadFile(key); err != nil || string(got) != "the install's key" {
		t.Errorf("the key holds %q: %v", got, err)
	}
	var failed []string
	events := fileEvents(t, f)
	for i := range events {
		if events[i].Action == "file.save_failed" {
			failed = append(failed, events[i].Subject)
		}
	}
	if len(failed) != 5 {
		t.Errorf("failed saves on record: %v", failed)
	}
	// What was refused, and what's on the timeline says so, as it would a read.
	if on := timelineFiles(t, b, &dockwellv1.ListEventsRequest{ServerId: id}); len(on) != 5 ||
		on[0].GetAction() != "file.save_failed" || on[0].GetKind() != dockwellv1.EventKind_EVENT_KIND_SERVER || on[0].GetSubject().GetPath() == "" {
		t.Errorf("the timeline has %v", on)
	}
}

// What can't go on record isn't written.
func TestASaveThatCantGoOnRecordWritesNothing(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	config := filepath.Join(t.TempDir(), "config")
	writeFile(t, config, "before")
	c := fileClient(b, b.origin)
	read, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: config}))
	if err != nil {
		t.Fatal(err)
	}
	f.execSQL(t, `CREATE TRIGGER keep_out BEFORE INSERT ON audit_events WHEN NEW.action = 'file.saving' BEGIN SELECT RAISE(ABORT, 'no room'); END`)
	_, err = c.SaveFile(t.Context(), connect.NewRequest(&dockwellv1.SaveFileRequest{
		ServerId: id, Path: config, Content: "after", Version: read.Msg.GetVersion(),
	}))
	if err == nil {
		t.Fatal("the save went ahead with nothing on record")
	}
	if got, err := os.ReadFile(config); err != nil || string(got) != "before" {
		t.Fatalf("the file holds %q: %v", got, err)
	}
}

// A dead mount is the server's trouble, not its connection's: a person hears so
// within a bound, and the agent can always end its stream and start another, with
// the call still stuck, as nothing can stop one.
func TestAPathThatStopsAnsweringLeavesTheServerOnline(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	server := f.addServer(t, false)
	id := server.id.ServerID
	dir := t.TempDir()
	dead := filepath.Join(dir, "mnt")
	writeFile(t, filepath.Join(dead, "f"), "x")
	well := filepath.Join(dir, "well")
	writeFile(t, well, "fine")

	server.stop()
	waitConnected(t, f, id, false)
	gone := make(chan struct{})
	t.Cleanup(func() { close(gone) })
	disk := agentfiles.New(server.dataDir, nil, nil)
	disk.AnswerWithin = 50 * time.Millisecond
	disk.Touching = func(p string) {
		if strings.HasPrefix(p, dead) {
			<-gone
		}
	}
	server.disk = disk
	server.connect(t, f)

	c := fileClient(b, b.origin)
	_, err := c.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: id, Path: dead}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded || reasonOf(err) != "path_not_answering" || !strings.Contains(err.Error(), "isn't answering") {
		t.Fatalf("listing a dead mount = %v", err)
	}
	if read, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: well})); err != nil || read.Msg.GetContent() != "fine" {
		t.Fatalf("a healthy path = %v, %v", read, err)
	}

	stopped := make(chan struct{})
	go func() { server.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent is waiting on a call that will never return, so this server would stay offline")
	}
	waitConnected(t, f, id, false)
	server.connect(t, f)
	if read, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: well})); err != nil || read.Msg.GetContent() != "fine" {
		t.Fatalf("after reconnecting = %v, %v", read, err)
	}
}

// Saving what lets someone back in takes a passkey, for that path on that server,
// and reads as a warning on Activity. Being asked isn't a failed save. Anywhere else
// takes nothing.
func TestSavingAWayBackInTakesAPasskey(t *testing.T) {
	storetest.Each(t, testSavingAWayBackInTakesAPasskey)
}

func testSavingAWayBackInTakesAPasskey(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, ".ssh", "authorized_keys")
	writeFile(t, keys, "ssh-ed25519 AAAA admin\n")
	via := filepath.Join(dir, "config.yml")
	if err := os.Symlink(keys, via); err != nil {
		t.Fatal(err)
	}
	c := fileClient(b, b.origin)
	read, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: via}))
	if err != nil {
		t.Fatal(err)
	}
	if !read.Msg.GetWayBackIn() || read.Msg.GetPath() != keys {
		t.Fatalf("a read says %v", read.Msg)
	}
	save := func(confirmed, proof string) (*connect.Response[dockwellv1.SaveFileResponse], error) {
		return c.SaveFile(t.Context(), connect.NewRequest(&dockwellv1.SaveFileRequest{
			ServerId: id, Path: via, Content: "typed\n", Version: read.Msg.GetVersion(), ConfirmedPath: confirmed, Proof: proof,
		}))
	}
	untouched := func() {
		t.Helper()
		if got, err := os.ReadFile(keys); err != nil || string(got) != "ssh-ed25519 AAAA admin\n" {
			t.Fatalf("the file holds %q: %v", got, err)
		}
	}
	action := dockwellv1.StepUpAction_STEP_UP_ACTION_SAVE_WAY_BACK_IN
	target := id + ":" + keys
	if _, err := save("", ""); reasonOf(err) != "needs_step_up" {
		t.Fatalf("with no proof = %v", err)
	}
	if _, err := save(keys, ""); reasonOf(err) != "needs_step_up" {
		t.Fatalf("saying a path was confirmed, with no proof = %v", err)
	}
	untouched()
	for _, e := range timelineFiles(t, b, &dockwellv1.ListEventsRequest{ServerId: id}) {
		if e.GetAction() == "file.save_failed" {
			t.Errorf("being asked for a passkey reads as a save that failed: %v", e)
		}
	}
	for what, other := range map[string]string{
		"another path":   stepUpFor(t, b, action, id+":"+keys+".old"),
		"another server": stepUpFor(t, b, action, "srv_another:"+keys),
		"another action": stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_REVEAL_SECRET, target),
	} {
		if _, err := save(keys, other); reasonOf(err) != "needs_step_up" {
			t.Fatalf("with a proof for %s = %v", what, err)
		}
		untouched()
	}
	// The path comes from the request, so a proof for one path can't be sent for another.
	if _, err := save(via, stepUpFor(t, b, action, target)); reasonOf(err) != "needs_step_up" {
		t.Fatalf("with the proof, naming another path = %v", err)
	}
	untouched()
	saved, err := save(keys, stepUpFor(t, b, action, target))
	if err != nil || !saved.Msg.GetWayBackIn() {
		t.Fatalf("with its proof = %v, %v", saved, err)
	}
	if got, err := os.ReadFile(keys); err != nil || string(got) != "typed\n" {
		t.Fatalf("the file holds %q: %v", got, err)
	}
	waitForFileEvent(t, b, id, "file.save_way_back_in", keys)
}

// heldWrite is a server whose agent holds each save as it begins to write, past where
// it can stop, with a person signed in to save.
func heldWrite(t *testing.T, f *fullServer, answerWithin time.Duration) (server *testServer, reached chan struct{}, release func()) {
	t.Helper()
	server = f.addServer(t, false)
	server.stop()
	waitConnected(t, f, server.id.ServerID, false)
	reached = make(chan struct{}, 1)
	gone := make(chan struct{})
	release = sync.OnceFunc(func() { close(gone) })
	t.Cleanup(release)
	disk := agentfiles.New(server.dataDir, nil, nil)
	disk.AnswerWithin = answerWithin
	disk.Writing = func(string) {
		reached <- struct{}{}
		<-gone
	}
	server.disk = disk
	server.connect(t, f)
	return server, reached, release
}

// waitForFileEvent waits for the timeline to have an action on a path.
func waitForFileEvent(t *testing.T, b *browser, server, action, path string) *dockwellv1.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, e := range timelineFiles(t, b, &dockwellv1.ListEventsRequest{ServerId: server}) {
			if e.GetAction() == action && e.GetSubject().GetPath() == path {
				return e
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the timeline never had %s on %s: %v", action, path, timelineFiles(t, b, &dockwellv1.ListEventsRequest{ServerId: server}))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A save left behind as it wrote may have landed, so that's what the person hears and
// what goes on record, under where it got to, not "couldn't save". When it ends, the
// record says how, as whoever saved it, though nobody was waiting by then.
func TestASaveThatMayHaveLandedSaysSoThenHowItEnded(t *testing.T) {
	storetest.Each(t, testASaveThatMayHaveLandedSaysSoThenHowItEnded)
}

func testASaveThatMayHaveLandedSaysSoThenHowItEnded(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	server, reached, release := heldWrite(t, f, 50*time.Millisecond)
	id := server.id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "config")
	writeFile(t, target, "before")
	via := filepath.Join(dir, "current")
	if err := os.Symlink(target, via); err != nil {
		t.Fatal(err)
	}
	c := fileClient(b, b.origin)
	read, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: via}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SaveFile(t.Context(), connect.NewRequest(&dockwellv1.SaveFileRequest{ServerId: id, Path: via, Content: "after", Version: read.Msg.GetVersion()}))
	if reasonOf(err) != "save_may_land" || !strings.Contains(err.Error(), "may still land") {
		t.Fatalf("a save left behind as it wrote = %v", err)
	}
	<-reached
	unknown := waitForFileEvent(t, b, id, "file.save_unknown", target)
	for _, e := range timelineFiles(t, b, &dockwellv1.ListEventsRequest{ServerId: id}) {
		if e.GetAction() == "file.save_failed" {
			t.Errorf("it reads as a save that failed: %v", e)
		}
	}
	release()
	landed := waitForFileEvent(t, b, id, "file.save", target)
	if landed.GetActor() == nil || landed.GetActor().GetId() != unknown.GetActor().GetId() {
		t.Errorf("it's on record as %v, and the save was %v's", landed.GetActor(), unknown.GetActor())
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "after" {
		t.Fatalf("the file holds %q: %v", got, err)
	}
}

// A stream that drops as a save writes leaves it either way too, and what's on
// record comes from the agent's next stream.
func TestASaveWhoseStreamDropsMayHaveLanded(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	server, reached, release := heldWrite(t, f, time.Minute)
	id := server.id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config")
	writeFile(t, config, "before")
	c := fileClient(b, b.origin)
	read, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: config}))
	if err != nil {
		t.Fatal(err)
	}
	saved := make(chan error, 1)
	go func() {
		_, err := c.SaveFile(context.Background(), connect.NewRequest(&dockwellv1.SaveFileRequest{ServerId: id, Path: config, Content: "after", Version: read.Msg.GetVersion()}))
		saved <- err
	}()
	<-reached
	server.stop()
	if err := <-saved; reasonOf(err) != "server_offline" || !strings.Contains(err.Error(), "may or may not have saved") {
		t.Fatalf("a save on a stream that dropped = %v", err)
	}
	waitForFileEvent(t, b, id, "file.save_unknown", config)
	waitConnected(t, f, id, false)
	server.connect(t, f)
	release()
	waitForFileEvent(t, b, id, "file.save", config)
}

// timelineFiles are the timeline's events about files, as Activity reads them.
func timelineFiles(t *testing.T, b *browser, request *dockwellv1.ListEventsRequest) []*dockwellv1.Event {
	t.Helper()
	list, err := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON()).ListEvents(t.Context(), connect.NewRequest(request))
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(list.Msg.GetEvents(), func(e *dockwellv1.Event) bool { return !strings.HasPrefix(e.GetAction(), "file.") })
}

// Not asking for a passkey to read a file rests on this: what a stolen session
// read can be seen, by the person it was stolen from, without a database.
func TestWhatsReadIsOnActivity(t *testing.T) { storetest.Each(t, testWhatsReadIsOnActivity) }

func testWhatsReadIsOnActivity(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "app.env")
	writeFile(t, config, "s3cret-in-a-file\n")
	c := fileClient(b, b.origin)
	if _, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: config})); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "id_rsa")
	if _, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: missing})); reasonOf(err) != "not_found" {
		t.Fatal(err)
	}
	got := timelineFiles(t, b, &dockwellv1.ListEventsRequest{ServerId: id})
	slices.Reverse(got)
	if len(got) != 2 {
		t.Fatalf("the timeline has %v", got)
	}
	for i, want := range []struct{ action, path string }{{"file.read", config}, {"file.refuse", missing}} {
		e := got[i]
		if e.GetAction() != want.action || e.GetKind() != dockwellv1.EventKind_EVENT_KIND_ACCESS || e.GetActor() == nil ||
			e.GetSubject().GetId() != id || e.GetSubject().GetName() != "hz-1" || e.GetSubject().GetPath() != want.path {
			t.Errorf("the timeline says %v", e)
		}
	}
	access := timelineFiles(t, b, &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS}})
	if len(access) != 2 {
		t.Errorf("filtering by access keeps %v", access)
	}
	// A plain listing isn't there: it returns names, and it runs on every click.
	if _, err := c.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: id, Path: dir})); err != nil {
		t.Fatal(err)
	}
	if after := timelineFiles(t, b, &dockwellv1.ListEventsRequest{ServerId: id}); len(after) != 2 {
		t.Errorf("a listing went on the timeline: %v", after)
	}
}

// Someone looking for credentials mostly misses, so a miss has to leave a mark as
// a hit does, whatever kind of refusal it was.
func TestEveryRefusalIsOnRecord(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	server := f.addServer(t, false)
	id := server.id.ServerID
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	shut := filepath.Join(dir, "shut")
	writeFile(t, filepath.Join(shut, "f"), "x")
	if err := os.Chmod(shut, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shut, 0o755) })
	c := fileClient(b, b.origin)
	read := func(path string) {
		t.Helper()
		if _, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: path})); err == nil {
			t.Fatalf("read %s", path)
		}
	}
	list := func(path string) {
		t.Helper()
		if _, err := c.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: id, Path: path})); err == nil {
			t.Fatalf("listed %s", path)
		}
	}
	missing := filepath.Join(dir, "id_rsa")
	read(missing)                  // nothing there
	read(fifo)                     // not a file
	read(dir)                      // a directory
	read(filepath.Join(shut, "f")) // denied, unless root runs this
	read(dir + "/a/../b")          // a path that doesn't say what it means
	list("/proc")                  // a refused listing
	list(missing)                  // a listing of nothing
	want := 7
	seen := map[string]bool{}
	events := fileEvents(t, f)
	for i := range events {
		e := &events[i]
		if e.Action != "file.refuse" || e.Target != id {
			t.Errorf("went on record as %+v", e)
		}
		seen[e.Subject] = true
	}
	if len(events) != want {
		t.Fatalf("%d of %d refusals are on record: %v", len(events), want, seen)
	}
	if !seen[fifo] || !seen[missing] || !seen["/proc"] {
		t.Errorf("on record: %v", seen)
	}

	// Reading from a server that's gone is a failed read too. Listing one isn't
	// kept: the panel asks again by itself while it reconnects.
	server.stop()
	waitConnected(t, f, id, false)
	read(fifo)
	if _, err := c.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: id, Path: dir})); reasonOf(err) != "server_offline" {
		t.Fatalf("listing with the server gone = %v", err)
	}
	if got := fileEvents(t, f); len(got) != want+1 {
		t.Errorf("with the server gone, the audit log went from %d to %d", want, len(got))
	}
}

// A file's name can hold anything but a slash, and a link can lead through one,
// which is how a newline or a terminal's escape codes could reach a log or a screen.
func TestAPathIsCleanedBeforeItsStored(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dir := t.TempDir()
	evil := dir + "/evil\n\x1b[31m\u202e"
	writeFile(t, evil+"/f", "x")
	if err := os.Symlink(evil+"/f", dir+"/link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil+"/nothing", dir+"/broken"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+"/gone\n\x1b[31m", dir+"/ends-badly"); err != nil {
		t.Fatal(err)
	}
	c := fileClient(b, b.origin)
	if _, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: dir + "/link"})); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: dir + "/broken"}))
	if reasonOf(err) != "not_found" {
		t.Fatal(err)
	}
	if strings.ContainsAny(err.Error(), "\n\x1b\u202e") {
		t.Errorf("the refusal carries them: %q", err.Error())
	}
	_, err = c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: dir + "/ends-badly"}))
	if reasonOf(err) != "not_found" {
		t.Fatal(err)
	}
	if strings.ContainsAny(err.Error(), "\n\x1b") {
		t.Errorf("the refusal carries them: %q", err.Error())
	}
	// What a request names is cleaned too, without the agent's help.
	_, err = c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: dir + "/x\n\x1b[31m"}))
	if reasonOf(err) != "invalid" {
		t.Fatal(err)
	}
	events := fileEvents(t, f)
	if len(events) != 4 {
		t.Fatalf("the audit log has %v", events)
	}
	for i := range events {
		for _, field := range []string{events[i].Subject, events[i].Detail} {
			if strings.ContainsAny(field, "\n\x1b\u202e") {
				t.Errorf("%s stored %q", events[i].Action, field)
			}
		}
	}
	for _, e := range timelineFiles(t, b, &dockwellv1.ListEventsRequest{ServerId: id}) {
		if strings.ContainsAny(e.GetSubject().GetPath(), "\n\x1b\u202e") {
			t.Errorf("the timeline shows %q", e.GetSubject().GetPath())
		}
	}
}

// tokenAndCookie sends a person's session with a token beside it.
type tokenAndCookie struct {
	browser *browser
	token   string
}

func (c tokenAndCookie) Do(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+c.token)
	return c.browser.Do(r)
}

func TestWhatTheAgentRefusesReachesThePerson(t *testing.T) {
	storetest.Each(t, testWhatTheAgentRefusesReachesThePerson)
}

func testWhatTheAgentRefusesReachesThePerson(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	server := f.addServer(t, false)
	id := server.id.ServerID
	key := filepath.Join(server.dataDir, "control", "key")
	writeFile(t, key, "the install's key")
	dir := t.TempDir()
	if err := os.Symlink(key, filepath.Join(dir, "planted")); err != nil {
		t.Fatal(err)
	}
	c := fileClient(b, b.origin)
	read := func(server, path string) error {
		_, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: server, Path: path}))
		return err
	}

	for _, p := range []string{key, filepath.Join(dir, "planted"), "/proc/self/environ"} {
		err := read(id, p)
		if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "file_refused" || !strings.Contains(err.Error(), "SSH") {
			t.Errorf("reading %s = %v, want file_refused", p, err)
		}
	}
	events := fileEvents(t, f)
	if len(events) != 3 {
		t.Fatalf("the audit log has %v", events)
	}
	for i := range events {
		if e := &events[i]; e.Action != "file.refuse" || e.Target != id {
			t.Errorf("a refusal went on record as %+v", e)
		}
	}
	// Where a link led is on record, as well as what it was called.
	if d := events[1].Detail; !strings.Contains(d, "planted") || !strings.Contains(d, server.dataDir) {
		t.Errorf("a planted link went on record as %s", d)
	}

	_, err := c.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: id, Path: "/proc"}))
	if reasonOf(err) != "file_refused" {
		t.Errorf("listing /proc = %v", err)
	}

	missing := filepath.Join(dir, "missing")
	for _, c := range []struct {
		name, server, path, reason string
		code                       connect.Code
	}{
		{"nothing there", id, missing, "not_found", connect.CodeNotFound},
		{"a directory", id, dir, "not_a_file", connect.CodeFailedPrecondition},
		{"no such server", "srv_nope", dir, "not_found", connect.CodeNotFound},
		{"a relative path", id, "etc/nginx", "invalid", connect.CodeInvalidArgument},
		{"a path with ..", id, dir + "/../etc", "invalid", connect.CodeInvalidArgument},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := read(c.server, c.path)
			if connect.CodeOf(err) != c.code || reasonOf(err) != c.reason {
				t.Errorf("got %v, want %s", err, c.reason)
			}
		})
	}

	_, err = c.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: id, Path: key + "/.."}))
	if reasonOf(err) != "invalid" {
		t.Errorf("listing a path with .. = %v", err)
	}
	file := filepath.Join(dir, "file")
	writeFile(t, file, "x")
	_, err = c.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: id, Path: file}))
	if reasonOf(err) != "not_a_directory" {
		t.Errorf("listing a file = %v", err)
	}

	server.stop()
	waitConnected(t, f, id, false)
	if err := read(id, file); connect.CodeOf(err) != connect.CodeUnavailable || reasonOf(err) != "server_offline" || !strings.Contains(err.Error(), "hz-1") {
		t.Errorf("reading from an offline server = %v", err)
	}
}

// What can't go on record doesn't go out. The test stops the audit log taking
// this one event with a trigger, which is SQLite's own spelling.
func TestAReadThatCantGoOnRecordReturnsNothing(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	config := filepath.Join(t.TempDir(), "config")
	writeFile(t, config, "s3cret")
	f.execSQL(t, `CREATE TRIGGER keep_out BEFORE INSERT ON audit_events WHEN NEW.action = 'file.read' BEGIN SELECT RAISE(ABORT, 'no room'); END`)
	res, err := fileClient(b, b.origin).ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: config}))
	if err == nil {
		t.Fatalf("the file went out with nothing on record: %v", res.Msg)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error holds the file: %v", err)
	}
}

// An agent from before files, or one without them, says so, through the same
// way any other failure of an agent comes back.
func TestAnAgentThatCantReadFilesSaysSo(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	server := f.addServer(t, false)
	server.stop()
	waitConnected(t, f, server.id.ServerID, false)
	server.disk = nil
	server.connect(t, f)
	c := fileClient(b, b.origin)
	_, err := c.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: server.id.ServerID, Path: "/"}))
	if reasonOf(err) != "agent_failed" || !strings.Contains(err.Error(), "hz-1") {
		t.Errorf("got %v", err)
	}
	// A read that fails this way, or as its time runs out, is on record like any other.
	_, err = c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: server.id.ServerID, Path: "/etc/hosts"}))
	if reasonOf(err) != "agent_failed" {
		t.Errorf("reading = %v", err)
	}
	if got := fileEvents(t, f); len(got) != 1 || got[0].Action != "file.refuse" || got[0].Subject != "/etc/hosts" {
		t.Errorf("the audit log has %v", got)
	}
}
