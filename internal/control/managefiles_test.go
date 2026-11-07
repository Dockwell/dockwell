// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A person makes, renames, moves and deletes on a server, puts back what they deleted,
// and deletes it for good, each on the timeline with where it was, never with what a
// file holds (ADR 0045).
func TestAPersonManagesAServersFiles(t *testing.T) {
	storetest.Each(t, testAPersonManagesAServersFiles)
}

func testAPersonManagesAServersFiles(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "site", "index.html"), "<h1>s3cret-in-a-file</h1>")
	c := fileClient(b, b.origin)
	site := filepath.Join(dir, "site")

	made, err := c.MakeFile(t.Context(), connect.NewRequest(&dockwellv1.MakeFileRequest{ServerId: id, Path: filepath.Join(site, "assets"), Type: dockwellv1.FileType_FILE_TYPE_DIRECTORY}))
	if err != nil || made.Msg.GetFile().GetType() != dockwellv1.FileType_FILE_TYPE_DIRECTORY {
		t.Fatalf("MakeFile = %v, %v", made, err)
	}
	if _, err := c.MakeFile(t.Context(), connect.NewRequest(&dockwellv1.MakeFileRequest{ServerId: id, Path: filepath.Join(site, "assets"), Type: dockwellv1.FileType_FILE_TYPE_FILE})); reasonOf(err) != "file_exists" {
		t.Errorf("making over what's there = %v", err)
	}
	renamed, err := c.MoveFile(t.Context(), connect.NewRequest(&dockwellv1.MoveFileRequest{ServerId: id, From: filepath.Join(site, "index.html"), To: filepath.Join(site, "home.html")}))
	if err != nil || renamed.Msg.GetTo() != filepath.Join(site, "home.html") {
		t.Fatalf("MoveFile = %v, %v", renamed, err)
	}
	if _, err := c.MoveFile(t.Context(), connect.NewRequest(&dockwellv1.MoveFileRequest{ServerId: id, From: filepath.Join(site, "home.html"), To: filepath.Join(site, "assets", "home.html")})); err != nil {
		t.Fatalf("moving it into a folder = %v", err)
	}
	moved := filepath.Join(site, "assets", "home.html")
	deleted, err := c.DeleteFile(t.Context(), connect.NewRequest(&dockwellv1.DeleteFileRequest{ServerId: id, Path: moved}))
	if err != nil || deleted.Msg.GetDeleted().GetId() == "" {
		t.Fatalf("DeleteFile = %v, %v", deleted, err)
	}
	listed, err := c.ListDeletedFiles(t.Context(), connect.NewRequest(&dockwellv1.ListDeletedFilesRequest{ServerId: id}))
	if err != nil || len(listed.Msg.GetDeleted()) != 1 || listed.Msg.GetDeleted()[0].GetPath() != moved {
		t.Fatalf("ListDeletedFiles = %v, %v", listed, err)
	}
	if _, err := c.RestoreFile(t.Context(), connect.NewRequest(&dockwellv1.RestoreFileRequest{ServerId: id, Id: deleted.Msg.GetDeleted().GetId()})); err != nil {
		t.Fatalf("RestoreFile = %v", err)
	}
	if got, _ := os.ReadFile(moved); string(got) != "<h1>s3cret-in-a-file</h1>" {
		t.Errorf("put back, it holds %q", got)
	}
	again, err := c.DeleteFile(t.Context(), connect.NewRequest(&dockwellv1.DeleteFileRequest{ServerId: id, Path: moved}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PurgeDeletedFile(t.Context(), connect.NewRequest(&dockwellv1.PurgeDeletedFileRequest{ServerId: id, Id: again.Msg.GetDeleted().GetId()})); err != nil {
		t.Fatalf("PurgeDeletedFile = %v", err)
	}
	if listed, _ := c.ListDeletedFiles(t.Context(), connect.NewRequest(&dockwellv1.ListDeletedFilesRequest{ServerId: id})); len(listed.Msg.GetDeleted()) != 0 {
		t.Errorf("still listed: %v", listed.Msg)
	}
	if _, err := c.RestoreFile(t.Context(), connect.NewRequest(&dockwellv1.RestoreFileRequest{ServerId: id, Id: "../../etc"})); reasonOf(err) != "invalid" {
		t.Errorf("an ID that's a path = %v", err)
	}
	if _, err := c.DeleteFile(t.Context(), connect.NewRequest(&dockwellv1.DeleteFileRequest{ServerId: id, Path: "/"})); reasonOf(err) != "invalid" {
		t.Errorf("deleting / = %v", err)
	}

	// Each is on the timeline, by where it was, and a move says where to.
	waitForFileEvent(t, b, id, "file.make", filepath.Join(site, "assets"))
	move := waitForFileEvent(t, b, id, "file.move", filepath.Join(site, "index.html"))
	if move.GetSubject().GetToPath() != filepath.Join(site, "home.html") || move.GetKind() != dockwellv1.EventKind_EVENT_KIND_SERVER {
		t.Errorf("a rename on the timeline: %v", move)
	}
	waitForFileEvent(t, b, id, "file.restore", moved)
	waitForFileEvent(t, b, id, "file.make_failed", filepath.Join(site, "assets"))
	forGood := false
	for _, e := range timelineFiles(t, b, &dockwellv1.ListEventsRequest{ServerId: id}) {
		forGood = forGood || e.GetAction() == "file.delete" && e.GetSubject().GetForGood()
	}
	if !forGood {
		t.Error("deleting for good isn't on the timeline as that")
	}
	events := fileEvents(t, f)
	for i := range events {
		if strings.Contains(events[i].Detail, "s3cret") {
			t.Errorf("what a file holds is on record: %v", events[i])
		}
	}
}

// No token makes, moves or deletes anything on a server, whatever its scope.
func TestNoTokenChangesAServersFiles(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	config := filepath.Join(t.TempDir(), "config")
	writeFile(t, config, "s3cret")
	for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
		token := createToken(t, b, scope)
		for _, r := range []struct{ verb, path, body string }{
			{http.MethodPost, "/v1/servers/" + id + "/files:make", `{"path":"` + config + `.new","type":"FILE_TYPE_FILE"}`},
			{http.MethodPost, "/v1/servers/" + id + "/files:move", `{"from":"` + config + `","to":"` + config + `.moved"}`},
			{http.MethodPost, "/v1/servers/" + id + "/files:delete", `{"path":"` + config + `"}`},
			{http.MethodGet, "/v1/servers/" + id + "/deleted-files", ""},
			{http.MethodPost, "/v1/servers/" + id + "/deleted-files/" + strings.Repeat("A", 26) + ":restore", `{}`},
			{http.MethodDelete, "/v1/servers/" + id + "/deleted-files/" + strings.Repeat("A", 26), ""},
		} {
			code, reason, _ := callREST(t, b, r.verb, r.path, map[string]string{"Authorization": "Bearer " + token}, r.body)
			if code != http.StatusForbidden || reason != authapi.ReasonNeedsSession {
				t.Errorf("%v: %s %s = %d %s", scope, r.verb, r.path, code, reason)
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

// Making, moving, deleting or putting back what lets someone back in takes a passkey,
// for those paths on that server, and one proof is for one write.
func TestChangingAWayBackInTakesAPasskey(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	c := fileClient(b, b.origin)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, ".ssh", "authorized_keys")
	writeFile(t, keys, "ssh-ed25519 AAAA admin\n")
	plain := filepath.Join(dir, "site", "keys.txt")
	writeFile(t, plain, "ssh-ed25519 AAAA stranger\n")
	del := func(proof string) (*connect.Response[dockwellv1.DeleteFileResponse], error) {
		return c.DeleteFile(t.Context(), connect.NewRequest(&dockwellv1.DeleteFileRequest{ServerId: id, Path: keys, Proof: proof}))
	}
	if _, err := del(""); reasonOf(err) != "needs_step_up" {
		t.Fatalf("with no proof = %v", err)
	}
	for what, proof := range map[string]string{
		"another path":   stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_DELETE_WAY_BACK_IN, id+":"+keys+".old"),
		"another action": stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SAVE_WAY_BACK_IN, id+":"+keys),
		"another server": stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_DELETE_WAY_BACK_IN, "srv_another:"+keys),
	} {
		if _, err := del(proof); reasonOf(err) != "needs_step_up" {
			t.Fatalf("with a proof for %s = %v", what, err)
		}
	}
	if _, err := os.Stat(keys); err != nil {
		t.Fatal("it went with no passkey for it")
	}
	gone, err := del(stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_DELETE_WAY_BACK_IN, id+":"+keys))
	if err != nil || !gone.Msg.GetWayBackIn() {
		t.Fatalf("with its proof = %v, %v", gone, err)
	}
	if e := waitForFileEvent(t, b, id, "file.delete", keys); !e.GetSubject().GetWayBackIn() {
		t.Errorf("on the timeline: %v", e)
	}
	// Putting it back asks too, for where it goes.
	restore := func(proof string) error {
		_, err := c.RestoreFile(t.Context(), connect.NewRequest(&dockwellv1.RestoreFileRequest{ServerId: id, Id: gone.Msg.GetDeleted().GetId(), Proof: proof}))
		return err
	}
	if err := restore(""); reasonOf(err) != "needs_step_up" {
		t.Fatalf("putting it back with no proof = %v", err)
	}
	if err := restore(stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_RESTORE_WAY_BACK_IN, id+":"+keys)); err != nil {
		t.Fatalf("putting it back with its proof = %v", err)
	}
	// A move is confirmed for both its ends, on two lines.
	to := filepath.Join(dir, ".ssh", "keys.txt")
	move := func(proof string) error {
		_, err := c.MoveFile(t.Context(), connect.NewRequest(&dockwellv1.MoveFileRequest{ServerId: id, From: plain, To: to, Proof: proof}))
		return err
	}
	if err := move(""); reasonOf(err) != "needs_step_up" {
		t.Fatalf("a move into .ssh with no proof = %v", err)
	}
	if err := move(stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_MOVE_WAY_BACK_IN, id+":"+plain)); reasonOf(err) != "needs_step_up" {
		t.Fatalf("with a proof for one end = %v", err)
	}
	if err := move(stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_MOVE_WAY_BACK_IN, id+":"+plain+"\n"+to)); err != nil {
		t.Fatalf("with a proof for both = %v", err)
	}
	// Asked, with none following, is on Activity.
	if _, err := c.MakeFile(t.Context(), connect.NewRequest(&dockwellv1.MakeFileRequest{ServerId: id, Path: filepath.Join(dir, ".ssh", "authorized_keys2"), Type: dockwellv1.FileType_FILE_TYPE_FILE})); reasonOf(err) != "needs_step_up" {
		t.Fatalf("making authorized_keys2 = %v", err)
	}
	waitForFileEvent(t, b, id, "file.make_unconfirmed", filepath.Join(dir, ".ssh", "authorized_keys2"))
}

// An agent too old to make, move and delete with the care these take says so, and
// lists nothing deleted.
func TestAnAgentThatDoesntManageFilesSaysSo(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	server := f.addServer(t, false)
	id := server.id.ServerID
	server.stop()
	waitConnected(t, f, id, false)
	server.hello = func(h *agentv1.Hello) { h.ManagesFiles = false }
	server.connect(t, f)
	c := fileClient(b, b.origin)
	dir := t.TempDir()
	_, err := c.MakeFile(t.Context(), connect.NewRequest(&dockwellv1.MakeFileRequest{ServerId: id, Path: filepath.Join(dir, "x"), Type: dockwellv1.FileType_FILE_TYPE_FILE}))
	if reasonOf(err) != "agent_outdated" || !strings.Contains(err.Error(), "Update the agent on") {
		t.Fatalf("making through it = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Error("it made it anyway")
	}
	listed, err := c.ListDeletedFiles(t.Context(), connect.NewRequest(&dockwellv1.ListDeletedFilesRequest{ServerId: id}))
	if err != nil || len(listed.Msg.GetDeleted()) != 0 {
		t.Errorf("ListDeletedFiles = %v, %v", listed, err)
	}
}
