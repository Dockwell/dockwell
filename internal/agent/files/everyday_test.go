// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/netguard"
)

func TestSearchAndFolderSizeStopAtTheFilesBoundary(t *testing.T) {
	d := newDisk(t)
	tree := filepath.Join(d.root, "project")
	guide := filepath.Join(tree, "notes", "guide.txt")
	write(t, guide, "a line\nlook for the lighthouse")
	write(t, filepath.Join(tree, "lighthouse-key.txt"), "lighthouse\n-----BEGIN OPENSSH PRIVATE KEY-----\n")
	write(t, filepath.Join(tree, ".pgpass"), "lighthouse")
	write(t, filepath.Join(d.root, "outside.txt"), "lighthouse")
	link(t, d.root, filepath.Join(tree, "outside"))
	if err := os.Link(guide, filepath.Join(tree, "hard-link.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tree, "notes", "other.txt"), "bytes")
	// The other name is refused by Files, as is the original hard-linked file.
	found, err := d.server.SearchFiles(t.Context(), tree, "lighthouse", true)
	if err != nil || found.GetRefusal() != nil || len(found.GetResponse().GetMatches()) != 0 {
		t.Fatalf("search crossed its read boundary: %v, %v", found, err)
	}
	size, err := d.server.CalculateFolderSize(t.Context(), tree)
	if got := size.GetResponse(); err != nil || got.GetSizeBytes() != 82 || !got.GetPartial() ||
		!slices.Contains(got.GetOmissions(), sizeHardLinks) || !slices.Contains(got.GetOmissions(), sizeRefused) {
		t.Fatalf("size with hard links and refusals = %v, %v", size, err)
	}
	docker, err := d.server.CalculateFolderSize(t.Context(), d.docker)
	if got := docker.GetResponse(); err != nil || got.GetSizeBytes() != 14 || !got.GetPartial() ||
		!slices.Contains(got.GetOmissions(), sizeDocker) {
		t.Fatalf("Docker size = %v, %v", docker, err)
	}
	if err := os.Remove(filepath.Join(tree, "hard-link.txt")); err != nil {
		t.Fatal(err)
	}
	found, err = d.server.SearchFiles(t.Context(), tree, "LIGHTHOUSE", true)
	m := found.GetResponse().GetMatches()
	if err != nil || len(m) != 1 || m[0].GetPath() != guide || m[0].GetLine() != 2 {
		t.Fatalf("search = %v, %v", found, err)
	}
	n, err := d.server.CalculateFolderSize(t.Context(), filepath.Join(tree, "notes"))
	if err != nil || n.GetResponse().GetSizeBytes() != 35 || n.GetResponse().GetPartial() {
		t.Fatalf("size = %v, %v", n, err)
	}
	deep := filepath.Join(tree, strings.Repeat("folder/", scanDepth+1))
	write(t, filepath.Join(deep, "deep.txt"), "too deep")
	n, err = d.server.CalculateFolderSize(t.Context(), tree)
	if err != nil || !n.GetResponse().GetPartial() || n.GetResponse().GetSizeBytes() < 35 {
		t.Fatalf("bounded size = %v, %v", n, err)
	}
}

func TestRecursiveAccessConfirmsProtectedEntriesAndLeavesLinksAlone(t *testing.T) {
	d := newDisk(t)
	tree := filepath.Join(d.root, "project")
	member := filepath.Join(tree, "subfolder", "one.txt")
	wayIn := filepath.Join(tree, "authorized_keys")
	outside := filepath.Join(d.root, "outside.txt")
	write(t, member, "data")
	write(t, wayIn, "an authorized key")
	write(t, outside, "keep this")
	link(t, outside, filepath.Join(tree, "link"))
	if err := os.Link(outside, filepath.Join(tree, "hard-link")); err != nil {
		t.Fatal(err)
	}
	call := &agentv1.ChangeFileAccessCall{
		Path: tree, Change: &dockwellv1.FileAccessChange{Mode: proto.Uint32(0o700), Recursive: true},
	}
	out, err := d.server.ChangeFileAccess(t.Context(), call)
	if err != nil || out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP || out.GetConfirmation() == "" {
		t.Fatalf("change without proof = %v, %v", out, err)
	}
	assertPermissions(t, member, 0o644)
	call.Confirmed = out.GetConfirmation()
	call.Change.Mode = proto.Uint32(0o777)
	out, err = d.server.ChangeFileAccess(t.Context(), call)
	if err != nil || out.GetRefusal().GetKind() == agentv1.FileRefusal_KIND_UNSPECIFIED {
		t.Fatalf("changed operation reused proof: %v, %v", out, err)
	}
	assertPermissions(t, member, 0o644)
	call.Change.Mode = proto.Uint32(0o700)
	out, err = d.server.ChangeFileAccess(t.Context(), call)
	if err != nil || !out.GetResponse().GetWayBackIn() || out.GetResponse().GetChanged() != 4 || out.GetResponse().GetSkipped() != 2 {
		t.Fatalf("approved change = %v, %v", out, err)
	}
	assertPermissions(t, member, 0o600)
	assertPermissions(t, filepath.Dir(member), 0o700)
	assertPermissions(t, outside, 0o644)
	if err := CheckAccessChange(member, &dockwellv1.FileAccessChange{Mode: proto.Uint32(0o4700)}); err == nil {
		t.Fatal("an implicit set-ID change was accepted")
	}
}

func changeAccess(t *testing.T, s *Server, path string, change *dockwellv1.FileAccessChange) *dockwellv1.ChangeFileAccessResponse {
	t.Helper()
	call := &agentv1.ChangeFileAccessCall{Path: path, Change: change}
	out, err := s.ChangeFileAccess(t.Context(), call)
	if out.GetRefusal().GetKind() == agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		call.Confirmed = out.GetConfirmation()
		out, err = s.ChangeFileAccess(t.Context(), call)
	}
	if err != nil || out.GetResponse() == nil {
		t.Fatalf("change %s = %v, %v", path, out, err)
	}
	return out.GetResponse()
}

func TestRecursive644LeavesFoldersEnterableAndCanBeUndone(t *testing.T) {
	d := newDisk(t)
	tree := filepath.Join(d.root, "site")
	sub := filepath.Join(tree, "assets")
	plain, executable := filepath.Join(sub, "index.php"), filepath.Join(sub, "deploy.sh")
	write(t, plain, "plain")
	write(t, executable, "executable")
	for name, mode := range map[string]os.FileMode{tree: 0o700, sub: 0o750, plain: 0o600, executable: 0o711} {
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
	}
	out := changeAccess(t, d.server, tree, &dockwellv1.FileAccessChange{Mode: proto.Uint32(0o644), Recursive: true})
	if out.GetChanged() != 4 || len(out.GetUndo()) != 4 {
		t.Fatalf("change = %v", out)
	}
	assertPermissions(t, tree, 0o755)
	assertPermissions(t, sub, 0o755)
	assertPermissions(t, plain, 0o644)
	assertPermissions(t, executable, 0o755)
	if err := os.Chmod(executable, 0o700); err != nil {
		t.Fatal(err)
	}
	undone := changeAccess(t, d.server, tree, &dockwellv1.FileAccessChange{Restore: out.GetUndo(), Recursive: true})
	if undone.GetChanged() != 3 || undone.GetSkipped() != 1 {
		t.Fatalf("undo = %v", undone)
	}
	assertPermissions(t, tree, 0o700)
	assertPermissions(t, sub, 0o750)
	assertPermissions(t, plain, 0o600)
	assertPermissions(t, executable, 0o700)
}

func TestRecursiveAccessRefusesAnOversizedUndoBeforeWriting(t *testing.T) {
	d := newDisk(t)
	d.server.AnswerWithin = 10 * time.Second
	tree := filepath.Join(d.root, "site")
	var paths []string
	pathBytes := len(tree)
	for i := range 3_000 {
		name := filepath.Join(tree, fmt.Sprintf("%04d-%s", i, strings.Repeat("a", 200)))
		write(t, name, "")
		paths = append(paths, name)
		pathBytes += len(name)
	}
	out, err := d.server.ChangeFileAccess(t.Context(), &agentv1.ChangeFileAccessCall{
		Path: tree, Change: &dockwellv1.FileAccessChange{
			Mode: proto.Uint32(0o750), FileMode: proto.Uint32(0o640), Recursive: true,
		},
	})
	r := out.GetRefusal()
	want := fmt.Sprintf("%d entries would need %d bytes of paths, over the %d-byte limit", len(paths)+1, pathBytes, maxAccessPathsBytes)
	if err != nil || out.GetResponse() != nil || r.GetKind() != agentv1.FileRefusal_KIND_TOO_LARGE || !strings.Contains(r.GetMessage(), want) {
		t.Fatalf("oversized undo = %v, %v; want %q", out, err, want)
	}
	assertPermissions(t, tree, 0o755)
	for _, path := range paths {
		assertPermissions(t, path, 0o644)
	}
}

func TestAccessDoesNotReadFilesAndCanNarrowKeys(t *testing.T) {
	d := newDisk(t)
	tree := filepath.Join(d.root, "site")
	for i := range 100 {
		file := filepath.Join(tree, fmt.Sprintf("asset-%d", i))
		write(t, file, "")
		if err := os.Truncate(file, 2<<20); err != nil {
			t.Fatal(err)
		}
	}
	key := filepath.Join(tree, "id_ed25519")
	write(t, key, "-----BEGIN OPENSSH PRIVATE KEY-----")
	list := filepath.Join(tree, ".htpasswd")
	write(t, list, "stored password hashes")
	out := changeAccess(t, d.server, tree, &dockwellv1.FileAccessChange{
		Mode: proto.Uint32(0o700), FileMode: proto.Uint32(0o600), Recursive: true,
	})
	if out.GetChanged() != 103 || out.GetSkipped() != 0 {
		t.Fatalf("large tree change = %v", out)
	}
	for _, p := range []string{key, list} {
		assertPermissions(t, p, 0o600)
		widened, err := d.server.ChangeFileAccess(t.Context(), &agentv1.ChangeFileAccessCall{
			Path: p, Change: &dockwellv1.FileAccessChange{Mode: proto.Uint32(0o644)},
		})
		if err != nil || widened.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
			t.Fatalf("widened credential mode without approval: %v, %v", widened, err)
		}
	}
}

