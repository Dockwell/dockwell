// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/serve"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// restAnswer is what a REST call answered, its body read and closed.
type restAnswer struct {
	StatusCode int
	Header     http.Header
}

// rawREST makes a REST call with a body of bytes, as a browser sends a file, and
// returns what it answered and all of its body.
func rawREST(t *testing.T, b *browser, verb, path string, body []byte, extra map[string]string) (restAnswer, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, verb, b.origin+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	req.Header.Set("Origin", b.origin)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return restAnswer{StatusCode: res.StatusCode, Header: res.Header}, out
}

// A person uploads a file in parts, picks it up again after a drop, puts it in
// place, and downloads it and its folder, each on the timeline, and each streamed:
// a part and the download are each well past what Vanguard would hold.
func TestAPersonUploadsAndDownloads(t *testing.T) { storetest.Each(t, testAPersonUploadsAndDownloads) }

func testAPersonUploadsAndDownloads(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(dir, "site")
	writeFile(t, filepath.Join(site, "index.html"), "<h1>hi</h1>")
	c := fileClient(b, b.origin)
	data := bytes.Repeat([]byte("dockwell "), 20<<20/9)
	to := filepath.Join(site, "backup.sql")

	began, err := c.BeginUpload(t.Context(), connect.NewRequest(&dockwellv1.BeginUploadRequest{ServerId: id, Path: to, SizeBytes: uint64(len(data))}))
	if err != nil || began.Msg.GetUpload().GetMaxBytes() == 0 {
		t.Fatalf("BeginUpload = %v, %v", began, err)
	}
	upload := began.Msg.GetUpload().GetUploadId()
	put := func(offset int, part []byte) (restAnswer, []byte) {
		return rawREST(t, b, http.MethodPut, "/v1/servers/"+id+"/uploads/"+upload+"?offset="+strconv.Itoa(offset), part, nil)
	}
	half := 2 << 20
	// A PUT that works says where the upload got.
	var got struct {
		Upload struct {
			ReceivedBytes string `json:"receivedBytes"`
		} `json:"upload"`
	}
	if res, body := put(0, data[:half]); res.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Upload.ReceivedBytes != strconv.Itoa(half) {
		t.Fatalf("PUT the first part = %d %s", res.StatusCode, body)
	}
	state, err := c.BeginUpload(t.Context(), connect.NewRequest(&dockwellv1.BeginUploadRequest{ServerId: id, UploadId: upload}))
	if err != nil || state.Msg.GetUpload().GetReceivedBytes() != uint64(half) {
		t.Fatalf("where it got = %v, %v", state, err)
	}
	if res, body := put(0, data); res.StatusCode != http.StatusBadRequest || res.Header.Get(authapi.ReasonKey) != "upload_offset" || !strings.Contains(string(body), "Send from "+strconv.Itoa(half)) {
		t.Errorf("a PUT from the start = %d %s", res.StatusCode, body)
	}
	if res, body := put(half, data[half:]); res.StatusCode != http.StatusOK || len(data)-half <= serve.MaxMessageBytes {
		t.Fatalf("PUT the rest = %d %s", res.StatusCode, body)
	}
	finished, err := c.FinishUpload(t.Context(), connect.NewRequest(&dockwellv1.FinishUploadRequest{ServerId: id, UploadId: upload}))
	if err != nil || finished.Msg.GetPath() != to {
		t.Fatalf("FinishUpload = %v, %v", finished, err)
	}
	if got, _ := os.ReadFile(to); !bytes.Equal(got, data) {
		t.Fatal("what's on the disk isn't what was sent")
	}
	if e := waitForFileEvent(t, b, id, "file.upload", to); e.GetSubject().GetSizeBytes() != uint64(len(data)) {
		t.Errorf("on the timeline: %v", e)
	}

	res, body := rawREST(t, b, http.MethodGet, "/v1/servers/"+id+"/files:download?path="+to, nil, nil)
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, data) || res.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("GET a download = %d, %d bytes, %v", res.StatusCode, len(body), res.Header)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="backup.sql"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	waitForFileEvent(t, b, id, "file.download", to)
	res, body = rawREST(t, b, http.MethodGet, "/v1/servers/"+id+"/files:download?path="+site, nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Disposition"), "site.zip") {
		t.Fatalf("GET a folder = %d %v", res.StatusCode, res.Header)
	}
	z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || len(z.File) != 2 {
		t.Fatalf("the folder's archive: %v, %v", z, err)
	}
	// What a read refuses, a download does, on record.
	res, body = rawREST(t, b, http.MethodGet, "/v1/servers/"+id+"/files:download?path=/proc/self/environ", nil, nil)
	if res.StatusCode != http.StatusForbidden || res.Header.Get(authapi.ReasonKey) != "file_refused" || !strings.Contains(string(body), "so Dockwell doesn't open it") {
		t.Errorf("GET /proc = %d %s", res.StatusCode, body)
	}
	waitForFileEvent(t, b, id, "file.refuse", "/proc/self/environ")

	// An archive unpacks into a folder beside it.
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, _ := zw.Create("theme/style.css")
	_, _ = w.Write([]byte("body{}"))
	_ = zw.Close()
	theme := filepath.Join(site, "theme.zip")
	began, err = c.BeginUpload(t.Context(), connect.NewRequest(&dockwellv1.BeginUploadRequest{ServerId: id, Path: theme, SizeBytes: uint64(archive.Len()), Unpack: dockwellv1.Unpack_UNPACK_ZIP}))
	if err != nil {
		t.Fatal(err)
	}
	upload = began.Msg.GetUpload().GetUploadId()
	if res, body := put(0, archive.Bytes()); res.StatusCode != http.StatusOK {
		t.Fatalf("PUT an archive = %d %s", res.StatusCode, body)
	}
	finished, err = c.FinishUpload(t.Context(), connect.NewRequest(&dockwellv1.FinishUploadRequest{ServerId: id, UploadId: upload}))
	if err != nil || finished.Msg.GetUnpackedTo() != filepath.Join(site, "theme") {
		t.Fatalf("FinishUpload = %v, %v", finished, err)
	}
	if got, _ := os.ReadFile(filepath.Join(site, "theme", "theme", "style.css")); string(got) != "body{}" {
		t.Errorf("unpacked, it holds %q", got)
	}
}

