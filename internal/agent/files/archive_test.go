// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

const testArchiveOwner = "usr_test"

func makeTestZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipNames(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	out := map[string]string{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		out[f.Name] = buf.String()
	}
	return out
}

func waitJob(t *testing.T, s *Server, id string) *dockwellv1.ArchiveJob {
	t.Helper()
	job, err := s.WaitArchiveJob(t.Context(), id)
	if err != nil {
		t.Fatalf("WaitArchiveJob(%s): %v", id, err)
	}
	return job
}

func TestCompressAndExtractArchives(t *testing.T) {
	if maxArchived != maxUnpacked {
		t.Fatalf("maxArchived (%d) != maxUnpacked (%d)", maxArchived, maxUnpacked)
	}
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "index.html"), "<h1>hello</h1>\n")
	write(t, filepath.Join(site, "assets", "app.js"), "console.log(1)\n")

	archive := filepath.Join(site, "bundle.zip")
	comp, err := d.server.Compress(t.Context(), []string{
		filepath.Join(site, "index.html"),
		filepath.Join(site, "assets"),
	}, archive, "", testArchiveOwner)
	if err != nil || comp.GetRefusal() != nil || comp.GetJob() == nil {
		t.Fatalf("Compress = %v, %v", comp, err)
	}
	doneComp := waitJob(t, d.server, comp.GetJob().GetJobId())
	if doneComp.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE || doneComp.GetPath() != archive || doneComp.GetEntries() != 3 {
		t.Fatalf("compressed job = %v", doneComp)
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	got := zipNames(t, raw)
	if got["index.html"] != "<h1>hello</h1>\n" || got["assets/app.js"] != "console.log(1)\n" {
		t.Fatalf("archive contents = %v", got)
	}

	// Compressing over an existing file is refused synchronously in preflight.
	if dup, _ := d.server.Compress(t.Context(), []string{filepath.Join(site, "index.html")}, archive, "", testArchiveOwner); dup.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_EXISTS {
		t.Errorf("compress over existing = %v, want KIND_EXISTS", dup)
	}

	// Extract the created zip into site/bundle.
	ext, err := d.server.Extract(t.Context(), archive, "", "", testArchiveOwner)
	if err != nil || ext.GetRefusal() != nil || ext.GetJob() == nil {
		t.Fatalf("Extract = %v, %v", ext, err)
	}
	doneExt := waitJob(t, d.server, ext.GetJob().GetJobId())
	if doneExt.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE || doneExt.GetUnpackedTo() != filepath.Join(site, "bundle") || doneExt.GetEntries() != 3 {
		t.Fatalf("extracted job = %v", doneExt)
	}
	if b, _ := os.ReadFile(filepath.Join(site, "bundle", "index.html")); string(b) != "<h1>hello</h1>\n" {
		t.Errorf("extracted index.html = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(site, "bundle", "assets", "app.js")); string(b) != "console.log(1)\n" {
		t.Errorf("extracted assets/app.js = %q", b)
	}

	// Extracting when the destination folder already exists is refused with KIND_EXISTS.
	if again, _ := d.server.Extract(t.Context(), archive, "", "", testArchiveOwner); again.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_EXISTS {
		t.Errorf("extract over existing = %v, want KIND_EXISTS", again)
	}
}

func TestCompressRefusesPrivateKeyAndLeavesNothingBehind(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "ok.txt"), "hello\n")
	write(t, filepath.Join(site, "secret.pem"), "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0BAQEFAASCBK\n-----END PRIVATE KEY-----\n")

	// Compressing a single file that holds a private key is refused cleanly
	// before creating any archive or staging file on disk.
	singleArchive := filepath.Join(site, "only-key.zip")
	comp, err := d.server.Compress(t.Context(), []string{
		filepath.Join(site, "secret.pem"),
	}, singleArchive, "", testArchiveOwner)
	if err != nil || comp.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("Compress single private key = %v, %v, want KIND_REFUSED", comp, err)
	}
	if there(singleArchive) {
		t.Error("refused archive was created on disk")
	}
	entries, _ := os.ReadDir(site)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), uploadPrefix) {
			t.Errorf("staging file %s was left behind", e.Name())
		}
	}

	// Compressing a multi-file selection leaves out the private key, lists it,
	// and packs the remaining files cleanly (L3).
	archive := filepath.Join(site, "mixed.zip")
	comp, err = d.server.Compress(t.Context(), []string{
		filepath.Join(site, "ok.txt"),
		filepath.Join(site, "secret.pem"),
	}, archive, "", testArchiveOwner)
	if err != nil || comp.GetRefusal() != nil {
		t.Fatalf("Compress mixed selection = %v, %v", comp, err)
	}
	done := waitJob(t, d.server, comp.GetJob().GetJobId())
	if done.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("mixed compress job = %v", done)
	}
	if done.GetLeftOut() != 1 || len(done.GetLeftOutPaths()) != 1 || done.GetLeftOutPaths()[0] != "secret.pem" {
		t.Errorf("left_out = %d (%v), want 1 ([secret.pem])", done.GetLeftOut(), done.GetLeftOutPaths())
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(zr.Comment, "secret.pem") {
		t.Errorf("zip comment = %q, want secret.pem listed", zr.Comment)
	}
	got := zipNames(t, raw)
	if got["ok.txt"] != "hello\n" {
		t.Errorf("ok.txt = %q, want hello", got["ok.txt"])
	}
	if _, leaked := got["secret.pem"]; leaked {
		t.Error("secret.pem leaked into compressed archive")
	}
}

