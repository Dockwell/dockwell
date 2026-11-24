// SPDX-License-Identifier: AGPL-3.0-only

package files

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

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

func TestCompressAndExtractArchives(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	site := filepath.Join(d.root, "site")
	write(t, filepath.Join(site, "index.html"), "<h1>hello</h1>\n")
	write(t, filepath.Join(site, "assets", "app.js"), "console.log(1)\n")

	archive := filepath.Join(site, "bundle.zip")
	comp, err := d.server.Compress(t.Context(), []string{
		filepath.Join(site, "index.html"),
		filepath.Join(site, "assets"),
	}, archive, "")
	if err != nil || comp.GetRefusal() != nil {
		t.Fatalf("Compress = %v, %v", comp, err)
	}
	if comp.GetCompressed().GetPath() != archive || comp.GetCompressed().GetEntries() != 3 {
		t.Fatalf("compressed = %v", comp.GetCompressed())
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	got := zipNames(t, raw)
	if got["index.html"] != "<h1>hello</h1>\n" || got["assets/app.js"] != "console.log(1)\n" {
		t.Fatalf("archive contents = %v", got)
	}

	// Compressing over an existing file is refused.
	if dup, _ := d.server.Compress(t.Context(), []string{filepath.Join(site, "index.html")}, archive, ""); dup.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_EXISTS {
		t.Errorf("compress over existing = %v, want KIND_EXISTS", dup)
	}

	// Extract the created zip into site/bundle.
	ext, err := d.server.Extract(t.Context(), archive, "")
	if err != nil || ext.GetRefusal() != nil {
		t.Fatalf("Extract = %v, %v", ext, err)
	}
	if ext.GetExtracted().GetUnpackedTo() != filepath.Join(site, "bundle") || ext.GetExtracted().GetUnpackedEntries() != 3 {
		t.Fatalf("extracted = %v", ext.GetExtracted())
	}
	if b, _ := os.ReadFile(filepath.Join(site, "bundle", "index.html")); string(b) != "<h1>hello</h1>\n" {
		t.Errorf("extracted index.html = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(site, "bundle", "assets", "app.js")); string(b) != "console.log(1)\n" {
		t.Errorf("extracted assets/app.js = %q", b)
	}

	// Extracting when the destination folder already exists is refused with KIND_EXISTS.
	if again, _ := d.server.Extract(t.Context(), archive, ""); again.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_EXISTS {
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
	}, archive, "")
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

func TestCompressAndExtractWayBackInNeedStepUp(t *testing.T) {
	d := newDisk(t)
	d.noAccounts()
	sys := filepath.Join(d.root, "etc")
	d.server.systemPlaces = func() []string { return []string{sys} }
	write(t, filepath.Join(sys, "app.conf"), "ok = 1\n")

	archive := filepath.Join(sys, "backup.zip")
	if r, _ := d.server.Compress(t.Context(), []string{filepath.Join(sys, "app.conf")}, archive, ""); r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP {
		t.Fatalf("Compress in system path without step-up = %v", r)
	}
	comp, err := d.server.Compress(t.Context(), []string{filepath.Join(sys, "app.conf")}, archive, archive)
	if err != nil || comp.GetRefusal() != nil || !comp.GetCompressed().GetWayBackIn() {
		t.Fatalf("Compress in system path with step-up = %v, %v", comp, err)
	}

	dst := filepath.Join(sys, "backup")
	r, _ := d.server.Extract(t.Context(), archive, "")
	if r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_NEEDS_STEP_UP || r.GetRefusal().GetPath() != dst {
		t.Fatalf("Extract in system path without step-up = %v", r)
	}
	ext, err := d.server.Extract(t.Context(), archive, dst)
	if err != nil || ext.GetRefusal() != nil || !ext.GetExtracted().GetWayBackIn() {
		t.Fatalf("Extract in system path with step-up = %v, %v", ext, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "app.conf")); string(b) != "ok = 1\n" {
		t.Errorf("extracted app.conf = %q", b)
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
	r, err := d.server.Extract(t.Context(), bad, "")
	if err != nil || r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_UNPACK_REFUSED {
		t.Fatalf("Extract zip-slip = %v, %v, want KIND_UNPACK_REFUSED", r, err)
	}
	if there(filepath.Join(site, "slip")) || there(filepath.Join(site, "escape.txt")) {
		t.Error("zip-slip left files behind")
	}

	// Hard-linked archive
	good := filepath.Join(site, "ok.zip")
	if err := os.WriteFile(good, makeTestZip(t, map[string]string{"a.txt": "hi"}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(good, filepath.Join(site, "ok2.zip")); err != nil {
		t.Fatal(err)
	}
	if r, _ := d.server.Extract(t.Context(), good, ""); r.GetRefusal().GetKind() != agentv1.FileRefusal_KIND_REFUSED {
		t.Errorf("Extract hard-linked archive = %v, want KIND_REFUSED", r)
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
