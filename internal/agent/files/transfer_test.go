// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// me is who the tests upload as, as the control plane names a user and a session.
const me = "usr_1/ses_1"

// inParts gives data a part at a time, as ReceiveFile would, then says there's no more.
func inParts(data []byte, size int) func() ([]byte, error) {
	return func() ([]byte, error) {
		if len(data) == 0 {
			return nil, io.EOF
		}
		n := min(size, len(data))
		part := data[:n]
		data = data[n:]
		return part, nil
	}
}

func (d *fakeDisk) begin(t *testing.T, p string, size int, unpack dockwellv1.Unpack, confirmed string) *agentv1.UploadResult {
	t.Helper()
	r, err := d.server.BeginUpload(t.Context(), p, uint64(size), unpack, "", confirmed, me)
	if err != nil {
		t.Fatalf("BeginUpload(%s): %v", p, err)
	}
	mustSend(t, r)
	return r
}

func (d *fakeDisk) upload(t *testing.T, p string, data []byte, unpack dockwellv1.Unpack) *agentv1.FinishUploadResult {
	t.Helper()
	up := d.begin(t, p, len(data), unpack, "").GetUpload()
	if up == nil {
		t.Fatalf("an upload to %s didn't begin", p)
	}
	w, err := d.server.WriteUpload(t.Context(), up.GetUploadId(), me, 0, inParts(data, 1000))
	if err != nil || w.GetRefusal() != nil {
		t.Fatalf("WriteUpload = %v, %v", w, err)
	}
	f, err := d.server.FinishUpload(t.Context(), up.GetUploadId(), me)
	if err != nil {
		t.Fatal(err)
	}
	mustSend(t, f)
	return f
}

func TestUploadingAFile(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "index.html"), "hi")
	data := bytes.Repeat([]byte("0123456789"), 5000)

	up := d.begin(t, filepath.Join(site, "big.bin"), len(data), 0, "").GetUpload()
	if up.GetMaxBytes() != MaxUploadBytes || up.GetReceivedBytes() != 0 || up.GetPath() != filepath.Join(site, "big.bin") {
		t.Fatalf("began %v", up)
	}
	if there(filepath.Join(site, "big.bin")) {
		t.Fatal("it's in place before it's all there")
	}
	// Half, then the connection drops.
	half := data[:len(data)/2]
	w, _ := d.server.WriteUpload(t.Context(), up.GetUploadId(), me, 0, inParts(half, 777))
	if w.GetUpload().GetReceivedBytes() != uint64(len(half)) {
		t.Fatalf("wrote %v", w)
	}
	if f, _ := d.server.FinishUpload(t.Context(), up.GetUploadId(), me); f.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_OFFSET {
		t.Errorf("finishing half an upload: %v", f)
	}
	// Picked up from where it got, and only from there.
	again, _ := d.server.BeginUpload(t.Context(), "", 0, 0, up.GetUploadId(), "", me)
	if again.GetUpload().GetReceivedBytes() != uint64(len(half)) {
		t.Fatalf("where it got: %v", again)
	}
	if w, _ := d.server.WriteUpload(t.Context(), up.GetUploadId(), me, 0, inParts(data, 1000)); w.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_OFFSET {
		t.Errorf("a write from the start: %v", w)
	}
	w, _ = d.server.WriteUpload(t.Context(), up.GetUploadId(), me, uint64(len(half)), inParts(data[len(half):], 999))
	if w.GetUpload().GetReceivedBytes() != uint64(len(data)) {
		t.Fatalf("the rest: %v", w)
	}
	f, _ := d.server.FinishUpload(t.Context(), up.GetUploadId(), me)
	if f.GetFinished().GetPath() != filepath.Join(site, "big.bin") || f.GetFinished().GetFile().GetSizeBytes() != uint64(len(data)) {
		t.Fatalf("finished %v", f)
	}
	if got, _ := os.ReadFile(filepath.Join(site, "big.bin")); !bytes.Equal(got, data) {
		t.Error("what's there isn't what was sent")
	}
	if mode := modeOf(t, filepath.Join(site, "big.bin")); mode.Perm() != 0o644 {
		t.Errorf("it has %v", mode)
	}
	if left, _ := filepath.Glob(filepath.Join(site, uploadPrefix+"*")); len(left) != 0 {
		t.Errorf("left beside it: %v", left)
	}
	if again, _ := d.server.BeginUpload(t.Context(), "", 0, 0, up.GetUploadId(), "", me); again.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UPLOAD_GONE {
		t.Errorf("a finished upload picked up: %v", again)
	}
	// More than it said is refused, and never over what's there.
	more := d.begin(t, filepath.Join(site, "more.bin"), 10, 0, "").GetUpload()
	if w, _ := d.server.WriteUpload(t.Context(), more.GetUploadId(), me, 0, inParts(data, 100)); w.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_TOO_LARGE {
		t.Errorf("more than it said: %v", w)
	}
	if got := d.begin(t, filepath.Join(site, "index.html"), 2, 0, "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_EXISTS {
		t.Errorf("over what's there: %v", got)
	}
	if r, _ := d.server.BeginUpload(t.Context(), filepath.Join(site, "x"), MaxUploadBytes+1, 0, "", "", me); r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_TOO_LARGE {
		t.Errorf("past the limit: %v", r)
	}
	// What a read refuses, an upload does.
	if got := d.begin(t, filepath.Join(d.data, "x"), 1, 0, "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("into Dockwell's data: %v", got)
	}
}