// M1: selected folder path must not double the folder name when checking credentials.
func TestArchiveSelectionFolderChecksExactCredentialPath(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	etc := filepath.Join(site, "etc")
	write(t, filepath.Join(etc, "ok.txt"), "safe")
	write(t, filepath.Join(etc, ".pgpass"), "localhost:5432:*:postgres:pw")
	exactSecret := filepath.Join(etc, "shadow-exact")
	write(t, exactSecret, "root:*:19000:0:99999:7:::\n")

	orig := checkCredentials
	checkCredentials = func(p string) string {
		if p == exactSecret {
			return whyHashes
		}
		return orig(p)
	}
	t.Cleanup(func() { checkCredentials = orig })

	// 1) Compress selection containing folder `etc`
	archive := filepath.Join(site, "etc.zip")
	comp, err := d.server.Compress(t.Context(), []string{etc}, archive, "", testArchiveOwner)
	if err != nil || comp.GetRefusal() != nil {
		t.Fatalf("Compress = %v, %v", comp, err)
	}
	done := waitJob(t, d.server, comp.GetJob().GetJobId())
	if done.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("job = %v", done)
	}
	if done.GetLeftOut() != 2 {
		t.Errorf("compress left_out = %d, want 2", done.GetLeftOut())
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	got := zipNames(t, raw)
	if got["etc/ok.txt"] != "safe" {
		t.Fatalf("expected etc/ok.txt, got %v", got)
	}
	if _, leaked := got["etc/shadow-exact"]; leaked {
		t.Fatal("exact-path credential leaked into compressed archive (doubled folder name bug)")
	}
	if _, leaked := got["etc/.pgpass"]; leaked {
		t.Fatal(".pgpass leaked into compressed archive")
	}

	// 2) DownloadMany selection containing folder `etc`
	var body bytes.Buffer
	res, err := d.server.DownloadMany(t.Context(), []string{etc}, func(m *agentv1.SendFileRequest) error {
		body.Write(m.GetData())
		return nil
	})
	if err != nil || res.GetRefusal() != nil {
		t.Fatalf("DownloadMany = %v, %v", res, err)
	}
	if res.GetLeftOut() != 2 {
		t.Errorf("download left_out = %d, want 2", res.GetLeftOut())
	}
	dlGot := zipNames(t, body.Bytes())
	if _, leaked := dlGot["etc/shadow-exact"]; leaked {
		t.Fatal("exact-path credential leaked into multi-path download")
	}
	if _, leaked := dlGot["etc/.pgpass"]; leaked {
		t.Fatal(".pgpass leaked into multi-path download")
	}
}