func TestAccessChecksRenamedCredentialsOnlyWhenWidening(t *testing.T) {
	d := newDisk(t)
	tree := filepath.Join(d.root, "site")
	plain := filepath.Join(tree, "about")
	// Binary data can contain bytes shaped like a hash line (ADR 0045).
	write(t, plain, "binary\x00\noperator:$6$test-hash\n")
	for name, content := range map[string]string{
		"deploy":   "settings\n  -----BEGIN OPENSSH PRIVATE KEY-----\n",
		"accounts": "operator:$6$test-hash\n",
	} {
		path := filepath.Join(tree, name)
		write(t, path, content)
		out, err := d.server.ChangeFileAccess(t.Context(), &agentv1.ChangeFileAccessCall{
			Path: path, Change: &dockwellv1.FileAccessChange{Mode: proto.Uint32(0o600)},
		})
		if err != nil || out.GetResponse().GetChanged() != 1 || out.GetResponse().GetWayBackIn() {
			t.Fatalf("narrowing %s = %v, %v", name, out, err)
		}
		for _, change := range []*dockwellv1.FileAccessChange{
			{Mode: proto.Uint32(0o644)},
			{Uid: proto.Uint32(uint32(os.Getuid()) + 1)},
			{Gid: proto.Uint32(uint32(os.Getgid()) + 1)},
		} {
			out, err := d.server.ChangeFileAccess(t.Context(), &agentv1.ChangeFileAccessCall{Path: path, Change: change})
			if err != nil || out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
				t.Fatalf("widening %s with %v = %v, %v", name, change, out, err)
			}
		}
		assertPermissions(t, path, 0o600)
	}
	change := &dockwellv1.FileAccessChange{Mode: proto.Uint32(0o664)}
	out, err := d.server.ChangeFileAccess(t.Context(), &agentv1.ChangeFileAccessCall{Path: plain, Change: change})
	if err != nil || out.GetResponse().GetChanged() != 1 || out.GetResponse().GetWayBackIn() {
		t.Fatalf("widening a plain file = %v, %v", out, err)
	}
	out, err = d.server.ChangeFileAccess(t.Context(), &agentv1.ChangeFileAccessCall{
		Path: tree, Change: &dockwellv1.FileAccessChange{Recursive: true, FileMode: proto.Uint32(0o644)},
	})
	if err != nil || out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Fatalf("widening credentials in a tree = %v, %v", out, err)
	}
	assertPermissions(t, plain, 0o664)
}

