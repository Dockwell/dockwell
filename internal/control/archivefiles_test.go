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
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func waitArchiveJob(t *testing.T, c dockwellv1connect.FileServiceClient, serverID, jobID string) *dockwellv1.ArchiveJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := c.GetArchiveJob(t.Context(), connect.NewRequest(&dockwellv1.GetArchiveJobRequest{
			ServerId: serverID,
			JobId:    jobID,
		}))
		if err != nil {
			t.Fatalf("GetArchiveJob(%s): %v", jobID, err)
		}
		job := res.Msg.GetJob()
		if job.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("archive job %s did not finish in time: %v", jobID, job)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

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
	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x01}
	if err := os.WriteFile(filepath.Join(site, "logo.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	c := fileClient(b, b.origin)

	// Compress selection into site/release.zip (async job, poll via GetArchiveJob & ListArchiveJobs).
	archive := filepath.Join(site, "release.zip")
	comp, err := c.CompressFiles(t.Context(), connect.NewRequest(&dockwellv1.CompressFilesRequest{
		ServerId:    id,
		Paths:       []string{filepath.Join(site, "index.html"), filepath.Join(site, "css")},
		ArchivePath: archive,
	}))
	if err != nil || comp.Msg.GetJob().GetJobId() == "" {
		t.Fatalf("CompressFiles = %v, %v", comp, err)
	}
	compJob := waitArchiveJob(t, c, id, comp.Msg.GetJob().GetJobId())
	if compJob.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE || compJob.GetPath() != archive || compJob.GetEntries() != 3 {
		t.Fatalf("compress job = %v", compJob)
	}
	// L5 + N2: compress Activity writes both started and done rows with source paths.
	waitForFileEvent(t, b, id, "file.compress_started", archive)
	ce := waitForFileEvent(t, b, id, "file.compress", archive)
	if ce.GetSubject().GetPathCount() != 2 || len(ce.GetSubject().GetPaths()) != 2 {
		t.Errorf("file.compress paths = %v (count %d), want 2", ce.GetSubject().GetPaths(), ce.GetSubject().GetPathCount())
	}

	// N2: a tiny compress job started without ever polling GetArchiveJob or
	// ListArchiveJobs (CLI/API/closed tab) still lands both started and done rows.
	unpolled := filepath.Join(site, "unpolled.zip")
	if up, err := c.CompressFiles(t.Context(), connect.NewRequest(&dockwellv1.CompressFilesRequest{
		ServerId:    id,
		Paths:       []string{filepath.Join(site, "index.html")},
		ArchivePath: unpolled,
	})); err != nil || up.Msg.GetJob().GetJobId() == "" {
		t.Fatalf("unpolled CompressFiles = %v, %v", up, err)
	}
	waitForFileEvent(t, b, id, "file.compress_started", unpolled)
	waitForFileEvent(t, b, id, "file.compress", unpolled)

	// ListArchiveJobs includes the finished job.
	listed, err := c.ListArchiveJobs(t.Context(), connect.NewRequest(&dockwellv1.ListArchiveJobsRequest{ServerId: id}))
	if err != nil || len(listed.Msg.GetJobs()) == 0 {
		t.Fatalf("ListArchiveJobs = %v, %v", listed, err)
	}

	// Extract site/release.zip into site/release.
	ext, err := c.ExtractArchive(t.Context(), connect.NewRequest(&dockwellv1.ExtractArchiveRequest{
		ServerId: id,
		Path:     archive,
	}))
	if err != nil || ext.Msg.GetJob().GetJobId() == "" {
		t.Fatalf("ExtractArchive = %v, %v", ext, err)
	}
	extJob := waitArchiveJob(t, c, id, ext.Msg.GetJob().GetJobId())
	if extJob.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE || extJob.GetUnpackedTo() != filepath.Join(site, "release") || extJob.GetEntries() != 3 {
		t.Fatalf("extract job = %v", extJob)
	}
	waitForFileEvent(t, b, id, "file.extract_started", archive)
	e := waitForFileEvent(t, b, id, "file.extract", archive)
	if e.GetSubject().GetToPath() != filepath.Join(site, "release") {
		t.Errorf("file.extract to_path = %q", e.GetSubject().GetToPath())
	}
	if got, _ := os.ReadFile(filepath.Join(site, "release", "css", "app.css")); string(got) != "body{}" {
		t.Errorf("extracted css = %q", got)
	}

	// M5: image preview reads via ReadFile(allow_binary=true), recording file.read and NOT file.download.
	logoPath := filepath.Join(site, "logo.png")
	readImg, err := c.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{
		ServerId:    id,
		Path:        logoPath,
		AllowBinary: true,
	}))
	if err != nil || !readImg.Msg.GetBinary() || !bytes.Equal(readImg.Msg.GetRawContent(), pngBytes) {
		t.Fatalf("ReadFile allow_binary = %v, %v", readImg, err)
	}
	waitForFileEvent(t, b, id, "file.read", logoPath)

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
	// L2: one file.download event for the selection carrying resolved paths, and ZERO download events for logo.png.
	dlEvent := waitForFileEvent(t, b, id, "file.download", filepath.Join(site, "index.html"))
	if dlEvent.GetSubject().GetPathCount() != 2 || len(dlEvent.GetSubject().GetPaths()) != 2 {
		t.Errorf("multi download event paths = %v (count %d), want 2", dlEvent.GetSubject().GetPaths(), dlEvent.GetSubject().GetPathCount())
	}
	eventsRes, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{ServerId: id, PageSize: 50}))
	if err != nil {
		t.Fatal(err)
	}
	dlCount := 0
	for _, ev := range eventsRes.Msg.GetEvents() {
		if ev.GetAction() == "file.download" {
			dlCount++
			if ev.GetSubject().GetPath() == logoPath {
				t.Errorf("image preview recorded a file.download event for %s", logoPath)
			}
		}
	}
	if dlCount != 1 {
		t.Errorf("file.download event count = %d, want 1", dlCount)
	}

	// Tokens are refused for compress, extract, archive job queries, and multi-path download.
	for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
		bearer := map[string]string{"Authorization": "Bearer " + createToken(t, b, scope)}
		for _, r := range []struct {
			verb, path string
			body       []byte
		}{
			{http.MethodPost, "/v1/servers/" + id + "/files:compress", []byte(`{"paths":["` + filepath.Join(site, "index.html") + `"],"archivePath":"` + filepath.Join(site, "t.zip") + `"}`)},
			{http.MethodPost, "/v1/servers/" + id + "/files:extract", []byte(`{"path":"` + archive + `"}`)},
			{http.MethodGet, "/v1/servers/" + id + "/archive-jobs", nil},
			{http.MethodGet, "/v1/servers/" + id + "/archive-jobs/" + compJob.GetJobId(), nil},
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

func TestArchiveWayBackInTakesDedicatedStepUp(t *testing.T) {
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
	// MAKE_WAY_BACK_IN proof must NOT work for CompressFiles (M4).
	wrongProof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_MAKE_WAY_BACK_IN, id+":"+archive)
	if _, err := c.CompressFiles(t.Context(), connect.NewRequest(&dockwellv1.CompressFilesRequest{
		ServerId: id, Paths: []string{plain}, ArchivePath: archive, Proof: wrongProof,
	})); reasonOf(err) != "needs_step_up" {
		t.Fatalf("compress with MAKE_WAY_BACK_IN proof = %v, want needs_step_up", err)
	}
	proof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_COMPRESS_WAY_BACK_IN, id+":"+archive)
	comp, err := c.CompressFiles(t.Context(), connect.NewRequest(&dockwellv1.CompressFilesRequest{
		ServerId: id, Paths: []string{plain}, ArchivePath: archive, Proof: proof,
	}))
	if err != nil || !comp.Msg.GetJob().GetWayBackIn() {
		t.Fatalf("compress into .ssh with proof = %v, %v", comp, err)
	}
	if j := waitArchiveJob(t, c, id, comp.Msg.GetJob().GetJobId()); j.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("compress job = %v", j)
	}

	if _, err := c.ExtractArchive(t.Context(), connect.NewRequest(&dockwellv1.ExtractArchiveRequest{
		ServerId: id, Path: archive,
	})); reasonOf(err) != "needs_step_up" {
		t.Fatalf("extract into .ssh with no proof = %v", err)
	}
	dst := filepath.Join(sshDir, "bundle")
	wrongExtProof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_MAKE_WAY_BACK_IN, id+":"+archive+"\n"+dst)
	if _, err := c.ExtractArchive(t.Context(), connect.NewRequest(&dockwellv1.ExtractArchiveRequest{
		ServerId: id, Path: archive, Proof: wrongExtProof,
	})); reasonOf(err) != "needs_step_up" {
		t.Fatalf("extract with MAKE_WAY_BACK_IN proof = %v, want needs_step_up", err)
	}
	// N3: even with the dedicated EXTRACT_WAY_BACK_IN passkey, entries whose full
	// resolved path sits inside .ssh are still refused during unpack.
	eProof := stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_EXTRACT_WAY_BACK_IN, id+":"+archive+"\n"+dst)
	ext, err := c.ExtractArchive(t.Context(), connect.NewRequest(&dockwellv1.ExtractArchiveRequest{
		ServerId: id, Path: archive, Proof: eProof,
	}))
	if err != nil || !ext.Msg.GetJob().GetWayBackIn() || ext.Msg.GetJob().GetUnpackedTo() != dst {
		t.Fatalf("extract into .ssh with proof = %v, %v", ext, err)
	}
	if j := waitArchiveJob(t, c, id, ext.Msg.GetJob().GetJobId()); j.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED || j.GetErrorReason() != "unpack_refused" {
		t.Fatalf("extract into .ssh job = %v, want FAILED/unpack_refused", j)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("refused extract into .ssh left %s behind", dst)
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