// Something put where an upload goes, while it went, isn't replaced.
func TestAnUploadNeverReplaces(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "keep"), "keep")
	up := d.begin(t, filepath.Join(site, "a.txt"), 3, 0, "").GetUpload()
	_, _ = d.server.WriteUpload(t.Context(), up.GetUploadId(), me, 0, inParts([]byte("new"), 10))
	write(t, filepath.Join(site, "a.txt"), "made meanwhile")
	if f, _ := d.server.FinishUpload(t.Context(), up.GetUploadId(), me); f.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_EXISTS {
		t.Errorf("= %v", f)
	}
	if got, _ := os.ReadFile(filepath.Join(site, "a.txt")); string(got) != "made meanwhile" {
		t.Errorf("it holds %q", got)
	}
}

// What lets someone back in takes a passkey to upload, as it does to make.
func TestUploadingAWayBackInTakesAPasskey(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	keys := filepath.Join(d.root, ".ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(keys), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := d.begin(t, keys, 5, 0, "").GetRefusal(); got.GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Fatalf("with no passkey: %v", got)
	}
	up := d.begin(t, keys, 5, 0, keys).GetUpload()
	if !up.GetWayBackIn() {
		t.Fatalf("with one: %v", up)
	}
	_, _ = d.server.WriteUpload(t.Context(), up.GetUploadId(), me, 0, inParts([]byte("ssh-e"), 10))
	if f, _ := d.server.FinishUpload(t.Context(), up.GetUploadId(), me); !f.GetFinished().GetWayBackIn() {
		t.Errorf("finished %v", f)
	}
}

// An upload's file is what the upload made: a folder swapped for a link to another
// stops it, and what's elsewhere stays as it is.
func TestAnUploadGoesWhereItBegan(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	dir := filepath.Join(d.root, "site", "dir")
	outside := filepath.Join(d.root, "outside")
	write(t, filepath.Join(dir, "x"), "x")
	write(t, filepath.Join(outside, "y"), "y")
	up := d.begin(t, filepath.Join(dir, "a.txt"), 3, 0, "").GetUpload()
	if err := os.Rename(dir, dir+"-checked"); err != nil {
		t.Fatal(err)
	}
	link(t, outside, dir)
	if w, _ := d.server.WriteUpload(t.Context(), up.GetUploadId(), me, 0, inParts([]byte("abc"), 10)); w.GetRefusal() == nil {
		t.Errorf("wrote through the link: %v", w)
	}
	if there(filepath.Join(outside, "a.txt")) {
		t.Error("it went where the link leads")
	}
}