func TestAccessDeadlineReportsCompletedChanges(t *testing.T) {
	d := newDisk(t)
	tree := filepath.Join(d.root, "site")
	file := filepath.Join(tree, "one.txt")
	write(t, file, "one")
	d.server.AnswerWithin = 250 * time.Millisecond
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	reached := make(chan struct{}, 1)
	d.server.Touching = func(path string) {
		if path != tree {
			return
		}
		st, err := os.Stat(file)
		if err == nil && st.Mode().Perm() == 0o600 {
			reached <- struct{}{}
			<-release
		}
	}
	c := &dockwellv1.FileAccessChange{Mode: proto.Uint32(0o700), Recursive: true}
	out := changeAccess(t, d.server, tree, c)
	if len(reached) != 1 || out.GetChanged() != 1 || out.GetStopped() == "" || len(out.GetUndo()) != 1 {
		t.Fatalf("stopped change = %v", out)
	}
	unblock()
	assertPermissions(t, tree, 0o755)
	assertPermissions(t, file, 0o600)
}

func TestDownloadNamesAndAddressScope(t *testing.T) {
	for end, want := range map[string]string{
		"release-4.tar.gz?sig=secret":              "release-4.tar.gz",
		"access-token-stuff.txt":                   "download",
		"aVG92rjtTdh8lros.zip":                     "download",
		"b39dcaf2-20fe-48b4-ab03-63cc69c27c51.zip": "download",
		"filename%3Ftoken.txt":                     "download",
	} {
		dest, err := DownloadDestination("/tmp", "https://example.org/"+end, "")
		if err != nil || filepath.Base(dest) != want {
			t.Errorf("name for %q = %q, %v", end, dest, err)
		}
	}
	if got := DownloadOrigin("https://example.org:8000/path?secret=token"); got != "https://example.org:8000" {
		t.Errorf("download origin = %q", got)
	}
	d := newDisk(t)
	d.server.DownloadHosts = []string{"127.0.0.1", "8.8.8.8"}
	local := []netip.Prefix{netip.MustParsePrefix("8.8.8.8/32")}
	for _, host := range []string{"127.0.0.1", "8.8.8.8"} {
		u, _ := downloadURL("http://" + host)
		if why := netguard.Forbidden(host, d.server.downloadOptions(u, "public.example", local, time.Second)); why == "" {
			t.Errorf("redirect from public URL permitted to %s", host)
		}
		if why := netguard.Forbidden(host, d.server.downloadOptions(u, host, local, time.Second)); why != "" {
			t.Errorf("explicit allowlisted host %s was blocked: %s", host, why)
		}
	}
}

