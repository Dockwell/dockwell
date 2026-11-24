// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/url"
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

func TestCompressExtractAndDownloadSelection(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testCompressExtractAndDownloadSelection)
}

func testCompressExtractAndDownloadSelection(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(dir, "site")
	writeFile(t, filepath.Join(site, "index.html"), "<h1>hi</h1>")
	writeFile(t, filepath.Join(site, "css", "app.css"), "body{}")
	c := fileClient(b, b.origin)

	// Compress selection into site/release.zip.
	archive := filepath.Join(site, "release.zip")
	comp, err := c.CompressFiles(t.Context(), connect.NewRequest(&dockwellv1.CompressFilesRequest{
		ServerId:    id,
		Paths:       []string{filepath.Join(site, "index.html"), filepath.Join(site, "css")},
		ArchivePath: archive,
	}))
	if err != nil || comp.Msg.GetPath() != archive || comp.Msg.GetEntries() != 3 {
		t.Fatalf("CompressFiles = %v, %v", comp, err)
	}
	waitForFileEvent(t, b, id, "file.compress", archive)

	// Extract site/release.zip into site/release.
	ext, err := c.ExtractArchive(t.Context(), connect.NewRequest(&dockwellv1.ExtractArchiveRequest{
		ServerId: id,
		Path:     archive,
	}))
	if err != nil || ext.Msg.GetUnpackedTo() != filepath.Join(site, "release") || ext.Msg.GetUnpackedEntries() != 3 {
		t.Fatalf("ExtractArchive = %v, %v", ext, err)
	}
	e := waitForFileEvent(t, b, id, "file.extract", archive)
	if e.GetSubject().GetToPath() != filepath.Join(site, "release") {
		t.Errorf("file.extract to_path = %q", e.GetSubject().GetToPath())
	}
	if got, _ := os.ReadFile(filepath.Join(site, "release", "css", "app.css")); string(got) != "body{}" {
		t.Errorf("extracted css = %q", got)
	}

	// Download multiple paths as one zip over REST.
	q := url.Values{}
	q.Add("paths", filepath.Join(site, "index.html"))
	q.Add("paths", filepath.Join(site, "css"))
	res, raw := rawREST(t, b, http.MethodGet, "/v1/servers/"+id+"/files:download?"+q.Encode(), nil, nil)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("multi download status = %d, ct = %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("invalid zip: %v", err)
	}
	found := map[string]bool{}
	for _, zf := range zr.File {
		found[zf.Name] = true
	}
	if !found["index.html"] || !found["css/app.css"] {
		t.Errorf("zip entries = %v", found)
	}
	waitForFileEvent(t, b, id, "file.download", filepath.Join(site, "index.html"))
	waitForFileEvent(t, b, id, "file.download", filepath.Join(site, "css"))

	// Tokens are refused for compress, extract and multi-path download.
	for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
		bearer := map[string]string{"Authorization": "Bearer " + createToken(t, b, scope)}
		for _, r := range []struct {
			verb, path string
			body       []byte
		}{
			{http.MethodPost, "/v1/servers/" + id + "/files:compress", []byte(`{"paths":["` + filepath.Join(site, "index.html") + `"],"archivePath":"` + filepath.Join(site, "t.zip") + `"}`)},
			{http.MethodPost, "/v1/servers/" + id + "/files:extract", []byte(`{"path":"` + archive + `"}`)},
			{http.MethodGet, "/v1/servers/" + id + "/files:download?" + q.Encode(), nil},
		} {
			extra := bearer
			if r.verb == http.MethodPost {
				extra = map[string]string{"Authorization": bearer["Authorization"], "Content-Type": "application/json"}
			}
			tRes, _ := rawREST(t, b, r.verb, r.path, r.body, extra)
			if tRes.StatusCode != http.StatusForbidden || tRes.Header.Get(authapi.ReasonKey) != authapi.ReasonNeedsSession {
				t.Errorf("%v: %s %s = %d %s", scope, r.verb, r.path, tRes.StatusCode, tRes.Header.Get(authapi.ReasonKey))
			}
		}
	}
}

func TestArchiveWayBackInTakesAPasskey(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "site", "note.txt")
	writeFile(t, plain, "hello")
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	c := fileClient(b, b.origin)
	archive := filepath.Join(sshDir, "bundle.zip")
	if _, err := c.CompressFiles(t.Context(), connect.NewRequest(&dockwellv1.CompressFilesRequest{
		ServerId: id, Paths: []string{plain}, ArchivePath: archive,
	})); reasonOf(err) != "needs_step_up" {
		t.Fatalf("compress into .ssh with no proof = %v", err)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_MAKE_WAY_BACK_IN, id+":"+archive)
	comp, err := c.CompressFiles(t.Context(), connect.NewRequest(&dockwellv1.CompressFilesRequest{
		ServerId: id, Paths: []string{plain}, ArchivePath: archive, Proof: proof,
	}))
	if err != nil || !comp.Msg.GetWayBackIn() {
		t.Fatalf("compress into .ssh with proof = %v, %v", comp, err)
	}
	if _, err := c.ExtractArchive(t.Context(), connect.NewRequest(&dockwellv1.ExtractArchiveRequest{
		ServerId: id, Path: archive,
	})); reasonOf(err) != "needs_step_up" {
		t.Fatalf("extract into .ssh with no proof = %v", err)
	}
	dst := filepath.Join(sshDir, "bundle")
	eProof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_MAKE_WAY_BACK_IN, id+":"+dst)
	ext, err := c.ExtractArchive(t.Context(), connect.NewRequest(&dockwellv1.ExtractArchiveRequest{
		ServerId: id, Path: archive, Proof: eProof,
	}))
	if err != nil || !ext.Msg.GetWayBackIn() || ext.Msg.GetUnpackedTo() != dst {
		t.Fatalf("extract into .ssh with proof = %v, %v", ext, err)
	}
}

func TestOldAgentRefusesArchiveCalls(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	server := f.addServer(t, false)
	id := server.id.ServerID
	server.stop()
	waitConnected(t, f, id, false)
	server.hello = func(h *agentv1.Hello) { h.ArchivesFiles = false }
	server.connect(t, f)
	c := fileClient(b, b.origin)
	if _, err := c.CompressFiles(t.Context(), connect.NewRequest(&dockwellv1.CompressFilesRequest{
		ServerId: id, Paths: []string{"/tmp/a"}, ArchivePath: "/tmp/a.zip",
	})); reasonOf(err) != "agent_outdated" || !strings.Contains(err.Error(), "Update the agent on") {
		t.Errorf("old agent CompressFiles = %v, want agent_outdated", err)
	}
	if _, err := c.ExtractArchive(t.Context(), connect.NewRequest(&dockwellv1.ExtractArchiveRequest{
		ServerId: id, Path: "/tmp/a.zip",
	})); reasonOf(err) != "agent_outdated" || !strings.Contains(err.Error(), "Update the agent on") {
		t.Errorf("old agent ExtractArchive = %v, want agent_outdated", err)
	}
}