// A path that became a way back in after its upload began, as an admin's home is
// there now, took no passkey, so the upload isn't put in place.
func TestAnUploadThatBecameAWayInIsntPutInPlace(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	home := filepath.Join(d.root, "srv", "deploy")
	write(t, filepath.Join(home, "notes.txt"), "n")
	p := filepath.Join(home, ".bashrc")
	up := d.begin(t, p, 3, 0, "").GetUpload()
	if up == nil || up.GetWayBackIn() {
		t.Fatalf("began %v", up)
	}
	_, _ = d.server.WriteUpload(t.Context(), up.GetUploadId(), me, 0, inParts([]byte("abc"), 10))
	d.adminAccounts(home)
	if f, _ := d.server.FinishUpload(t.Context(), up.GetUploadId(), me); f.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_CHANGED || f.GetFinished() != nil {
		t.Errorf("finished %v", f)
	}
	if there(p) {
		t.Error("it's in place with no passkey")
	}
}

// An upload is its beginner's. Another session, even one that knows its ID, finds
// no such upload, and its file is in no listing or folder's download to find the ID
// in.
func TestAnUploadIsItsBeginnersAlone(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "keep"), "keep")
	to := filepath.Join(site, "a.txt")
	up := d.begin(t, to, 3, 0, "").GetUpload()
	id := up.GetUploadId()

	listed, err := d.server.List(t.Context(), site, "", "")
	if err != nil || listed.GetRefusal() != nil {
		t.Fatalf("List = %v, %v", listed, err)
	}
	for _, e := range listed.GetListing().GetEntries() {
		if strings.HasPrefix(e.GetName(), uploadPrefix) {
			t.Errorf("the listing shows %s", e.GetName())
		}
	}
	_, data, _ := d.download(t, site)
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range z.File {
		if strings.HasPrefix(f.Name, uploadPrefix) {
			t.Errorf("the folder's download has %s", f.Name)
		}
	}

	const other = "usr_1/ses_2"
	if r, _ := d.server.BeginUpload(t.Context(), "", 0, 0, id, "", other); r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UPLOAD_GONE {
		t.Errorf("BeginUpload from another session = %v", r)
	}
	if w, _ := d.server.WriteUpload(t.Context(), id, other, 0, inParts([]byte("bad"), 10)); w.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UPLOAD_GONE {
		t.Errorf("WriteUpload from another session = %v", w)
	}
	if f, _ := d.server.FinishUpload(t.Context(), id, other); f.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UPLOAD_GONE {
		t.Errorf("FinishUpload from another session = %v", f)
	}
	if f, _ := d.server.CancelUpload(t.Context(), id, other); f.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UPLOAD_GONE {
		t.Errorf("CancelUpload from another session = %v", f)
	}
	if _, err := d.server.BeginUpload(t.Context(), filepath.Join(site, "b.txt"), 1, 0, "", "", ""); err == nil {
		t.Error("an upload began for no one")
	}

	// Its own session still has all of it.
	if w, _ := d.server.WriteUpload(t.Context(), id, me, 0, inParts([]byte("abc"), 10)); w.GetRefusal() != nil || w.GetUpload().GetReceivedBytes() != 3 {
		t.Fatalf("WriteUpload = %v", w)
	}
	if f, _ := d.server.FinishUpload(t.Context(), id, me); f.GetFinished() == nil {
		t.Fatalf("FinishUpload = %v", f)
	}
	if text, _ := os.ReadFile(to); string(text) != "abc" {
		t.Errorf("%s holds %q", to, text)
	}
}

// Cancelling an upload removes what it has, by what it is, and its note.
func TestCancellingAnUpload(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "keep"), "keep")
	up := d.begin(t, filepath.Join(site, "a.txt"), 6, 0, "").GetUpload()
	if w, _ := d.server.WriteUpload(t.Context(), up.GetUploadId(), me, 0, inParts([]byte("abc"), 10)); w.GetRefusal() != nil {
		t.Fatalf("WriteUpload = %v", w)
	}
	if f, err := d.server.CancelUpload(t.Context(), up.GetUploadId(), me); err != nil || f.GetRefusal() != nil {
		t.Fatalf("CancelUpload = %v, %v", f, err)
	}
	if left, _ := filepath.Glob(filepath.Join(site, uploadPrefix+"*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if r, _ := d.server.BeginUpload(t.Context(), "", 0, 0, up.GetUploadId(), "", me); r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UPLOAD_GONE {
		t.Errorf("after cancelling = %v", r)
	}

	// What took its file's place isn't its to remove.
	again := d.begin(t, filepath.Join(site, "b.txt"), 6, 0, "").GetUpload()
	part := filepath.Join(site, uploadPrefix+again.GetUploadId())
	// Moved aside rather than removed, so its inode isn't free for the new one.
	if err := os.Rename(part, filepath.Join(site, "aside")); err != nil {
		t.Fatal(err)
	}
	write(t, part, "someone else's")
	if f, _ := d.server.CancelUpload(t.Context(), again.GetUploadId(), me); f.GetRefusal() == nil {
		t.Errorf("CancelUpload over another file = %v", f)
	}
	if text, _ := os.ReadFile(part); string(text) != "someone else's" {
		t.Errorf("%s holds %q", part, text)
	}
}