// M2: archive jobs use their own pool and run preflight under attempt() so a
// hung mount never blocks listings/reads.
func TestArchiveJobsUseOwnPoolAndAttempt(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "a.txt"), "hi")

	// Fill both archive slots directly and verify a 3rd Compress returns KIND_BUSY
	// while List and Read keep working immediately.
	rel1, r1 := d.server.tryTakeArchive()
	rel2, r2 := d.server.tryTakeArchive()
	if r1 != nil || r2 != nil {
		t.Fatalf("first two archive slots failed: %v, %v", r1, r2)
	}
	defer rel1()
	defer rel2()

	busyComp, err := d.server.Compress(t.Context(), []string{filepath.Join(site, "a.txt")}, filepath.Join(site, "out.zip"), "", testArchiveOwner)
	if err != nil || busyComp.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_BUSY {
		t.Fatalf("3rd archive job = %v, %v, want KIND_BUSY", busyComp, err)
	}
	listRes, err := d.server.List(t.Context(), site, "", "")
	if err != nil || listRes.GetRefusal() != nil {
		t.Fatalf("List while archive pool full = %v, %v", listRes, err)
	}
	readRes, err := d.server.Read(t.Context(), filepath.Join(site, "a.txt"))
	if err != nil || readRes.GetFile().GetContent() != "hi" {
		t.Fatalf("Read while archive pool full = %v, %v", readRes, err)
	}

	// Verify attempt() bounds a hung path on Compress.
	rel1()
	rel2()
	unblock := make(chan struct{})
	// The hung Compress goes on in the background once let go: wait for it to
	// return before the test's folder is removed (main's CI on e7a2bea6e:
	// "directory not empty").
	t.Cleanup(func() {
		close(unblock)
		deadline := time.Now().Add(10 * time.Second)
		for d.server.stuckCount() > 0 {
			if time.Now().After(deadline) {
				t.Error("the hung Compress never returned")
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	d.server.AnswerWithin = 80 * time.Millisecond
	hung := filepath.Join(site, "hung.zip")
	d.server.Touching = func(p string) {
		if p == hung {
			<-unblock
		}
	}
	compHung, err := d.server.Compress(t.Context(), []string{filepath.Join(site, "a.txt")}, hung, "", testArchiveOwner)
	if err != nil || compHung.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NOT_ANSWERING {
		t.Fatalf("hung Compress = %v, %v, want KIND_NOT_ANSWERING", compHung, err)
	}
}

// M3: compress writes 0600 (or tighter with source folder); extract keeps entry
// modes masked to drop setuid/setgid/sticky, group/other write, and anything
// above the archive's own visibility.
func TestCompressAndExtractPermissions(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, ".env"), "SECRET=1\n")
	if err := os.Chmod(filepath.Join(site, ".env"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Compress writes archive with 0600 permissions.
	archive := filepath.Join(site, "app.zip")
	comp, err := d.server.Compress(t.Context(), []string{filepath.Join(site, ".env")}, archive, "", testArchiveOwner)
	if err != nil || comp.GetRefusal() != nil {
		t.Fatalf("Compress = %v, %v", comp, err)
	}
	done := waitJob(t, d.server, comp.GetJob().GetJobId())
	if done.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("compress job = %v", done)
	}
	st, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("compressed archive mode = %o, want 0600", st.Mode().Perm())
	}

	// Build a .tar.gz with 0600, 0755, 04755 (setuid), 02755 (setgid), 01777 (sticky+ww), and 0700 dir.
	var tgz bytes.Buffer
	gw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gw)
	addTar := func(name string, mode int64, body string, dir bool) {
		hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if !dir {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	addTar("priv/", 0o700, "", true)
	addTar("priv/.env", 0o600, "A=1", false)
	addTar("run.sh", 0o755, "#!/bin/sh", false)
	addTar("suid", 0o4755, "bin", false)
	addTar("sgid", 0o2755, "bin", false)
	addTar("sticky", 0o1777, "world", false)
	addTar("plain.txt", 0o644, "hello", false)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	// 1) Archive mode 0644: keeps 0600 as 0600, drops setuid/setgid/sticky/g+w/o+w.
	worldArc := filepath.Join(site, "world.tar.gz")
	if err := os.WriteFile(worldArc, tgz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	ext1, err := d.server.Extract(t.Context(), worldArc, "", "", testArchiveOwner)
	if err != nil || ext1.GetRefusal() != nil {
		t.Fatalf("Extract world = %v, %v", ext1, err)
	}
	if j := waitJob(t, d.server, ext1.GetJob().GetJobId()); j.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("world extract job = %v", j)
	}
	checkMode := func(rel string, want fs.FileMode) {
		t.Helper()
		info, err := os.Stat(filepath.Join(site, "world", rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
			t.Errorf("%s kept special bits: %v", rel, info.Mode())
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s perm = %o, want %o", rel, info.Mode().Perm(), want)
		}
	}
	checkMode("priv", 0o700)
	checkMode("priv/.env", 0o600)
	checkMode("run.sh", 0o755)
	checkMode("suid", 0o755)
	checkMode("sgid", 0o755)
	checkMode("sticky", 0o755)
	checkMode("plain.txt", 0o644)

	// 2) Archive mode 0600: masks extracted files so none exceeds the archive's own visibility.
	privArc := filepath.Join(site, "private.tar.gz")
	if err := os.WriteFile(privArc, tgz.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ext2, err := d.server.Extract(t.Context(), privArc, "", "", testArchiveOwner)
	if err != nil || ext2.GetRefusal() != nil {
		t.Fatalf("Extract private = %v, %v", ext2, err)
	}
	if j := waitJob(t, d.server, ext2.GetJob().GetJobId()); j.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("private extract job = %v", j)
	}
	for rel, want := range map[string]fs.FileMode{
		"":          0o700,
		"plain.txt": 0o600,
		"run.sh":    0o700,
		"priv/.env": 0o600,
	} {
		info, err := os.Stat(filepath.Join(site, "private", rel))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("private/%s perm = %o, want %o", rel, info.Mode().Perm(), want)
		}
	}
}

// M4: step-up requires archive+destination, and a passkey for extract never
// turns off the per-entry key/login check inside the archive.
func TestCompressAndExtractWayBackInNeedStepUp(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	sys := filepath.Join(d.root, "etc")
	d.server.systemPlaces = func() []string { return []string{sys} }
	write(t, filepath.Join(sys, "app.conf"), "ok = 1\n")

	archive := filepath.Join(sys, "backup.zip")
	if r, _ := d.server.Compress(t.Context(), []string{filepath.Join(sys, "app.conf")}, archive, "", testArchiveOwner); r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Fatalf("Compress in system path without step-up = %v", r)
	}
	comp, err := d.server.Compress(t.Context(), []string{filepath.Join(sys, "app.conf")}, archive, archive, testArchiveOwner)
	if err != nil || comp.GetRefusal() != nil || !comp.GetJob().GetWayBackIn() {
		t.Fatalf("Compress in system path with step-up = %v, %v", comp, err)
	}
	if j := waitJob(t, d.server, comp.GetJob().GetJobId()); j.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("compress job = %v", j)
	}

	dst := filepath.Join(sys, "backup")
	r, _ := d.server.Extract(t.Context(), archive, "", "", testArchiveOwner)
	if r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP || r.GetRefusal().GetPath() != dst {
		t.Fatalf("Extract in system path without step-up = %v", r)
	}
	if !strings.Contains(r.GetRefusal().GetMessage(), archive) || !strings.Contains(r.GetRefusal().GetMessage(), dst) {
		t.Errorf("extract step-up refusal should name archive and destination: %q", r.GetRefusal().GetMessage())
	}
	// Confirmed destination alone (without matching archive) is refused.
	if r2, _ := d.server.Extract(t.Context(), archive, "", dst, testArchiveOwner); r2.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Fatalf("Extract with only dst confirmed = %v, want KIND_NEEDS_STEP_UP", r2)
	}
	ext, err := d.server.Extract(t.Context(), archive, archive, dst, testArchiveOwner)
	if err != nil || ext.GetRefusal() != nil || !ext.GetJob().GetWayBackIn() {
		t.Fatalf("Extract in system path with step-up = %v, %v", ext, err)
	}
	if j := waitJob(t, d.server, ext.GetJob().GetJobId()); j.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("extract job = %v", j)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "app.conf")); string(b) != "ok = 1\n" {
		t.Errorf("extracted app.conf = %q", b)
	}

	// Extracting an archive holding authorized_keys into a way-in destination
	// MUST STILL BE REFUSED even with a passkey confirming the destination!
	sneaky := filepath.Join(sys, "keys.zip")
	if err := os.WriteFile(sneaky, makeTestZip(t, map[string]string{
		"authorized_keys": "ssh-ed25519 AAAA... attacker\n",
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	sneakyDst := filepath.Join(sys, "keys")
	extBad, err := d.server.Extract(t.Context(), sneaky, sneaky, sneakyDst, testArchiveOwner)
	if err != nil || extBad.GetRefusal() != nil {
		t.Fatalf("start sneaky extract = %v, %v", extBad, err)
	}
	badDone := waitJob(t, d.server, extBad.GetJob().GetJobId())
	if badDone.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED {
		t.Fatalf("extract of authorized_keys with passkey succeeded: %v", badDone)
	}
	if there(sneakyDst) {
		t.Error("refused extract of authorized_keys left destination folder behind")
	}
}

func TestExtractRefusesZipSlipAndHardLink(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")

	// Zip slip archive
	bad := filepath.Join(site, "slip.zip")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, makeTestZip(t, map[string]string{"../escape.txt": "nope"}), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := d.server.Extract(t.Context(), bad, "", "", testArchiveOwner)
	if err != nil || r.GetRefusal() != nil {
		t.Fatalf("start Extract zip-slip = %v, %v", r, err)
	}
	slipJob := waitJob(t, d.server, r.GetJob().GetJobId())
	if slipJob.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED || slipJob.GetErrorReason() != "unpack_refused" {
		t.Fatalf("Extract zip-slip job = %v, want FAILED/unpack_refused", slipJob)
	}
	if there(filepath.Join(site, "slip")) || there(filepath.Join(site, "escape.txt")) {
		t.Error("zip-slip left files behind")
	}

	// Hard-linked archive is refused in preflight.
	good := filepath.Join(site, "ok.zip")
	if err := os.WriteFile(good, makeTestZip(t, map[string]string{"a.txt": "hi"}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(good, filepath.Join(site, "ok2.zip")); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.server.Extract(t.Context(), good, "", "", testArchiveOwner); r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("Extract hard-linked archive = %v, want KIND_REFUSED", r)
	}
}

// L3: extract counts skipped symlinks/hard links/FIFOs; compress counts left-out items.
func TestExtractCountsSkippedLinks(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	var tgz bytes.Buffer
	gw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "app.js", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.WriteHeader(&tar.Header{Name: "bin/tool", Linkname: "../app.js", Typeflag: tar.TypeSymlink})
	_ = tw.WriteHeader(&tar.Header{Name: "hard", Linkname: "app.js", Typeflag: tar.TypeLink})
	_ = tw.Close()
	_ = gw.Close()

	arc := filepath.Join(site, "pkg.tar.gz")
	if err := os.WriteFile(arc, tgz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	ext, err := d.server.Extract(t.Context(), arc, "", "", testArchiveOwner)
	if err != nil || ext.GetRefusal() != nil {
		t.Fatalf("Extract = %v, %v", ext, err)
	}
	done := waitJob(t, d.server, ext.GetJob().GetJobId())
	if done.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("job = %v", done)
	}
	if done.GetEntries() != 1 || done.GetSkippedLinks() != 2 {
		t.Errorf("entries=%d skipped_links=%d, want 1 and 2", done.GetEntries(), done.GetSkippedLinks())
	}
}

// M5: ReadWithOptions(..., true) returns binary preview bytes through the read path.
func TestReadFileAllowBinary(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d}
	imgPath := filepath.Join(site, "badge.png")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imgPath, png, 0o644); err != nil {
		t.Fatal(err)
	}

	plain, err := d.server.Read(t.Context(), imgPath)
	if err != nil || !plain.GetFile().GetBinary() || len(plain.GetFile().GetRawContent()) != 0 {
		t.Fatalf("plain Read on binary = %v, %v", plain, err)
	}
	preview, err := d.server.ReadWithOptions(t.Context(), imgPath, true)
	if err != nil || !preview.GetFile().GetBinary() || !bytes.Equal(preview.GetFile().GetRawContent(), png) {
		t.Fatalf("allowBinary Read = %v, %v", preview, err)
	}

	// Binary file carrying a PEM private key is still refused.
	keyBin := append(bytes.Clone(png), []byte("\n-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0BAQEFAASCBK\n-----END PRIVATE KEY-----\n")...)
	keyPath := filepath.Join(site, "hidden.png")
	if err := os.WriteFile(keyPath, keyBin, 0o644); err != nil {
		t.Fatal(err)
	}
	ref, err := d.server.ReadWithOptions(t.Context(), keyPath, true)
	if err != nil || ref.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("allowBinary Read with key = %v, %v, want KIND_REFUSED", ref, err)
	}
}