func assertPermissions(t *testing.T, p string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != want {
		t.Fatalf("%s has permissions %o, want %o", p, fi.Mode().Perm(), want)
	}
}

func TestURLDownloadsGuardRedirectsAndCancelWithoutPublishing(t *testing.T) {
	d := newDisk(t)
	done := make(chan struct{})
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "http://"+strings.ReplaceAll(r.Host, "127.0.0.1", "localhost"), http.StatusFound)
		case "/large":
			w.Header().Set("Content-Length", "1073741825")
			w.WriteHeader(http.StatusOK)
		case "/slow":
			_, _ = w.Write([]byte("part of the file"))
			w.(http.Flusher).Flush()
			close(done)
			<-r.Context().Done()
		default:
			_, _ = w.Write([]byte("downloaded bytes"))
		}
	}))
	defer site.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	begin := func(url, name string) *agentv1.DownloadFileFromURLCall {
		t.Helper()
		call := &agentv1.DownloadFileFromURLCall{Path: filepath.Join(d.root, name), Url: url, Owner: "owner/session", JobId: rand.Text()}
		started, err := d.server.DownloadFileFromURL(ctx, call)
		if err != nil {
			t.Fatal(err)
		}
		if started.GetJob() == nil {
			if started.GetRefusal().GetKind() == agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
				call.Confirmed = started.GetConfirmation()
				started, err = d.server.DownloadFileFromURL(ctx, call)
			}
		}
		if err != nil || started.GetJob() == nil {
			t.Fatalf("start download: %v, %v", started, err)
		}
		return call
	}
	finish := func(r *agentv1.DownloadFileFromURLCall) *dockwellv1.ArchiveJob {
		t.Helper()
		job, err := d.server.WaitArchiveJob(ctx, r.GetJobId())
		if err != nil {
			t.Fatal(err)
		}
		return job
	}
	blocked := begin(site.URL+"//?signature=secret", "blocked")
	if job := finish(blocked); job.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED ||
		!strings.Contains(job.GetError(), "download host") || strings.Contains(job.GetError(), "secret") {
		t.Fatalf("guarded download = %v", job)
	}
	d.server.DownloadHosts = []string{"127.0.0.1", "localhost"}
	if job := finish(begin(site.URL+"/redirect", "redirected")); job.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED ||
		!strings.Contains(job.GetError(), "download host") {
		t.Fatalf("redirect must be guarded separately: %v", job)
	}
	if job := finish(begin(site.URL+"/large", "large")); job.GetErrorReason() != "too_large" {
		t.Fatalf("oversize download = %v", job)
	}
	ok := begin(site.URL+"/file.txt", "fetched.txt")
	if job := finish(ok); job.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_DONE || job.GetBytes() != 16 {
		t.Fatalf("download = %v", job)
	}
	if b, err := os.ReadFile(ok.GetPath()); err != nil || string(b) != "downloaded bytes" {
		t.Fatalf("published bytes = %q, %v", b, err)
	}
	slow := begin(site.URL+"/slow", "cancelled")
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	entries, err := os.ReadDir(d.root)
	if err != nil {
		t.Fatal(err)
	}
	var stage string
	for _, item := range entries {
		if arriving(item.Name()) {
			stage = filepath.Join(d.root, item.Name())
		}
	}
	if stage == "" {
		t.Fatal("running download has no staging directory")
	}
	if r := d.read(t, filepath.Join(stage, downloadStageFile)); r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("reading partial by name = %v", r)
	}
	renamed := filepath.Join(d.root, "renamed-stage")
	if err := os.Rename(stage, renamed); err != nil {
		t.Fatal(err)
	}
	if r := d.read(t, filepath.Join(renamed, downloadStageFile)); r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Fatalf("reading partial by renamed path = %v", r)
	}
	if out, _ := d.server.CancelFileDownload(slow.GetJobId(), "owner/other"); out.GetRefusal() == nil {
		t.Fatal("another session could cancel the download")
	}
	if out, _ := d.server.CancelFileDownload(slow.GetJobId(), slow.GetOwner()); out.GetJob() == nil {
		t.Fatal("the initiating session could not cancel the download")
	}
	if job := finish(slow); job.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_CANCELLED {
		t.Fatalf("cancelled download = %v", job)
	}
	for _, name := range []string{"blocked", "large", "cancelled"} {
		if _, err := os.Stat(filepath.Join(d.root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was published: %v", name, err)
		}
	}
}