// An upload not finished within a day goes, by what it is.
func TestAnUnfinishedUploadGoesAfterADay(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "keep"), "keep")
	up := d.begin(t, filepath.Join(site, "a.txt"), 3, 0, "").GetUpload()
	d.server.pruneUploads(t.Context())
	if left, _ := filepath.Glob(filepath.Join(site, uploadPrefix+"*")); len(left) != 1 {
		t.Fatalf("pruned too soon: %v", left)
	}
	d.server.now = func() time.Time { return time.Now().Add(keepUploads + time.Hour) }
	d.server.pruneUploads(t.Context())
	if left, _ := filepath.Glob(filepath.Join(site, uploadPrefix+"*")); len(left) != 0 {
		t.Errorf("kept past a day: %v", left)
	}
	if again, _ := d.server.BeginUpload(t.Context(), "", 0, 0, up.GetUploadId(), "", me); again.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UPLOAD_GONE {
		t.Errorf("= %v", again)
	}
}

func zipOf(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, text := range entries {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(text))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func tarGzOf(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	w := tar.NewWriter(z)
	for name, text := range entries {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(text)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(text))
	}
	if err := w.WriteHeader(&tar.Header{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	_ = z.Close()
	return b.Bytes()
}

// An archive unpacks into a new folder beside it, and only into that.
func TestUnpackingAnArchive(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "keep"), "keep")
	entries := map[string]string{"index.html": "<h1>hi</h1>", "assets/app.css": "body{}", "./assets/img/logo.svg": "<svg/>"}

	f := d.upload(t, filepath.Join(site, "theme.zip"), zipOf(t, entries), dockwellv1.Unpack_UNPACK_ZIP).GetFinished()
	if f.GetUnpackedTo() != filepath.Join(site, "theme") || f.GetUnpackedEntries() != 3 {
		t.Fatalf("unpacked %v", f)
	}
	if got, _ := os.ReadFile(filepath.Join(site, "theme", "assets", "img", "logo.svg")); string(got) != "<svg/>" {
		t.Errorf("it holds %q", got)
	}
	g := d.upload(t, filepath.Join(site, "release.tar.gz"), tarGzOf(t, entries), dockwellv1.Unpack_UNPACK_TAR_GZ).GetFinished()
	if g.GetUnpackedTo() != filepath.Join(site, "release") {
		t.Fatalf("unpacked %v", g)
	}
	if there(filepath.Join(site, "release", "link")) {
		t.Error("a link was unpacked")
	}
	// What's there isn't replaced: the archive is uploaded, and isn't unpacked.
	write(t, filepath.Join(site, "theme", "index.html"), "mine")
	again := d.upload(t, filepath.Join(site, "theme.tgz"), tarGzOf(t, map[string]string{"index.html": "theirs"}), dockwellv1.Unpack_UNPACK_TAR_GZ)
	if again.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UNPACK_REFUSED || again.GetFinished() == nil {
		t.Errorf("into a folder that's there: %v", again)
	}
	if got, _ := os.ReadFile(filepath.Join(site, "theme", "index.html")); string(got) != "mine" {
		t.Errorf("what was there holds %q", got)
	}
	// A link isn't made, so a file named through it stays inside.
	outside := filepath.Join(d.root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	var through bytes.Buffer
	gz := gzip.NewWriter(&through)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "out", Linkname: outside, Typeflag: tar.TypeSymlink})
	_ = tw.WriteHeader(&tar.Header{Name: "out/x", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	if got := d.upload(t, filepath.Join(site, "through.tar.gz"), through.Bytes(), dockwellv1.Unpack_UNPACK_TAR_GZ); got.GetRefusal() != nil {
		t.Fatalf("through a link: %v", got)
	}
	if there(filepath.Join(outside, "x")) || !there(filepath.Join(site, "through", "out", "x")) {
		t.Error("a file went through a link")
	}
	// Too deep, or too many entries, stops it.
	d.server.unpackLimit = 3
	for name, entries := range map[string]map[string]string{
		"many.zip": {"a": "1", "b": "2", "c": "3", "d": "4"},
		"deep.zip": {strings.Repeat("d/", maxUnpackDepth) + "f": "x"},
	} {
		got := d.upload(t, filepath.Join(site, name), zipOf(t, entries), dockwellv1.Unpack_UNPACK_ZIP)
		if got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UNPACK_REFUSED || there(filepath.Join(site, unpackedName(name))) {
			t.Errorf("%s: %v", name, got)
		}
	}
	d.server.unpackLimit = 0
	for name, why := range map[string]map[string]string{
		"out.zip":    {"../escaped.txt": "x"},
		"up.zip":     {"a/../../escaped.txt": "x"},
		"keys.zip":   {"home/.ssh/authorized_keys": "ssh-ed25519 AAAA"},
		"sudo.zip":   {"conf/sudoers.local": "ALL ALL=(ALL) NOPASSWD: ALL"},
		"pgpass.zip": {"conf/.pgpass": "*:*:*:*:pw"},
	} {
		got := d.upload(t, filepath.Join(site, name), zipOf(t, why), dockwellv1.Unpack_UNPACK_ZIP)
		if got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UNPACK_REFUSED || got.GetFinished() == nil {
			t.Errorf("%s: %v", name, got)
		}
		if there(filepath.Join(site, unpackedName(name))) {
			t.Errorf("%s left %s", name, unpackedName(name))
		}
	}
	if there(filepath.Join(d.root, "escaped.txt")) || there(filepath.Join(site, "escaped.txt")) {
		t.Error("an entry left its folder")
	}
	if got := d.upload(t, filepath.Join(site, "broken.zip"), []byte("not a zip"), dockwellv1.Unpack_UNPACK_ZIP); got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UNPACK_REFUSED {
		t.Errorf("what isn't a zip: %v", got)
	}
}

// download collects what a download sends, and checks each part is a part.
func (d *fakeDisk) download(t *testing.T, p string) ([]*agentv1.SendFileRequest, []byte, *agentv1.DownloadFileResult) {
	t.Helper()
	var sent []*agentv1.SendFileRequest
	var data []byte
	out, err := d.server.Download(t.Context(), p, func(m *agentv1.SendFileRequest) error {
		if len(m.GetData()) > FrameBytes {
			t.Fatalf("a part of %d bytes", len(m.GetData()))
		}
		sent = append(sent, m)
		data = append(data, m.GetData()...)
		return nil
	})
	if err != nil {
		t.Fatalf("Download(%s): %v", p, err)
	}
	return sent, data, out
}

func TestDownloadingAFile(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	big := bytes.Repeat([]byte("abcdefghij"), 300_000)
	write(t, filepath.Join(site, "big.bin"), string(big))
	link(t, filepath.Join(site, "big.bin"), filepath.Join(site, "latest"))

	sent, data, out := d.download(t, filepath.Join(site, "latest"))
	if out.GetRefusal() != nil || !bytes.Equal(data, big) || out.GetSentBytes() != uint64(len(big)) {
		t.Fatalf("= %v, %d bytes", out.GetRefusal(), len(data))
	}
	if first := sent[0]; first.GetName() != "big.bin" || first.GetPath() != filepath.Join(site, "big.bin") || first.GetSizeBytes() != uint64(len(big)) || len(first.GetData()) != 0 {
		t.Errorf("the first part says %v", first)
	}
	// What a read refuses, a download does.
	write(t, filepath.Join(site, ".pgpass"), "*:*:*:*:pw")
	write(t, filepath.Join(site, "key.pem"), "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n")
	for _, p := range []string{filepath.Join(site, ".pgpass"), filepath.Join(site, "key.pem"), filepath.Join(d.data, "control", "key")} {
		if sent, _, out := d.download(t, p); out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED || len(sent) != 0 {
			t.Errorf("%s: %v", p, out)
		}
	}
	// A key past the first MiB stops it before the key goes.
	hidden := append(make([]byte, 2<<20), []byte("\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAwJh2\n")...)
	write(t, filepath.Join(site, "backup.img"), string(hidden))
	_, data, out = d.download(t, filepath.Join(site, "backup.img"))
	if out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED || bytes.Contains(data, []byte("MIIEowIBAAKCAQEAwJh2")) {
		t.Errorf("a key deep in a file went out: %v", out)
	}
	// A program that only names one goes.
	write(t, filepath.Join(site, "ssh-keygen"), "\x00\x01-----BEGIN %s PRIVATE KEY-----\x00usage")
	if _, _, out := d.download(t, filepath.Join(site, "ssh-keygen")); out.GetRefusal() != nil {
		t.Errorf("a program naming a key: %v", out)
	}
}

// A folder goes as a zip archive, without what a read refuses, links or another
// filesystem.
func TestDownloadingAFolder(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "index.html"), "<h1>hi</h1>")
	write(t, filepath.Join(site, "assets", "app.css"), "body{}")
	write(t, filepath.Join(site, ".pgpass"), "*:*:*:*:pw")
	write(t, filepath.Join(site, "id_rsa"), "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n")
	link(t, filepath.Join(d.data, "control", "key"), filepath.Join(site, "to-key"))

	sent, data, out := d.download(t, site)
	if out.GetRefusal() != nil || !sent[0].GetArchive() || sent[0].GetName() != "site.zip" {
		t.Fatalf("= %v, %v", out, sent[0])
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range z.File {
		names = append(names, f.Name)
	}
	got := strings.Join(names, ",")
	for _, want := range []string{"index.html", "assets/", "assets/app.css"} {
		if !strings.Contains(got, want) {
			t.Errorf("the archive has %s, not %s", got, want)
		}
	}
	for _, not := range []string{".pgpass", "id_rsa", "to-key"} {
		if strings.Contains(got, not) {
			t.Errorf("the archive has %s", not)
		}
	}
	r, err := z.Open("assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := io.ReadAll(r); string(text) != "body{}" {
		t.Errorf("app.css holds %q", text)
	}
	// The data directory itself isn't a folder to download.
	if _, _, out := d.download(t, d.data); out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("Dockwell's data: %v", out)
	}
}