func TestDownloadManyStreamsSelectionAsZip(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "a.txt"), "alpha")
	write(t, filepath.Join(site, "sub", "b.txt"), "beta")
	write(t, filepath.Join(site, "secret.key"), "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0BAQEFAASCBK\n-----END PRIVATE KEY-----\n")

	var first *agentv1.SendFileRequest
	var body bytes.Buffer
	res, err := d.server.DownloadMany(t.Context(), []string{
		filepath.Join(site, "a.txt"),
		filepath.Join(site, "sub"),
	}, func(m *agentv1.SendFileRequest) error {
		if first == nil {
			first = m
		}
		body.Write(m.GetData())
		return nil
	})
	if err != nil || res.GetRefusal() != nil {
		t.Fatalf("DownloadMany = %v, %v", res, err)
	}
	if first.GetName() != "site.zip" || !first.GetArchive() {
		t.Fatalf("first frame = %v", first)
	}
	if len(first.GetResolvedPaths()) != 2 {
		t.Errorf("resolved_paths = %v, want 2", first.GetResolvedPaths())
	}
	got := zipNames(t, body.Bytes())
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got["a.txt"] != "alpha" || got["sub/b.txt"] != "beta" {
		t.Fatalf("zip entries = %v (%v)", got, keys)
	}

	// Refuses before sending anything when a single selected file holds a private key.
	sent := false
	ref, err := d.server.DownloadMany(t.Context(), []string{
		filepath.Join(site, "secret.key"),
	}, func(*agentv1.SendFileRequest) error {
		sent = true
		return nil
	})
	if err != nil || ref.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED || sent {
		t.Fatalf("DownloadMany single key = %v, %v, sent=%v", ref, err, sent)
	}

	// A multi-file selection leaves out the private key, lists it, and packs the rest (L3).
	var mixedBody bytes.Buffer
	mixed, err := d.server.DownloadMany(t.Context(), []string{
		filepath.Join(site, "a.txt"),
		filepath.Join(site, "secret.key"),
	}, func(m *agentv1.SendFileRequest) error {
		mixedBody.Write(m.GetData())
		return nil
	})
	if err != nil || mixed.GetRefusal() != nil {
		t.Fatalf("DownloadMany mixed = %v, %v", mixed, err)
	}
	if mixed.GetLeftOut() != 1 || len(mixed.GetLeftOutPaths()) != 1 || mixed.GetLeftOutPaths()[0] != "secret.key" {
		t.Errorf("DownloadMany mixed left_out = %d (%v), want 1 ([secret.key])", mixed.GetLeftOut(), mixed.GetLeftOutPaths())
	}
	mixedGot := zipNames(t, mixedBody.Bytes())
	if mixedGot["a.txt"] != "alpha" {
		t.Errorf("a.txt = %q, want alpha", mixedGot["a.txt"])
	}
	if _, leaked := mixedGot["secret.key"]; leaked {
		t.Error("secret.key leaked into selection zip")
	}
	mzr, err := zip.NewReader(bytes.NewReader(mixedBody.Bytes()), int64(mixedBody.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mzr.Comment, "secret.key") {
		t.Errorf("selection zip comment = %q, want secret.key listed", mzr.Comment)
	}
}