func TestURLDownloadPublicationStopsWaiting(t *testing.T) {
	d := newDisk(t)
	served := make(chan struct{})
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(served)
		_, _ = w.Write([]byte("downloaded bytes"))
	}))
	defer site.Close()
	d.server.DownloadHosts = []string{"127.0.0.1"}
	d.server.AnswerWithin = 250 * time.Millisecond
	dest := filepath.Join(d.root, "stalled.txt")
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	d.server.Touching = func(path string) {
		if path != dest {
			return
		}
		select {
		case <-served:
			close(held)
			<-release
		default:
		}
	}
	call := &agentv1.DownloadFileFromURLCall{Path: dest, Url: site.URL, Owner: "owner/session", JobId: rand.Text()}
	started, err := d.server.DownloadFileFromURL(t.Context(), call)
	if started.GetRefusal().GetKind() == agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		call.Confirmed = started.GetConfirmation()
		started, err = d.server.DownloadFileFromURL(t.Context(), call)
	}
	if err != nil || started.GetJob() == nil {
		t.Fatalf("start download = %v, %v", started, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	job, err := d.server.WaitArchiveJob(ctx, call.GetJobId())
	if err != nil || job.GetState() != dockwellv1.ArchiveJobState_ARCHIVE_JOB_STATE_FAILED ||
		job.GetErrorReason() != "path_not_answering" {
		t.Fatalf("stalled download = %v, %v", job, err)
	}
	select {
	case <-held:
	default:
		t.Fatal("download did not reach the publication hold")
	}
	unblock()
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stalled download published a file: %v", err)
	}
}

func TestDownloadFreeSpaceKeepsTheMarginUpFront(t *testing.T) {
	room := &boundedZipWriter{where: "/tmp/download", statfs: func(_ int, fs *unix.Statfs_t) error {
		fs.Blocks, fs.Bavail, fs.Bsize = 100, 10, 4096
		return nil
	}}
	if err := room.checkRoomFor(6 * 4096); err == nil || room.full == nil {
		t.Fatal("known download length was allowed to consume the free-space margin")
	}
}