// No token uploads or downloads, whatever its scope.
func TestNoTokenUploadsOrDownloads(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	config := filepath.Join(t.TempDir(), "config")
	writeFile(t, config, "s3cret")
	for _, scope := range []dockwellv1.TokenScope{dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE} {
		bearer := map[string]string{"Authorization": "Bearer " + createToken(t, b, scope)}
		upload := strings.Repeat("A", 26)
		for _, r := range []struct {
			verb, path string
			body       []byte
		}{
			{http.MethodPost, "/v1/servers/" + id + "/uploads", []byte(`{"path":"` + config + `.new","sizeBytes":"3"}`)},
			{http.MethodPut, "/v1/servers/" + id + "/uploads/" + upload + "?offset=0", []byte("abc")},
			{http.MethodPost, "/v1/servers/" + id + "/uploads/" + upload + ":finish", []byte(`{}`)},
			{http.MethodGet, "/v1/servers/" + id + "/files:download?path=" + config, nil},
		} {
			extra := bearer
			if r.verb == http.MethodPost {
				extra = map[string]string{"Authorization": bearer["Authorization"], "Content-Type": "application/json"}
			}
			res, body := rawREST(t, b, r.verb, r.path, r.body, extra)
			if res.StatusCode != http.StatusForbidden || res.Header.Get(authapi.ReasonKey) != authapi.ReasonNeedsSession || strings.Contains(string(body), "s3cret") {
				t.Errorf("%v: %s %s = %d %s %s", scope, r.verb, r.path, res.StatusCode, res.Header.Get(authapi.ReasonKey), body)
			}
		}
	}
	if got := fileEvents(t, f); len(got) != 0 {
		t.Errorf("a refused token got as far as the audit log: %v", got)
	}
}