// N1: binary preview reads cap at MaxPreviewBytes (1 MiB), pre-size the read
// buffer without io.ReadAll doubling, and serialize through TakePreview so even
// four concurrent max-size previews stay within a tight heap delta.
func TestPreviewReadHeapAtCap(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, MaxPreviewBytes)
	copy(payload, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	for i := 8; i < len(payload); i++ {
		payload[i] = byte((i*31 + 7) & 0xff)
		if payload[i] == ':' {
			payload[i] = '_'
		}
	}
	imgPath := filepath.Join(site, "max.png")
	if err := os.WriteFile(imgPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	overPath := filepath.Join(site, "over.png")
	if err := os.WriteFile(overPath, append(bytes.Clone(payload), 0x42), 0o644); err != nil {
		t.Fatal(err)
	}

	// Over-cap image is truncated with empty RawContent.
	over, err := d.server.ReadWithOptions(t.Context(), overPath, true)
	if err != nil || !over.GetFile().GetBinary() || !over.GetFile().GetTruncated() || len(over.GetFile().GetRawContent()) != 0 {
		t.Fatalf("over-cap preview = %v, %v", over, err)
	}

	readHeap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	// Warm up one call and protobuf's one-time descriptor tables.
	warm, err := d.server.ReadWithOptions(t.Context(), imgPath, true)
	if err != nil || len(warm.GetFile().GetRawContent()) != MaxPreviewBytes {
		t.Fatalf("warm preview = %d bytes, %v", len(warm.GetFile().GetRawContent()), err)
	}
	if _, err := proto.Marshal(&agentv1.CallResult{Result: &agentv1.CallResult_ReadFile{ReadFile: warm}}); err != nil {
		t.Fatalf("warm marshal = %v", err)
	}
	warm = nil
	before := readHeap()

	// Single max-size preview + wire marshal.
	one, err := d.server.ReadWithOptions(t.Context(), imgPath, true)
	if err != nil || len(one.GetFile().GetRawContent()) != MaxPreviewBytes {
		t.Fatalf("single preview = %d bytes, %v", len(one.GetFile().GetRawContent()), err)
	}
	wire, err := proto.Marshal(&agentv1.CallResult{Result: &agentv1.CallResult_ReadFile{ReadFile: one}})
	if err != nil || len(wire) < MaxPreviewBytes {
		t.Fatalf("marshal single preview = %d, %v", len(wire), err)
	}
	singlePeak := readHeap()
	runtime.KeepAlive(one)
	runtime.KeepAlive(wire)
	singleDelta := int64(singlePeak) - int64(before)
	t.Logf("single %d KiB preview + marshal: baseline=%d KiB peak=%d KiB delta=%d KiB",
		MaxPreviewBytes/1024, before/1024, singlePeak/1024, singleDelta/1024)
	if singleDelta > 4<<20 {
		t.Errorf("single preview grew live heap by %d bytes (> 4 MiB)", singleDelta)
	}

	// Four concurrent previews serialized through TakePreview.
	before4 := readHeap()
	var peak4 uint64
	var peakMu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			rel, err := d.server.TakePreview(t.Context())
			if err != nil {
				errs <- err
				return
			}
			defer rel()
			res, err := d.server.ReadWithOptions(t.Context(), imgPath, true)
			if err != nil {
				errs <- err
				return
			}
			w, err := proto.Marshal(&agentv1.CallResult{Result: &agentv1.CallResult_ReadFile{ReadFile: res}})
			if err != nil {
				errs <- err
				return
			}
			h := readHeap()
			runtime.KeepAlive(res)
			runtime.KeepAlive(w)
			peakMu.Lock()
			peak4 = max(peak4, h)
			peakMu.Unlock()
			if len(w) < MaxPreviewBytes {
				errs <- fs.ErrInvalid
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent preview error: %v", err)
	}
	delta4 := int64(peak4) - int64(before4)
	t.Logf("4 concurrent %d KiB previews through TakePreview: baseline=%d KiB peak=%d KiB delta=%d KiB",
		MaxPreviewBytes/1024, before4/1024, peak4/1024, delta4/1024)
	if delta4 > 4<<20 {
		t.Errorf("4 concurrent previews grew live heap by %d bytes (> 4 MiB)", delta4)
	}
}

// N3: when the extract destination itself is a way in (~/.ssh, sudoers.d,
// systemd unit or drop-in folders, root's user units), each entry is checked
// by its full resolved path and a passkey never lets a held-back entry through.
func TestExtractWayInDestinationChecksFullPath(t *testing.T) {
	d := newDisk(t)
	adminHome := filepath.Join(d.root, "home", "admin")
	sysEtc := filepath.Join(d.root, "etc")
	d.server.accounts = func() (string, string) {
		return "root:x:0:0::" + filepath.Join(d.root, "root") + ":/bin/sh\nadmin:x:1000:1000::" + adminHome + ":/bin/sh\n", "sudo:x:27:admin\n"
	}
	d.server.systemPlaces = func() []string { return []string{sysEtc, filepath.Join(d.root, "root")} }

	cases := []struct {
		name        string
		archivePath string
		entryName   string
		entryBody   string
	}{
		{
			name:        "ssh rc when destination is ~/.ssh",
			archivePath: filepath.Join(adminHome, ".ssh.zip"),
			entryName:   "rc",
			entryBody:   "#!/bin/sh\nid\n",
		},
		{
			name:        "sudoers drop-in when destination is sudoers.d",
			archivePath: filepath.Join(sysEtc, "sudoers.d.zip"),
			entryName:   "90-all",
			entryBody:   "admin ALL=(ALL) NOPASSWD: ALL\n",
		},
		{
			name:        "systemd user unit when destination is root's user units",
			archivePath: filepath.Join(d.root, "root", ".config", "systemd", "user.zip"),
			entryName:   "x.service",
			entryBody:   "[Service]\nExecStart=/bin/sh\n",
		},
		{
			name:        "systemd system unit when destination is /etc/systemd/system",
			archivePath: filepath.Join(sysEtc, "systemd", "system.zip"),
			entryName:   "backdoor.service",
			entryBody:   "[Service]\nExecStart=/bin/sh\n",
		},
		{
			name:        "systemd drop-in override.conf when destination is ssh.service.d",
			archivePath: filepath.Join(sysEtc, "systemd", "system", "ssh.service.d.zip"),
			entryName:   "override.conf",
			entryBody:   "[Service]\nExecStartPre=/bin/sh\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(tc.archivePath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tc.archivePath, makeTestZip(t, map[string]string{tc.entryName: tc.entryBody}), 0o600); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(filepath.Dir(tc.archivePath), UnpackedName(filepath.Base(tc.archivePath)))
			// Without a passkey, preflight asks for step-up.
			noProof, err := d.server.Extract(t.Context(), tc.archivePath, "", "", testArchiveOwner)
			if err != nil || noProof.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
				t.Fatalf("Extract without passkey = %v, %v; want KIND_NEEDS_STEP_UP", noProof, err)
			}
			// Even WITH a passkey confirming both archive and destination, the entry must be refused!
			withProof, err := d.server.Extract(t.Context(), tc.archivePath, tc.archivePath, dst, testArchiveOwner)
			if err != nil || withProof.GetRefusal() != nil {
				t.Fatalf("Extract with passkey preflight = %v, %v", withProof, err)
			}
			done := waitJob(t, d.server, withProof.GetJob().GetJobId())
			if done.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED || done.GetErrorReason() != "unpack_refused" {
				t.Fatalf("Extract with passkey job = %v, want FAILED/unpack_refused", done)
			}
			if there(dst) {
				t.Errorf("refused extract left destination %s behind", dst)
			}
		})
	}
}