// A folder's archive takes what it checked: a FIFO swapped in for a file between
// the check and the open is left out. It's a race, so this runs it long enough to
// have caught it before (the review saw it within a hundred).
func TestAFolderArchiveTakesWhatItChecked(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "f"), "plain")
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_ = os.WriteFile(filepath.Join(site, ".plain"), []byte("plain"), 0o644)
			_ = os.Rename(filepath.Join(site, ".plain"), filepath.Join(site, "f"))
			_ = unix.Mkfifo(filepath.Join(site, ".fifo"), 0o644)
			_ = os.Rename(filepath.Join(site, ".fifo"), filepath.Join(site, "f"))
			if fd, err := unix.Open(filepath.Join(site, "f"), unix.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
				_, _ = unix.Write(fd, []byte("FROM-A-FIFO"))
				_ = unix.Close(fd)
			}
		}
	}()
	defer func() { stop.Store(true); <-done }()
	for i := range 500 {
		_, data, out := d.download(t, site)
		if out.GetRefusal() != nil {
			continue
		}
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			continue
		}
		for _, f := range z.File {
			r, err := f.Open()
			if err != nil {
				continue
			}
			text, _ := io.ReadAll(r)
			if bytes.Contains(text, []byte("FROM-A-FIFO")) {
				t.Fatalf("after %d downloads, the archive holds what a FIFO gave", i+1)
			}
		}
	}
}