// What can't go on record doesn't go out, as with a read: the audit log is stopped
// from taking the download's event, so not a byte of the file is sent.
func TestADownloadThatCantGoOnRecordSendsNothing(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	config := filepath.Join(t.TempDir(), "config")
	writeFile(t, config, "s3cret")
	f.execSQL(t, `CREATE TRIGGER keep_out BEFORE INSERT ON audit_events WHEN NEW.action = 'file.download' BEGIN SELECT RAISE(ABORT, 'no room'); END`)
	res, body := rawREST(t, b, http.MethodGet, "/v1/servers/"+id+"/files:download?path="+config, nil, nil)
	if res.StatusCode == http.StatusOK || strings.Contains(string(body), "s3cret") || res.Header.Get("Content-Disposition") != "" {
		t.Errorf("GET with nothing on record = %d %v %s", res.StatusCode, res.Header, body)
	}
}

// A download that stops partway, at a private key, cuts the connection, so a browser
// shows it failed rather than save what came as the whole, and the key stays.
func TestADownloadCutShortFails(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dump := filepath.Join(t.TempDir(), "dump.sql")
	writeFile(t, dump, strings.Repeat("insert into t values (1);\n", 200_000)+"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAwJh2\n")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, b.origin+"/v1/servers/"+id+"/files:download?path="+dump, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || err == nil {
		t.Errorf("a download cut short = %d, and reading it all = %v", res.StatusCode, err)
	}
	if len(data) == 0 || bytes.Contains(data, []byte("PRIVATE KEY")) {
		t.Errorf("%d bytes came, key and all: %v", len(data), bytes.Contains(data, []byte("PRIVATE KEY")))
	}
}

