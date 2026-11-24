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
	"sort"
	"strings"
	"testing"
	"time"

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

	archive := filepath.Join(site, "leak.zip")
	comp, err := d.server.Compress(t.Context(), []string{
		filepath.Join(site, "ok.txt"),
		filepath.Join(site, "secret.pem"),
	}, archive, "", testArchiveOwner)
	if err != nil || comp.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("Compress with private key = %v, %v, want KIND_REFUSED", comp, err)
	}
	if there(archive) {
		t.Error("refused archive was created on disk")
	}
	entries, _ := os.ReadDir(site)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), uploadPrefix) {
			t.Errorf("staging file %s was left behind", e.Name())
		}
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
	defer close(unblock)
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

	// Refuses before sending anything when a selected file holds a private key.
	sent := false
	ref, err := d.server.DownloadMany(t.Context(), []string{
		filepath.Join(site, "a.txt"),
		filepath.Join(site, "secret.key"),
	}, func(*agentv1.SendFileRequest) error {
		sent = true
		return nil
	})
	if err != nil || ref.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED || sent {
		t.Fatalf("DownloadMany with key = %v, %v, sent=%v", ref, err, sent)
	}
}