// A download whose reader goes stops.
func TestADownloadStopsWhenItsReaderGoes(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	write(t, filepath.Join(d.root, "big.bin"), strings.Repeat("x", 4*FrameBytes))
	gone := errors.New("the reader went")
	n := 0
	_, err := d.server.Download(t.Context(), filepath.Join(d.root, "big.bin"), func(*agentv1.SendFileRequest) error {
		n++
		if n > 2 {
			return gone
		}
		return nil
	})
	if !errors.Is(err, gone) || n != 3 {
		t.Errorf("= %v after %d parts", err, n)
	}
}

// A size reads as the panel writes it.
func TestASizeReadsAsThePanelWritesIt(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 512: "512 B", 1536: "1.5 KB", 1023 << 10: "1023 KB", 1<<20 - 1: "1 MB", 2_200_000_000: "2 GB", MaxUploadBytes: "16 GB", MaxUploadBytes + 1: "16 GB"} {
		if got := sizeOf(n); got != want {
			t.Errorf("sizeOf(%d) = %q, want %q", n, got, want)
		}
	}
}

// A transfer that waits too long for a place says the server is busy, rather than
// leave someone looking at nothing moving, and one that's freed is taken again.
func TestABusyServerSaysSo(t *testing.T) {
	d := newDisk(t)
	d.server.transferWait = 50 * time.Millisecond
	var held []func()
	for range transfers {
		release, err := d.server.TakeTransfer(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
	}
	if _, err := d.server.TakeTransfer(t.Context()); !errors.Is(err, ErrBusy) {
		t.Fatalf("a fourth = %v", err)
	}
	if Busy().GetKind() != agentv1.FileRefusal_KIND_BUSY {
		t.Error("Busy() isn't KIND_BUSY")
	}
	held[0]()
	release, err := d.server.TakeTransfer(t.Context())
	if err != nil {
		t.Fatalf("once one ended = %v", err)
	}
	release()
	for _, r := range held[1:] {
		r()
	}
}

// An unpack stops short of filling the disk, and what it made goes. A write to an
// upload that fills it keeps what it has, and says so.
func TestUnpackingStopsShortOfAFullDisk(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "keep"), "keep")
	free := uint64(100 << 30)
	d.server.statfs = func(_ int, st *unix.Statfs_t) error {
		*st = unix.Statfs_t{Bsize: 4096, Blocks: 1000 << 30 / 4096, Bavail: free / 4096}
		return nil
	}
	if f := d.upload(t, filepath.Join(site, "roomy.zip"), zipOf(t, map[string]string{"a": "1"}), dockwellv1.Unpack_UNPACK_ZIP); f.GetRefusal() != nil {
		t.Fatalf("with room: %v", f)
	}
	free = 512 << 20
	got := d.upload(t, filepath.Join(site, "full.zip"), zipOf(t, map[string]string{"a": "1"}), dockwellv1.Unpack_UNPACK_ZIP)
	if got.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UNPACK_REFUSED || !strings.Contains(got.GetRefusal().GetMessage(), "stopped short of filling it") {
		t.Errorf("on a full disk: %v", got)
	}
	if there(filepath.Join(site, "full")) {
		t.Error("what it made on a full disk stayed")
	}
}