// An upload is its session's. Another session, signed in with a link and no passkey
// of its own, finds no way-in upload's ID in a listing, and with the ID can't see,
// fill, finish or stop it. The session that began it can stop it.
func TestAnUploadIsItsSessions(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	owner := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, ".ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(keys), 0o700); err != nil {
		t.Fatal(err)
	}
	mine := []byte("ssh-ed25519 AAAAowner owner@laptop\n")
	c := fileClient(owner, owner.origin)
	proof := stepUpFor(t, owner, dockwellv1.StepUpAction_STEP_UP_ACTION_MAKE_WAY_BACK_IN, id+":"+keys)
	began, err := c.BeginUpload(t.Context(), connect.NewRequest(&dockwellv1.BeginUploadRequest{ServerId: id, Path: keys, SizeBytes: uint64(len(mine)), Proof: proof}))
	if err != nil {
		t.Fatal(err)
	}
	upload := began.Msg.GetUpload().GetUploadId()

	link, _, err := f.auth.CreateSignInLink(t.Context(), "", "cli:root@hz-1")
	if err != nil {
		t.Fatal(err)
	}
	other := newBrowser(t, owner.origin, f.api, nil)
	if _, err := other.authClient().SignInWithLink(t.Context(), connect.NewRequest(&dockwellv1.SignInWithLinkRequest{Token: link.Token})); err != nil {
		t.Fatal(err)
	}
	oc := fileClient(other, other.origin)
	listed, err := oc.ListFiles(t.Context(), connect.NewRequest(&dockwellv1.ListFilesRequest{ServerId: id, Path: filepath.Dir(keys)}))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range listed.Msg.GetEntries() {
		if strings.HasPrefix(e.GetName(), ".dockwell-upload-") {
			t.Errorf("the listing shows %s", e.GetName())
		}
	}
	if _, err := oc.BeginUpload(t.Context(), connect.NewRequest(&dockwellv1.BeginUploadRequest{ServerId: id, UploadId: upload})); reasonOf(err) != "upload_gone" {
		t.Errorf("where another session's upload got = %v", err)
	}
	theirs := []byte("ssh-ed25519 AAAAother other@elsewhere\n")[:len(mine)]
	if res, body := rawREST(t, other, http.MethodPut, "/v1/servers/"+id+"/uploads/"+upload+"?offset=0", theirs, nil); res.StatusCode != http.StatusNotFound || res.Header.Get(authapi.ReasonKey) != "upload_gone" {
		t.Errorf("another session's PUT = %d %s", res.StatusCode, body)
	}
	if _, err := oc.FinishUpload(t.Context(), connect.NewRequest(&dockwellv1.FinishUploadRequest{ServerId: id, UploadId: upload})); reasonOf(err) != "upload_gone" {
		t.Errorf("another session's FinishUpload = %v", err)
	}
	if res, body := rawREST(t, other, http.MethodDelete, "/v1/servers/"+id+"/uploads/"+upload, nil, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("another session's DELETE = %d %s", res.StatusCode, body)
	}
	if _, err := os.Stat(keys); !os.IsNotExist(err) {
		t.Fatalf("authorized_keys is there: %v", err)
	}

	// Its own session fills it, or stops it.
	if res, body := rawREST(t, owner, http.MethodPut, "/v1/servers/"+id+"/uploads/"+upload+"?offset=0", mine[:4], nil); res.StatusCode != http.StatusOK {
		t.Fatalf("the owner's PUT = %d %s", res.StatusCode, body)
	}
	if _, err := c.CancelUpload(t.Context(), connect.NewRequest(&dockwellv1.CancelUploadRequest{ServerId: id, UploadId: upload})); err != nil {
		t.Fatalf("CancelUpload = %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(keys), ".dockwell-upload-*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if _, err := c.BeginUpload(t.Context(), connect.NewRequest(&dockwellv1.BeginUploadRequest{ServerId: id, UploadId: upload})); reasonOf(err) != "upload_gone" {
		t.Errorf("after stopping it = %v", err)
	}
}

// A transfer that stops moving lets go of its place: three downloads whose readers
// stopped, and an upload whose sender did, don't keep a fourth from starting. A
// HEAD isn't a download, so nothing goes on record for it.
func TestAStalledTransferLetsGo(t *testing.T) {
	was := transferStall
	transferStall = 300 * time.Millisecond
	t.Cleanup(func() { transferStall = was })
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	id := f.addServer(t, false).id.ServerID
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	writeFile(t, big, strings.Repeat("dockwell ", 32<<20/9))
	small := filepath.Join(dir, "small.txt")
	writeFile(t, small, "small")

	get := func(path string) *http.Response {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, b.origin+"/v1/servers/"+id+"/files:download?path="+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		res, err := b.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// Three that stop reading after the first bytes.
	for range 3 {
		res := get(big)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("a large download = %d", res.StatusCode)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
	}
	// And an upload that sends a little, then nothing.
	c := fileClient(b, b.origin)
	began, err := c.BeginUpload(t.Context(), connect.NewRequest(&dockwellv1.BeginUploadRequest{ServerId: id, Path: filepath.Join(dir, "up.bin"), SizeBytes: 1 << 20}))
	if err != nil {
		t.Fatal(err)
	}
	body, send := io.Pipe()
	defer send.Close()
	put, err := http.NewRequestWithContext(t.Context(), http.MethodPut, b.origin+"/v1/servers/"+id+"/uploads/"+began.Msg.GetUpload().GetUploadId()+"?offset=0", body)
	if err != nil {
		t.Fatal(err)
	}
	put.Header.Set("Content-Type", "application/octet-stream")
	put.Header.Set("Origin", b.origin)
	put.Header.Set("Sec-Fetch-Site", "same-origin")
	answered := make(chan int, 1)
	go func() {
		res, err := b.client.Do(put)
		if err != nil {
			answered <- 0
			return
		}
		_ = res.Body.Close()
		answered <- res.StatusCode
	}()
	if _, err := send.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("an upload whose sender stopped is still open")
	}

	start := time.Now()
	res, data := rawREST(t, b, http.MethodGet, "/v1/servers/"+id+"/files:download?path="+small, nil, nil)
	if res.StatusCode != http.StatusOK || string(data) != "small" || time.Since(start) > 10*time.Second {
		t.Errorf("a fourth download, after %v = %d %s", time.Since(start), res.StatusCode, data)
	}

	head, _ := rawREST(t, b, http.MethodHead, "/v1/servers/"+id+"/files:download?path="+small, nil, nil)
	if head.StatusCode != http.StatusMethodNotAllowed || head.Header.Get("Allow") != http.MethodGet {
		t.Errorf("HEAD = %d %v", head.StatusCode, head.Header)
	}
	waitForFileEvent(t, b, id, "file.download", small)
	n := 0
	for _, e := range timelineFiles(t, b, &dockwellv1.ListEventsRequest{ServerId: id}) {
		if e.GetAction() == "file.download" && e.GetSubject().GetPath() == small {
			n++
		}
	}
	if n != 1 {
		t.Errorf("small.txt is on record as downloaded %d times", n)
	}
}