// N5 + N6: mode 0000 extracts as 0000 (not 0644), and unpacked folders inherit
// the parent directory's setgid bit while files never take setgid from the archive.
func TestExtractZeroModeAndParentSetgid(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	shared := filepath.Join(d.root, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, os.ModeSetgid|0o755); err != nil {
		t.Fatal(err)
	}
	sharedInfo, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	parentHasSetgid := sharedInfo.Mode()&fs.ModeSetgid != 0

	// Build a .tar.gz with a 0000 file, a 02755 setgid file, an explicit dir, and an implicit nested dir.
	var tgz bytes.Buffer
	gw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gw)
	addTar := func(name string, mode int64, body string, dir bool) {
		hdr := &tar.Header{Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if !dir {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	addTar("zero.txt", 0o000, "secret", false)
	addTar("sgid-file", 0o2755, "bin", false)
	addTar("explicit/", 0o755, "", true)
	addTar("implicit/nested/f.txt", 0o644, "ok", false)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	tgzPath := filepath.Join(shared, "bundle.tar.gz")
	if err := os.WriteFile(tgzPath, tgz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	ext, err := d.server.Extract(t.Context(), tgzPath, "", "", testArchiveOwner)
	if err != nil || ext.GetRefusal() != nil {
		t.Fatalf("Extract tgz = %v, %v", ext, err)
	}
	if j := waitJob(t, d.server, ext.GetJob().GetJobId()); j.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("tgz extract job = %v", j)
	}
	zeroInfo, err := os.Stat(filepath.Join(shared, "bundle", "zero.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if zeroInfo.Mode().Perm() != 0o000 {
		t.Errorf("0000 tar entry perm = %04o, want 0000", zeroInfo.Mode().Perm())
	}
	// Restore readable mode for TempDir cleanup.
	_ = os.Chmod(filepath.Join(shared, "bundle", "zero.txt"), 0o600)

	sgidFileInfo, err := os.Stat(filepath.Join(shared, "bundle", "sgid-file"))
	if err != nil {
		t.Fatal(err)
	}
	if sgidFileInfo.Mode()&fs.ModeSetgid != 0 {
		t.Errorf("extracted file kept setgid bit: %v", sgidFileInfo.Mode())
	}
	if parentHasSetgid {
		for _, rel := range []string{"", "explicit", "implicit", "implicit/nested"} {
			info, err := os.Stat(filepath.Join(shared, "bundle", rel))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&fs.ModeSetgid == 0 {
				t.Errorf("extracted folder %q lost parent setgid bit: %v", rel, info.Mode())
			}
		}
	}

	// Build a zip with a Unix 0000 file entry and verify it also extracts as 0000 and inherits folder setgid.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	hdr := &zip.FileHeader{Name: "sub/zero-zip.txt", Method: zip.Deflate}
	hdr.SetMode(0o000)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("zipzero")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(shared, "zbundle.zip")
	if err := os.WriteFile(zipPath, zbuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	zext, err := d.server.Extract(t.Context(), zipPath, "", "", testArchiveOwner)
	if err != nil || zext.GetRefusal() != nil {
		t.Fatalf("Extract zip = %v, %v", zext, err)
	}
	if j := waitJob(t, d.server, zext.GetJob().GetJobId()); j.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE {
		t.Fatalf("zip extract job = %v", j)
	}
	zzInfo, err := os.Stat(filepath.Join(shared, "zbundle", "sub", "zero-zip.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if zzInfo.Mode().Perm() != 0o000 {
		t.Errorf("0000 zip entry perm = %04o, want 0000", zzInfo.Mode().Perm())
	}
	_ = os.Chmod(filepath.Join(shared, "zbundle", "sub", "zero-zip.txt"), 0o600)
	if parentHasSetgid {
		for _, rel := range []string{"", "sub"} {
			info, err := os.Stat(filepath.Join(shared, "zbundle", rel))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&fs.ModeSetgid == 0 {
				t.Errorf("zip extracted folder %q lost parent setgid bit: %v", rel, info.Mode())
			}
		}
	}
}

// N7: stray compress staging files left behind by an agent restart are swept
// on agent start, while unrelated files and actively-running jobs' staging files stay.
func TestSweepArchiveStagingOnAgentStart(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(site, "keep.txt")
	write(t, keep, "safe")

	// Simulate an abandoned compress staging file left behind before a restart.
	abandonedID := "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	strayName := uploadPrefix + "BBBBBBBBBBBBBBBBBBBBBBBBBB"
	strayPath := filepath.Join(site, strayName)
	if err := os.WriteFile(strayPath, []byte("partial-zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(strayPath, &st); err != nil {
		t.Fatal(err)
	}
	if err := d.server.saveArchiveStage(&archiveStage{
		ID:   abandonedID,
		Path: filepath.Join(site, "out.zip"),
		Name: strayName,
		Dev:  st.Dev,
		Ino:  st.Ino,
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate an actively running job whose staging file must NOT be swept.
	activeID := "CCCCCCCCCCCCCCCCCCCCCCCCCC"
	activeName := uploadPrefix + "DDDDDDDDDDDDDDDDDDDDDDDDDD"
	activePath := filepath.Join(site, activeName)
	if err := os.WriteFile(activePath, []byte("active-zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	var activeSt unix.Stat_t
	if err := unix.Stat(activePath, &activeSt); err != nil {
		t.Fatal(err)
	}
	if err := d.server.saveArchiveStage(&archiveStage{
		ID:   activeID,
		Path: filepath.Join(site, "active.zip"),
		Name: activeName,
		Dev:  activeSt.Dev,
		Ino:  activeSt.Ino,
	}); err != nil {
		t.Fatal(err)
	}
	d.server.registerArchiveJob(&archiveJob{
		id:        activeID,
		owner:     testArchiveOwner,
		kind:      dockwellv1.ArchiveJobKind_ARCHIVE_JOB_KIND_COMPRESS,
		state:     dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_RUNNING,
		path:      filepath.Join(site, "active.zip"),
		startTime: time.Now(),
		done:      make(chan struct{}),
	})

	d.server.SweepArchiveStaging(t.Context())

	if there(strayPath) {
		t.Error("abandoned compress staging file was not removed on agent start sweep")
	}
	if there(filepath.Join(d.server.archiveStagingDir(), abandonedID+".json")) {
		t.Error("abandoned archive staging note was not dropped")
	}
	if !there(activePath) {
		t.Error("actively running job's staging file was wrongly removed")
	}
	if !there(keep) {
		t.Error("unrelated file was removed")
	}
}