// Past the first MiB, a download stops where a read would have refused: an
// encrypted key in the old PEM form, whose headers come before its base64, and a
// line of a password file, even one split across two parts.
func TestADownloadStopsAtWhatAReadRefusesAnywhere(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	filler := strings.Repeat("insert into t values (1);\n", (2<<20)/26)
	for name, tail := range map[string]string{
		"pem.sql":  "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,0123456789ABCDEF0123456789ABCDEF\n\nc2VjcmV0c2VjcmV0c2VjcmV0\n-----END RSA PRIVATE KEY-----\n",
		"hash.sql": "root:$6$saltsalt$c2VjcmV0c2VjcmV0c2VjcmV0:19000:0:99999:7:::\n",
	} {
		write(t, filepath.Join(d.root, name), filler+tail)
		_, data, out := d.download(t, filepath.Join(d.root, name))
		if out.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
			t.Errorf("%s: %v", name, out)
		}
		if bytes.Contains(data, []byte("c2VjcmV0")) {
			t.Errorf("%s: what it holds went out", name)
		}
	}
	// A hash line that a part ends in the middle of stops before the rest of it.
	scan := &keyScan{}
	if r := scan.check("/x", []byte("ok\nroot:$")); r != nil {
		t.Fatalf("the start of a line = %v", r)
	}
	if r := scan.check("/x", []byte("6$salt$hash\n")); r == nil {
		t.Error("a hash line across two parts went out")
	}
	// What only mentions them goes.
	scan = &keyScan{}
	if r := scan.check("/x", []byte("price: $5$ off\nsee -----BEGIN RSA PRIVATE KEY----- in the docs\n")); r != nil {
		t.Errorf("a mention = %v", r)
	}
}
